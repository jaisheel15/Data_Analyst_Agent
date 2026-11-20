package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/datasets"
	"dataanalyst/api/internal/db"
	"dataanalyst/api/internal/sandbox"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxToolOutput = 8000 // bytes of tool output the model sees
	maxSQLRows    = 50
)

func (s *Server) requireInternalToken(next http.Handler) http.Handler {
	want := []byte(s.Config.InternalToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("X-Internal-Token"))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid internal token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) runTool(w http.ResponseWriter, r *http.Request) {
	tool := chi.URLParam(r, "tool")
	switch tool {
	case contract.ToolDescribeDataset, contract.ToolRunSQL, contract.ToolRunPython:
	default:
		writeError(w, http.StatusNotFound, "unknown tool "+tool)
		return
	}
	var req contract.ToolRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid tool request")
		return
	}
	run, ok := s.runForTool(w, r.Context(), req)
	if !ok {
		return
	}

	start := s.Now()
	resp := s.dispatch(r.Context(), tool, run, req.Input)
	resp.Output = truncate(resp.Output, maxToolOutput)
	if resp.ChartIDs == nil {
		resp.ChartIDs = []string{}
	}
	if err := s.recordToolCall(r.Context(), run.ID, tool, req.Input, resp, s.Now().Sub(start)); err != nil {
		s.internalError(w, "record tool call", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// runForTool checks the call belongs to a running run of the stated session.
func (s *Server) runForTool(w http.ResponseWriter, ctx context.Context, req contract.ToolRequest) (db.AgentRun, bool) {
	runID, err1 := uuid.Parse(req.RunID)
	sessionID, err2 := uuid.Parse(req.SessionID)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return db.AgentRun{}, false
	}
	run, err := s.q.GetRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && run.SessionID != sessionID) {
		writeError(w, http.StatusNotFound, "run not found")
		return db.AgentRun{}, false
	}
	if err != nil {
		s.internalError(w, "get run", err)
		return db.AgentRun{}, false
	}
	if run.Status != "running" {
		writeError(w, http.StatusConflict, "run is not running")
		return db.AgentRun{}, false
	}
	return run, true
}

func (s *Server) dispatch(ctx context.Context, tool string, run db.AgentRun, input json.RawMessage) contract.ToolResponse {
	var in struct {
		Table string `json:"table"`
		SQL   string `json:"sql"`
		Code  string `json:"code"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return contract.ToolResponse{Error: "tool input must be a JSON object"}
		}
	}
	sid := run.SessionID.String()
	switch tool {
	case contract.ToolDescribeDataset:
		tables, err := datasets.Describe(ctx, s.Layout.DBPath(sid), in.Table)
		if err != nil {
			return contract.ToolResponse{Error: err.Error()}
		}
		return contract.ToolResponse{Output: formatTables(tables)}
	case contract.ToolRunSQL:
		if strings.TrimSpace(in.SQL) == "" {
			return contract.ToolResponse{Error: `run_sql needs a non-empty "sql" string`}
		}
		res, errResp := s.exec(ctx, sid, sandbox.Request{Mode: "sql", Code: in.SQL, MaxRows: maxSQLRows})
		if errResp != nil {
			return *errResp
		}
		return contract.ToolResponse{Output: formatSQL(res), Error: res.Error}
	default: // run_python
		if strings.TrimSpace(in.Code) == "" {
			return contract.ToolResponse{Error: `run_python needs a non-empty "code" string`}
		}
		res, errResp := s.exec(ctx, sid, sandbox.Request{Mode: "python", Code: in.Code, MaxOutputChars: maxToolOutput})
		if errResp != nil {
			return *errResp
		}
		ids, err := s.keepCharts(ctx, run, res.Files)
		if err != nil {
			slog.Error("keep charts", "err", err)
		}
		return contract.ToolResponse{Output: formatPython(res, ids), Error: res.Error, ChartIDs: ids}
	}
}

// exec runs a sandbox request with a clean out dir. Infrastructure failures
// become tool errors the agent can read, never HTTP errors.
func (s *Server) exec(ctx context.Context, sessionID string, req sandbox.Request) (sandbox.Result, *contract.ToolResponse) {
	clearDir(s.Layout.OutDir(sessionID))
	res, err := s.Sandbox.Exec(ctx, sessionID, req)
	switch {
	case errors.Is(err, sandbox.ErrAtCapacity):
		return res, &contract.ToolResponse{Error: "all sandboxes are busy right now; wait a moment and try again"}
	case err != nil:
		slog.Error("sandbox exec", "session", sessionID, "err", err)
		return res, &contract.ToolResponse{Error: "the sandbox failed to run this call; try again"}
	}
	return res, nil
}

// keepCharts moves PNGs out of the sandbox out dir into the session's charts
// dir, so the next call's chart_1.png cannot overwrite this one.
func (s *Server) keepCharts(ctx context.Context, run db.AgentRun, files []string) ([]string, error) {
	sid := run.SessionID.String()
	var ids []string
	for _, f := range files {
		if filepath.Base(f) != f || !strings.HasSuffix(strings.ToLower(f), ".png") {
			continue // only plain PNG names written by the runner
		}
		// Agent code controls /out. Lstat (not Stat) so a symlink to, say,
		// /proc/self/environ is rejected instead of followed and served.
		info, err := os.Lstat(filepath.Join(s.Layout.OutDir(sid), f))
		if err != nil || !info.Mode().IsRegular() {
			slog.Warn("ignoring non-regular file in sandbox out dir", "session", sid, "file", f)
			continue
		}
		id := uuid.New()
		dst := filepath.Join(s.Layout.ChartsDir(sid), id.String()+".png")
		if err := os.Rename(filepath.Join(s.Layout.OutDir(sid), f), dst); err != nil {
			return ids, err
		}
		if err := s.q.CreateChart(ctx, db.CreateChartParams{ID: id, SessionID: run.SessionID, RunID: run.ID, Path: dst}); err != nil {
			return ids, err
		}
		ids = append(ids, id.String())
	}
	clearDir(s.Layout.OutDir(sid))
	return ids, nil
}

func (s *Server) recordToolCall(ctx context.Context, runID uuid.UUID, tool string, input json.RawMessage,
	resp contract.ToolResponse, took time.Duration) error {
	seq, err := s.q.NextToolSeq(ctx, runID)
	if err != nil {
		return err
	}
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	return s.q.CreateToolCall(ctx, db.CreateToolCallParams{
		ID: uuid.New(), RunID: runID, Seq: seq, Tool: tool, Input: input,
		OutputTruncated: resp.Output,
		Error:           pgtype.Text{String: resp.Error, Valid: resp.Error != ""},
		DurationMs:      int32(took.Milliseconds()),
	})
}

func (s *Server) getChart(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "chart not found")
		return
	}
	chart, err := s.q.GetChart(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "chart not found")
		return
	}
	if err != nil {
		s.internalError(w, "get chart", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, chart.Path)
}

func clearDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
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
