// Package tool runs the agent's tools (describe_dataset, run_sql, run_python)
// on behalf of the Python agent service, and records each call.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/storage"
	"dataanalyst/api/internal/platform/telemetry"
	"dataanalyst/api/internal/run"
	"dataanalyst/api/internal/sandbox"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxToolOutput = 8000 // bytes of tool output the model sees
	maxSQLRows    = 50
)

var (
	ErrRunNotFound   = errors.New("run not found")
	ErrRunNotRunning = errors.New("run is not running")
)

// Sandbox is the part of sandbox.Manager tools use.
type Sandbox interface {
	Exec(ctx context.Context, sessionID string, req sandbox.Request) (sandbox.Result, error)
}

// Datasets describes a session's tables (implemented by the dataset domain).
type Datasets interface {
	Describe(ctx context.Context, sessionID, only string) ([]agent.Table, error)
}

// Charts keeps the PNGs a Python call produced (implemented by the chart domain).
type Charts interface {
	Keep(ctx context.Context, sessionID, runID uuid.UUID, files []string) ([]string, error)
}

// Runs looks up and records runs (implemented by the run domain).
type Runs interface {
	Get(ctx context.Context, id uuid.UUID) (db.AgentRun, error)
	RecordToolCall(ctx context.Context, runID uuid.UUID, tool string, input json.RawMessage,
		resp agent.ToolResponse, took time.Duration) error
}

type Service struct {
	sandbox  Sandbox
	datasets Datasets
	charts   Charts
	runs     Runs
	layout   storage.Layout
	now      func() time.Time
}

func NewService(sb Sandbox, datasets Datasets, charts Charts, runs Runs, layout storage.Layout, now func() time.Time) *Service {
	return &Service{sandbox: sb, datasets: datasets, charts: charts, runs: runs, layout: layout, now: now}
}

// IsTool reports whether name is a tool the agent may call.
func IsTool(name string) bool {
	switch name {
	case agent.ToolDescribeDataset, agent.ToolRunSQL, agent.ToolRunPython:
		return true
	}
	return false
}

// RunFor checks the call belongs to a running run of the stated session.
func (s *Service) RunFor(ctx context.Context, req agent.ToolRequest) (db.AgentRun, error) {
	runID, err1 := uuid.Parse(req.RunID)
	sessionID, err2 := uuid.Parse(req.SessionID)
	if err1 != nil || err2 != nil {
		return db.AgentRun{}, ErrRunNotFound
	}
	r, err := s.runs.Get(ctx, runID)
	if errors.Is(err, run.ErrNotFound) || (err == nil && r.SessionID != sessionID) {
		return db.AgentRun{}, ErrRunNotFound
	}
	if err != nil {
		return db.AgentRun{}, err
	}
	if r.Status != run.StatusRunning {
		return db.AgentRun{}, ErrRunNotRunning
	}
	return r, nil
}

// Execute runs one tool call, records it, and returns what the model sees.
// Failures of the tool itself come back in the response, not as an error.
//
// The call is traced as a tool.<name> span. A Tool error (the agent's SQL or
// Python was wrong) leaves the span OK with tool.error set: the model reads
// it and retries. Only a Tool failure (the sandbox could not do its job)
// marks the span as an error.
func (s *Service) Execute(ctx context.Context, tool string, r db.AgentRun, input json.RawMessage) (agent.ToolResponse, error) {
	ctx, span := telemetry.Start(ctx, "tool."+tool, trace.WithAttributes(
		attribute.String(telemetry.AttrKind, "TOOL"),
		attribute.String("tool.name", tool),
		attribute.String("run.id", r.ID.String()),
		attribute.String("session.id", r.SessionID.String()),
	))
	defer span.End()
	telemetry.Payload(span, telemetry.AttrInput, string(input))

	start := s.now()
	resp, failed := s.dispatch(ctx, tool, r, input)
	resp.Output = truncate(resp.Output, maxToolOutput)
	if resp.ChartIDs == nil {
		resp.ChartIDs = []string{}
	}
	telemetry.Payload(span, telemetry.AttrOutput, resp.Output)
	if resp.Error != "" {
		telemetry.Payload(span, "tool.error", resp.Error)
		span.SetAttributes(attribute.Bool("tool.failed", failed))
	}
	if failed {
		telemetry.Fail(span, telemetry.Truncate(resp.Error, telemetry.MaxPayload))
	}
	err := s.runs.RecordToolCall(ctx, r.ID, tool, input, resp, s.now().Sub(start))
	if err != nil {
		span.RecordError(err)
	}
	return resp, err
}

