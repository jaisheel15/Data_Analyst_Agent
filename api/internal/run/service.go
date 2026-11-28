// Package run owns the lifecycle of one agent run: starting it, recording its
// tool calls, and pricing and closing it. Chat and tool both build on it.
package run

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/budget"
	"dataanalyst/api/internal/platform/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type Service struct {
	repo    *Repository
	pricing budget.Pricing
}

func NewService(repo *Repository, pricing budget.Pricing) *Service {
	return &Service{repo: repo, pricing: pricing}
}

// Start opens a run for a question; ErrInProgress if the session is busy.
func (s *Service) Start(ctx context.Context, sessionID uuid.UUID, question, purpose string, model agent.Model) (db.AgentRun, error) {
	return s.repo.Start(ctx, sessionID, question, purpose, model.Provider, model.Name)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (db.AgentRun, error) {
	return s.repo.Get(ctx, id)
}

// FailStale marks runs left running by a restart as failed.
func (s *Service) FailStale(ctx context.Context) error { return s.repo.FailStale(ctx) }

// SpentSince implements budget.SpendStore.
func (s *Service) SpentSince(ctx context.Context, since time.Time) (float64, error) {
	return s.repo.SpentSince(ctx, since)
}

// FinishedTurns returns finished question/answer pairs, newest first.
func (s *Service) FinishedTurns(ctx context.Context, sessionID uuid.UUID, limit int32) ([]db.ListFinishedTurnsRow, error) {
	return s.repo.FinishedTurns(ctx, sessionID, limit)
}

func (s *Service) ToolCalls(ctx context.Context, runID uuid.UUID) ([]db.ToolCall, error) {
	return s.repo.ToolCalls(ctx, runID)
}

// RecordToolCall stores one tool call under the run's next sequence number.
func (s *Service) RecordToolCall(ctx context.Context, runID uuid.UUID, tool string, input json.RawMessage,
	resp agent.ToolResponse, took time.Duration) error {
	seq, err := s.repo.NextToolSeq(ctx, runID)
	if err != nil {
		return err
	}
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	return s.repo.CreateToolCall(ctx, db.CreateToolCallParams{
		ID: uuid.New(), RunID: runID, Seq: seq, Tool: tool, Input: input,
		OutputTruncated: resp.Output,
		Error:           pgtype.Text{String: resp.Error, Valid: resp.Error != ""},
		DurationMs:      int32(took.Milliseconds()),
	})
}

// Finish records the end of a run and returns its cost and the answer message
// id (empty unless the run produced an answer). It is not cancelled with ctx:
// it must run even after the browser has disconnected. ctx still carries the
// Run's trace, so its log lines lead to it.
func (s *Service) Finish(ctx context.Context, run db.AgentRun, o Outcome) (cost float64, answerID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	p := db.FinishRunParams{ID: run.ID, Status: o.Status,
		Error:     pgtype.Text{String: o.ErrText, Valid: o.ErrText != ""},
		LatencyMs: pgtype.Int4{Int32: int32(o.Latency.Milliseconds()), Valid: true}}
	if u := o.Usage; u != nil {
		priced := *u
		if _, ok := s.pricing[priced.Model]; !ok {
			priced.Model = run.Model // the agent reported a name we do not price
		}
		c, err := s.pricing.Cost(priced)
		if err != nil {
			slog.ErrorContext(ctx, "cost", "run", run.ID, "err", err)
		}
		cost = c
		p.Steps, p.InputTokens, p.OutputTokens = int32(u.Steps), u.InputTokens, u.OutputTokens
		p.CachedTokens, p.CacheWriteTokens, p.CostUsd = u.CachedTokens, u.CacheWriteTokens, cost
	}
	if o.Status == StatusDone || o.Status == StatusBudgetExhausted {
		msg, err := s.repo.CreateAnswer(ctx, run.SessionID, o.Answer)
		if err != nil {
			slog.ErrorContext(ctx, "save answer", "run", run.ID, "err", err)
		} else {
			p.AnswerMessageID = uuid.NullUUID{UUID: msg.ID, Valid: true}
			answerID = msg.ID.String()
		}
	}
	if err := s.repo.Finish(ctx, p); err != nil {
		slog.ErrorContext(ctx, "finish run", "run", run.ID, "err", err)
	}
	return cost, answerID
}
