package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/datasets"
	"dataanalyst/api/internal/db"
	"dataanalyst/api/internal/history"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxQuestionChars = 4000

type askBody struct {
	Question string `json:"question"`
	// Provider and Model override the configured model. Honoured only for
	// eval requests, so the eval harness can compare providers.
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// postMessage runs the agent on a question and streams its work as SSE.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body askBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `expected JSON {"question": "..."}`)
		return
	}
	body.Question = strings.TrimSpace(body.Question)
	if body.Question == "" || len(body.Question) > maxQuestionChars {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("question must be 1-%d characters", maxQuestionChars))
		return
	}

	ctx := r.Context()
	purpose, model := "chat", contract.Model{Provider: s.Config.Provider, Name: s.Config.Model}
	if s.isEval(r) {
		purpose = "eval"
		if body.Model != "" {
			model = contract.Model{Provider: body.Provider, Name: body.Model}
		}
	}
	if _, priced := s.Pricing[model.Name]; !priced {
		slog.Error("refusing run: model has no price", "model", model.Name)
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("model %q has no price in pricing.yaml, so spending cannot be tracked", model.Name))
		return
	}
	if purpose == "chat" {
		allowed, err := s.budget.Allow(ctx)
		if err != nil {
			s.internalError(w, "check budget", err)
			return
		}
		if !allowed {
			writeError(w, http.StatusTooManyRequests, "daily budget reached; chat is available again after midnight UTC")
			return
		}
	}
	sid := sess.ID.String()
	if !s.Sandbox.HasCapacity(sid) {
		writeError(w, http.StatusServiceUnavailable, "all sandboxes are busy; try again in a minute")
		return
	}

	run, err := s.startRun(ctx, sess.ID, body.Question, purpose, model)
	if errors.Is(err, errRunInProgress) {
		writeError(w, http.StatusConflict, "a question is already running in this session")
		return
	}
	if err != nil {
		s.internalError(w, "start run", err)
		return
	}
	_ = s.q.TouchSession(ctx, sess.ID)

	req, err := s.runRequest(ctx, run, body.Question, model)
	if err != nil {
		s.finish(run, finishInfo{status: "error", errText: "could not prepare the run"})
		s.internalError(w, "prepare run", err)
		return
	}
	s.stream(w, r, run, req)
}

var errRunInProgress = errors.New("run in progress")

// startRun saves the question and a running run in one transaction. The
// partial unique index on running runs turns a concurrent second question
// into errRunInProgress, and the rollback discards its message.
func (s *Server) startRun(ctx context.Context, sessionID uuid.UUID, question, purpose string, model contract.Model) (db.AgentRun, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return db.AgentRun{}, err
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)
	msg, err := q.CreateMessage(ctx, db.CreateMessageParams{ID: uuid.New(), SessionID: sessionID, Role: "user", Content: question})
	if err != nil {
		return db.AgentRun{}, err
	}
	run, err := q.CreateRun(ctx, db.CreateRunParams{ID: uuid.New(), SessionID: sessionID, MessageID: msg.ID,
		Purpose: purpose, Provider: model.Provider, Model: model.Name})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return db.AgentRun{}, errRunInProgress
	}
	if err != nil {
		return db.AgentRun{}, err
	}
	return run, tx.Commit(ctx)
}

func (s *Server) runRequest(ctx context.Context, run db.AgentRun, question string, model contract.Model) (contract.RunRequest, error) {
	sid := run.SessionID.String()
	schema, err := datasets.Describe(ctx, s.Layout.DBPath(sid), "")
	if err != nil {
		return contract.RunRequest{}, fmt.Errorf("describe datasets: %w", err)
	}
	hist, err := s.history(ctx, run.SessionID)
	if err != nil {
		return contract.RunRequest{}, fmt.Errorf("history: %w", err)
	}
	return contract.RunRequest{
		RunID: run.ID.String(), SessionID: sid, Question: question, Model: model,
		Schema: schema, History: hist,
		Limits: contract.Limits{MaxSteps: s.Config.MaxSteps, MaxTokens: s.Config.MaxTokens},
	}, nil
}

func (s *Server) history(ctx context.Context, sessionID uuid.UUID) ([]contract.Turn, error) {
	rows, err := s.q.ListFinishedTurns(ctx, db.ListFinishedTurnsParams{SessionID: sessionID, Limit: history.MaxTurns})
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows) // oldest first
	turns := make([]history.StoredTurn, len(rows))
	for i, row := range rows {
		turns[i] = history.StoredTurn{Question: row.Question, Answer: row.Answer}
		if i < len(rows)-history.FullTurns {
			continue // only recent turns carry steps
		}
		calls, err := s.q.ListToolCallsForRun(ctx, row.RunID)
		if err != nil {
			return nil, err
		}
		for _, c := range calls {
			out := c.OutputTruncated
			if c.Error.Valid {
				out = strings.TrimSpace(out + "\nerror: " + c.Error.String)
			}
			turns[i].Steps = append(turns[i].Steps, history.StoredStep{Tool: c.Tool, Input: c.Input, Output: out})
		}
	}
	return history.Build(turns), nil
}