// dispatch runs the tool. failed is true for a Tool failure: resp.Error then
// says what broke, but the agent's input was not to blame.
func (s *Service) dispatch(ctx context.Context, tool string, r db.AgentRun, input json.RawMessage) (resp agent.ToolResponse, failed bool) {
	var in struct {
		Table string `json:"table"`
		SQL   string `json:"sql"`
		Code  string `json:"code"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return agent.ToolResponse{Error: "tool input must be a JSON object"}, false
		}
	}
	sid := r.SessionID.String()
	switch tool {
	case agent.ToolDescribeDataset:
		tables, err := s.datasets.Describe(ctx, sid, in.Table)
		if err != nil {
			return agent.ToolResponse{Error: err.Error()}, false
		}
		return agent.ToolResponse{Output: formatTables(tables)}, false
	case agent.ToolRunSQL:
		if strings.TrimSpace(in.SQL) == "" {
			return agent.ToolResponse{Error: `run_sql needs a non-empty "sql" string`}, false
		}
		res, errResp := s.exec(ctx, sid, sandbox.Request{Mode: "sql", Code: in.SQL, MaxRows: maxSQLRows})
		if errResp != nil {
			return *errResp, true
		}
		return agent.ToolResponse{Output: formatSQL(res), Error: res.Error}, res.Failed()
	default: // run_python
		if strings.TrimSpace(in.Code) == "" {
			return agent.ToolResponse{Error: `run_python needs a non-empty "code" string`}, false
		}
		res, errResp := s.exec(ctx, sid, sandbox.Request{Mode: "python", Code: in.Code, MaxOutputChars: maxToolOutput})
		if errResp != nil {
			return *errResp, true
		}
		ids := s.keepCharts(ctx, r, res.Files)
		return agent.ToolResponse{Output: formatPython(res, ids), Error: res.Error, ChartIDs: ids}, res.Failed()
	}
}

func (s *Service) keepCharts(ctx context.Context, r db.AgentRun, files []string) []string {
	ctx, span := telemetry.Start(ctx, "chart.keep")
	defer span.End()
	ids, err := s.charts.Keep(ctx, r.SessionID, r.ID, files)
	span.SetAttributes(attribute.Int("chart.files", len(files)), attribute.Int("chart.count", len(ids)))
	if err != nil {
		slog.ErrorContext(ctx, "keep charts", "err", err)
		span.RecordError(err)
		telemetry.Fail(span, err.Error())
	}
	return ids
}

// exec runs a sandbox request with a clean out dir. Infrastructure failures
// come back as a response the agent can read (never an HTTP error); they are
// Tool failures.
func (s *Service) exec(ctx context.Context, sessionID string, req sandbox.Request) (sandbox.Result, *agent.ToolResponse) {
	s.layout.ClearOut(sessionID)
	res, err := s.sandbox.Exec(ctx, sessionID, req)
	switch {
	case errors.Is(err, sandbox.ErrAtCapacity):
		return res, &agent.ToolResponse{Error: "all sandboxes are busy right now; wait a moment and try again"}
	case err != nil:
		slog.ErrorContext(ctx, "sandbox exec", "session", sessionID, "err", err)
		return res, &agent.ToolResponse{Error: "the sandbox failed to run this call; try again"}
	}
	return res, nil
}

// truncate keeps at most n bytes, never splitting a UTF-8 character
// (Postgres rejects invalid UTF-8 in text columns).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + fmt.Sprintf("\n[truncated %d more bytes]", len(s)-n)
}
