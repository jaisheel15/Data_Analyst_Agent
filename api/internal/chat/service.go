// Package chat is the "ask a question" domain: it validates a question,
// checks spending and capacity, opens a run, builds the agent request, and
// relays the agent's events to the browser.
package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/budget"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/run"

	"github.com/google/uuid"
)

const maxQuestionChars = 4000

var (
	ErrBadQuestion   = fmt.Errorf("question must be 1-%d characters", maxQuestionChars)
	ErrOverBudget    = errors.New("daily budget reached; chat is available again after midnight UTC")
	ErrNoCapacity    = errors.New("all sandboxes are busy; try again in a minute")
	ErrRunInProgress = errors.New("a question is already running in this session")
)

// ModelNotPricedError means the model has no entry in pricing.yaml.
type ModelNotPricedError struct{ Model string }

func (e ModelNotPricedError) Error() string {
	return fmt.Sprintf("model %q has no price in pricing.yaml, so spending cannot be tracked", e.Model)
}

// Settings are the run limits and default model.
type Settings struct {
	Provider  string
	Model     string
	MaxSteps  int
	MaxTokens int
}

// Runs is the part of the run domain chat uses.
type Runs interface {
	Start(ctx context.Context, sessionID uuid.UUID, question, purpose string, model agent.Model) (db.AgentRun, error)
	Finish(ctx context.Context, run db.AgentRun, o run.Outcome) (cost float64, answerID string)
	FinishedTurns(ctx context.Context, sessionID uuid.UUID, limit int32) ([]db.ListFinishedTurnsRow, error)
	ToolCalls(ctx context.Context, runID uuid.UUID) ([]db.ToolCall, error)
}

// Datasets describes a session's tables (implemented by the dataset domain).
type Datasets interface {
	Describe(ctx context.Context, sessionID, only string) ([]agent.Table, error)
}

// Sandbox reports whether a session could run code right now.
type Sandbox interface {
	HasCapacity(sessionID string) bool
}

// Sessions marks a session as recently used.
type Sessions interface {
	Touch(ctx context.Context, id uuid.UUID) error
}

type Service struct {
	runs     Runs
	datasets Datasets
	sandbox  Sandbox
	sessions Sessions
	pricing  budget.Pricing
	budget   budget.Budget
	cfg      Settings
}

func NewService(runs Runs, datasets Datasets, sb Sandbox, sessions Sessions, pricing budget.Pricing,
	b budget.Budget, cfg Settings) *Service {
	return &Service{runs: runs, datasets: datasets, sandbox: sb, sessions: sessions, pricing: pricing, budget: b, cfg: cfg}
}

// Question is one request to ask the agent. Eval requests may pick the model.
type Question struct {
	SessionID uuid.UUID
	Text      string
	Eval      bool
	Provider  string // honoured only when Eval
	Model     string // honoured only when Eval
}

// Start validates the question and opens a run. The returned request is ready
// to send to the agent service.
func (s *Service) Start(ctx context.Context, q Question) (db.AgentRun, agent.RunRequest, error) {
	text := strings.TrimSpace(q.Text)
	if text == "" || len(text) > maxQuestionChars {
		return db.AgentRun{}, agent.RunRequest{}, ErrBadQuestion
	}
	purpose, model := "chat", agent.Model{Provider: s.cfg.Provider, Name: s.cfg.Model}
	if q.Eval {
		purpose = "eval"
		if q.Model != "" {
			model = agent.Model{Provider: q.Provider, Name: q.Model}
		}
	}
	if _, priced := s.pricing[model.Name]; !priced {
		return db.AgentRun{}, agent.RunRequest{}, ModelNotPricedError{Model: model.Name}
	}
	if purpose == "chat" {
		allowed, err := s.budget.Allow(ctx)
		if err != nil {
			return db.AgentRun{}, agent.RunRequest{}, fmt.Errorf("check budget: %w", err)
		}
		if !allowed {
			return db.AgentRun{}, agent.RunRequest{}, ErrOverBudget
		}
	}
	if !s.sandbox.HasCapacity(q.SessionID.String()) {
		return db.AgentRun{}, agent.RunRequest{}, ErrNoCapacity
	}

	r, err := s.runs.Start(ctx, q.SessionID, text, purpose, model)
	if errors.Is(err, run.ErrInProgress) {
		return db.AgentRun{}, agent.RunRequest{}, ErrRunInProgress
	}
	if err != nil {
		return db.AgentRun{}, agent.RunRequest{}, fmt.Errorf("start run: %w", err)
	}
	_ = s.sessions.Touch(ctx, q.SessionID)

	req, err := s.request(ctx, r, text, model)
	if err != nil {
		s.runs.Finish(ctx, r, run.Outcome{Status: run.StatusError, ErrText: "could not prepare the run"})
		return db.AgentRun{}, agent.RunRequest{}, fmt.Errorf("prepare run: %w", err)
	}
	return r, req, nil
}

func (s *Service) request(ctx context.Context, r db.AgentRun, question string, model agent.Model) (agent.RunRequest, error) {
	sid := r.SessionID.String()
	schema, err := s.datasets.Describe(ctx, sid, "")
	if err != nil {
		return agent.RunRequest{}, fmt.Errorf("describe datasets: %w", err)
	}
	hist, err := s.history(ctx, r.SessionID)
	if err != nil {
		return agent.RunRequest{}, fmt.Errorf("history: %w", err)
	}
	return agent.RunRequest{
		RunID: r.ID.String(), SessionID: sid, Question: question, Model: model,
		Schema: schema, History: hist,
		Limits: agent.Limits{MaxSteps: s.cfg.MaxSteps, MaxTokens: s.cfg.MaxTokens},
	}, nil
}

func (s *Service) history(ctx context.Context, sessionID uuid.UUID) ([]agent.Turn, error) {
	rows, err := s.runs.FinishedTurns(ctx, sessionID, MaxTurns)
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows) // oldest first
	turns := make([]StoredTurn, len(rows))
	for i, row := range rows {
		turns[i] = StoredTurn{Question: row.Question, Answer: row.Answer}
		if i < len(rows)-FullTurns {
			continue // only recent turns carry steps
		}
		calls, err := s.runs.ToolCalls(ctx, row.RunID)
		if err != nil {
			return nil, err
		}
		for _, c := range calls {
			out := c.OutputTruncated
			if c.Error.Valid {
				out = strings.TrimSpace(out + "\nerror: " + c.Error.String)
			}
			turns[i].Steps = append(turns[i].Steps, StoredStep{Tool: c.Tool, Input: c.Input, Output: out})
		}
	}
	return Build(turns), nil
}
