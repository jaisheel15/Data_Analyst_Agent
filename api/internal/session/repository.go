package session

import (
	"context"
	"errors"
	"time"

	"dataanalyst/api/internal/platform/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound means no session has the requested id.
var ErrNotFound = errors.New("session not found")

// Repository is the only code in this domain that talks to Postgres.
type Repository struct{ q *db.Queries }

func NewRepository(q *db.Queries) *Repository { return &Repository{q: q} }

func (r *Repository) Create(ctx context.Context, id uuid.UUID) (db.Session, error) {
	return r.q.CreateSession(ctx, id)
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (db.Session, error) {
	s, err := r.q.GetSession(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Session{}, ErrNotFound
	}
	return s, err
}

func (r *Repository) Touch(ctx context.Context, id uuid.UUID) error { return r.q.TouchSession(ctx, id) }

func (r *Repository) ListMessages(ctx context.Context, id uuid.UUID) ([]db.Message, error) {
	return r.q.ListMessages(ctx, id)
}

func (r *Repository) ListExpired(ctx context.Context, idleSince time.Time) ([]uuid.UUID, error) {
	return r.q.ListExpiredSessions(ctx, db.Timestamptz(idleSince))
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.q.DeleteSession(ctx, id)
}
