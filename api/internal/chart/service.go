// Package chart owns the PNG charts the agent's Python code produces.
package chart

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"dataanalyst/api/internal/platform/storage"

	"github.com/google/uuid"
)

type Service struct {
	repo   *Repository
	layout storage.Layout
}

func NewService(repo *Repository, layout storage.Layout) *Service {
	return &Service{repo: repo, layout: layout}
}

// Keep moves PNGs out of the sandbox out dir into the session's charts dir,
// so the next call's chart_1.png cannot overwrite this one. It returns the
// new chart ids.
func (s *Service) Keep(ctx context.Context, sessionID, runID uuid.UUID, files []string) ([]string, error) {
	sid := sessionID.String()
	var ids []string
	for _, f := range files {
		if filepath.Base(f) != f || !strings.HasSuffix(strings.ToLower(f), ".png") {
			continue // only plain PNG names written by the runner
		}
		// Agent code controls /out. Lstat (not Stat) so a symlink to, say,
		// /proc/self/environ is rejected instead of followed and served.
		info, err := os.Lstat(filepath.Join(s.layout.OutDir(sid), f))
		if err != nil || !info.Mode().IsRegular() {
			slog.Warn("ignoring non-regular file in sandbox out dir", "session", sid, "file", f)
			continue
		}
		id := uuid.New()
		dst := filepath.Join(s.layout.ChartsDir(sid), id.String()+".png")
		if err := os.Rename(filepath.Join(s.layout.OutDir(sid), f), dst); err != nil {
			return ids, err
		}
		if err := s.repo.Create(ctx, id, sessionID, runID, dst); err != nil {
			return ids, err
		}
		ids = append(ids, id.String())
	}
	s.layout.ClearOut(sid)
	return ids, nil
}

// Path returns where a chart's PNG lives, or ErrNotFound.
func (s *Service) Path(ctx context.Context, id uuid.UUID) (string, error) {
	c, err := s.repo.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return c.Path, nil
}
