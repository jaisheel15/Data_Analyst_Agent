package dataset

import (
	"context"

	"dataanalyst/api/internal/platform/db"

	"github.com/google/uuid"
)

type Repository struct{ q *db.Queries }

func NewRepository(q *db.Queries) *Repository { return &Repository{q: q} }

func (r *Repository) List(ctx context.Context, sessionID uuid.UUID) ([]db.Dataset, error) {
	return r.q.ListDatasets(ctx, sessionID)
}

func (r *Repository) Create(ctx context.Context, p db.CreateDatasetParams) error {
	_, err := r.q.CreateDataset(ctx, p)
	return err
}