// stream relays agent events to the browser and records how the run ended.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, run db.AgentRun, req contract.RunRequest) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, data any) {
		raw, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		if flusher != nil {
			flusher.Flush()
		}
	}

	start := s.Now()
	finished := false
	var lastUsage *contract.Usage // charged even if the run never reaches done
	err := s.Agent.Run(r.Context(), req, func(ev contract.Event) error {
		switch ev.Type {
		case contract.EventUsage:
			lastUsage = ev.Usage
		case contract.EventStepStart:
			send(ev.Type, map[string]any{"step": ev.Step})
		case contract.EventToolCall:
			send(ev.Type, map[string]any{"step": ev.Step, "tool": ev.Tool, "input": ev.Input})
		case contract.EventToolResult:
			send(ev.Type, map[string]any{"step": ev.Step, "tool": ev.Tool, "summary": ev.Summary,
				"error": ev.Error, "chart_ids": orEmpty(ev.ChartIDs)})
			for _, id := range ev.ChartIDs {
				send("chart", map[string]string{"id": id, "url": "/api/charts/" + id})
			}
		case contract.EventAnswerDelta:
			send(ev.Type, map[string]string{"text": ev.Text})
		case contract.EventDone:
			finished = true
			status := "done"
			if ev.Status == "budget_exhausted" {
				status = "budget_exhausted"
			}
			info := finishInfo{status: status, answer: ev.Answer, usage: ev.Usage, latency: s.Now().Sub(start)}
			cost, msgID := s.finish(run, info)
			send(ev.Type, map[string]any{"answer": ev.Answer, "status": status, "cost_usd": cost,
				"steps": stepsOf(ev.Usage), "run_id": run.ID.String(), "message_id": msgID})
		case contract.EventError:
			finished = true
			usage := ev.Usage
			if usage == nil {
				usage = lastUsage
			}
			s.finish(run, finishInfo{status: "error", errText: ev.Error, usage: usage, latency: s.Now().Sub(start)})
			send(ev.Type, map[string]string{"error": ev.Error})
		}
		return nil
	})
	switch {
	case finished:
	case r.Context().Err() != nil:
		// The browser went away; the agent service saw the stream close.
		s.finish(run, finishInfo{status: "cancelled", usage: lastUsage, latency: s.Now().Sub(start)})
	default:
		slog.Error("agent run failed", "run", run.ID, "err", err)
		s.finish(run, finishInfo{status: "error", errText: errText(err), usage: lastUsage, latency: s.Now().Sub(start)})
		send(contract.EventError, map[string]string{"error": "the agent stopped unexpectedly; please try again"})
	}
}

type finishInfo struct {
	status  string
	answer  string
	errText string
	usage   *contract.Usage
	latency time.Duration
}

// finish records the end of a run. It uses its own context: it must run even
// after the browser has disconnected.
func (s *Server) finish(run db.AgentRun, f finishInfo) (cost float64, answerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := db.FinishRunParams{ID: run.ID, Status: f.status,
		Error:     pgtype.Text{String: f.errText, Valid: f.errText != ""},
		LatencyMs: pgtype.Int4{Int32: int32(f.latency.Milliseconds()), Valid: true}}
	if u := f.usage; u != nil {
		priced := *u
		if _, ok := s.Pricing[priced.Model]; !ok {
			priced.Model = run.Model // the agent reported a name we do not price
		}
		c, err := s.Pricing.Cost(priced)
		if err != nil {
			slog.Error("cost", "run", run.ID, "err", err)
		}
		cost = c
		p.Steps, p.InputTokens, p.OutputTokens = int32(u.Steps), u.InputTokens, u.OutputTokens
		p.CachedTokens, p.CacheWriteTokens, p.CostUsd = u.CachedTokens, u.CacheWriteTokens, cost
	}
	if f.status == "done" || f.status == "budget_exhausted" {
		msg, err := s.q.CreateMessage(ctx, db.CreateMessageParams{ID: uuid.New(), SessionID: run.SessionID,
			Role: "assistant", Content: f.answer})
		if err != nil {
			slog.Error("save answer", "run", run.ID, "err", err)
		} else {
			p.AnswerMessageID = uuid.NullUUID{UUID: msg.ID, Valid: true}
			answerID = msg.ID.String()
		}
	}
	if err := s.q.FinishRun(ctx, p); err != nil {
		slog.Error("finish run", "run", run.ID, "err", err)
	}
	return cost, answerID
}

func (s *Server) isEval(r *http.Request) bool {
	want := []byte(s.Config.EvalToken)
	got := []byte(r.Header.Get("X-Eval-Token"))
	return len(want) > 0 && subtle.ConstantTimeCompare(got, want) == 1
}

func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func stepsOf(u *contract.Usage) int {
	if u == nil {
		return 0
	}
	return u.Steps
}

func errText(err error) string {
	if err == nil {
		return "agent stream ended unexpectedly"
	}
	return err.Error()
}
