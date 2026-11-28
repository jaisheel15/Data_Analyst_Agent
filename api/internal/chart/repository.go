package chart

import (
	"context"
	"errors"

	"dataanalyst/api/internal/platform/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound means no chart has the requested id.
var ErrNotFound = errors.New("chart not found")

type Repository struct{ q *db.Queries }

func NewRepository(q *db.Queries) *Repository { return &Repository{q: q} }

func (r *Repository) Create(ctx context.Context, id, sessionID, runID uuid.UUID, path string) error {
	return r.q.CreateChart(ctx, db.CreateChartParams{ID: id, SessionID: sessionID, RunID: runID, Path: path})
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (db.Chart, error) {
	c, err := r.q.GetChart(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Chart{}, ErrNotFound
	}
	return c, err
}
