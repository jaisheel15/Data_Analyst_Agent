package dataset

import (
	"context"
	"encoding/json"
	"io"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/storage"

	"github.com/google/uuid"
)

// Service loads uploads into a session's DuckDB file and records them.
type Service struct {
	repo   *Repository
	loader Loader
	layout storage.Layout
}

func NewService(repo *Repository, layout storage.Layout, maxBytes int64) *Service {
	return &Service{repo: repo, layout: layout, loader: Loader{Layout: layout, MaxBytes: maxBytes}}
}

// Upload stores file as a new table of the session.
func (s *Service) Upload(ctx context.Context, sessionID uuid.UUID, filename string, file io.Reader) (agent.Table, error) {
	existing, err := s.repo.List(ctx, sessionID)
	if err != nil {
		return agent.Table{}, err
	}
	names := make([]string, len(existing))
	for i, d := range existing {
		names[i] = d.TableName
	}
	table, err := s.loader.Load(ctx, sessionID.String(), filename, file, names)
	if err != nil {
		return agent.Table{}, err
	}
	cols, _ := json.Marshal(table.Columns)
	err = s.repo.Create(ctx, db.CreateDatasetParams{
		ID: uuid.New(), SessionID: sessionID, TableName: table.Name,
		OriginalFilename: filename, Rows: table.Rows, ColumnsJson: cols,
	})
	return table, err
}

// List returns the session's tables as recorded at upload time.
func (s *Service) List(ctx context.Context, sessionID uuid.UUID) ([]agent.Table, error) {
	rows, err := s.repo.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]agent.Table, 0, len(rows))
	for _, d := range rows {
		t := agent.Table{Name: d.TableName, Rows: d.Rows, Columns: []agent.Column{}}
		_ = json.Unmarshal(d.ColumnsJson, &t.Columns)
		out = append(out, t)
	}
	return out, nil
}

// Describe reads the live database: every table, or only the named one, with sample rows.
func (s *Service) Describe(ctx context.Context, sessionID, only string) ([]agent.Table, error) {
	return Describe(ctx, s.layout.DBPath(sessionID), only)
}
