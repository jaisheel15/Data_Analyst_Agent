package run

import (
	"context"
	"errors"
	"time"

	"dataanalyst/api/internal/platform/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInProgress means the session already has a running run.
	ErrInProgress = errors.New("run in progress")
	// ErrNotFound means no run has the requested id.
	ErrNotFound = errors.New("run not found")
)

type Repository struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewRepository(pool *pgxpool.Pool, q *db.Queries) *Repository {
	return &Repository{pool: pool, q: q}
}

// Start saves the question and a running run in one transaction. The partial
// unique index on running runs turns a concurrent second question into
// ErrInProgress, and the rollback discards its message.
func (r *Repository) Start(ctx context.Context, sessionID uuid.UUID, question, purpose, provider, model string) (db.AgentRun, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return db.AgentRun{}, err
	}
	defer tx.Rollback(ctx)
	q := r.q.WithTx(tx)
	msg, err := q.CreateMessage(ctx, db.CreateMessageParams{ID: uuid.New(), SessionID: sessionID, Role: "user", Content: question})
	if err != nil {
		return db.AgentRun{}, err
	}
	run, err := q.CreateRun(ctx, db.CreateRunParams{ID: uuid.New(), SessionID: sessionID, MessageID: msg.ID,
		Purpose: purpose, Provider: provider, Model: model})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return db.AgentRun{}, ErrInProgress
	}
	if err != nil {
		return db.AgentRun{}, err
	}
	return run, tx.Commit(ctx)
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (db.AgentRun, error) {
	run, err := r.q.GetRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentRun{}, ErrNotFound
	}
	return run, err
}

func (r *Repository) Finish(ctx context.Context, p db.FinishRunParams) error {
	return r.q.FinishRun(ctx, p)
}

func (r *Repository) CreateAnswer(ctx context.Context, sessionID uuid.UUID, content string) (db.Message, error) {
	return r.q.CreateMessage(ctx, db.CreateMessageParams{ID: uuid.New(), SessionID: sessionID,
		Role: "assistant", Content: content})
}

func (r *Repository) FailStale(ctx context.Context) error { return r.q.FailStaleRuns(ctx) }

func (r *Repository) SpentSince(ctx context.Context, since time.Time) (float64, error) {
	return r.q.SpentSince(ctx, db.Timestamptz(since))
}

func (r *Repository) NextToolSeq(ctx context.Context, runID uuid.UUID) (int32, error) {
	return r.q.NextToolSeq(ctx, runID)
}

func (r *Repository) CreateToolCall(ctx context.Context, p db.CreateToolCallParams) error {
	return r.q.CreateToolCall(ctx, p)
}

func (r *Repository) ToolCalls(ctx context.Context, runID uuid.UUID) ([]db.ToolCall, error) {
	return r.q.ListToolCallsForRun(ctx, runID)
}

func (r *Repository) FinishedTurns(ctx context.Context, sessionID uuid.UUID, limit int32) ([]db.ListFinishedTurnsRow, error) {
	return r.q.ListFinishedTurns(ctx, db.ListFinishedTurnsParams{SessionID: sessionID, Limit: limit})
}
