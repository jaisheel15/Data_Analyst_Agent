// Package session owns browser sessions: creating them, loading them for
// other domains, describing them, and deleting the ones that went idle.
package session

import (
	"context"
	"log/slog"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/storage"

	"github.com/google/uuid"
)

// TTL is how long a session may sit idle before it is deleted.
const TTL = 24 * time.Hour

// Datasets lists a session's tables (implemented by the dataset domain).
type Datasets interface {
	List(ctx context.Context, sessionID uuid.UUID) ([]agent.Table, error)
}

// Sandboxes stops a session's sandbox (implemented by sandbox.Manager).
type Sandboxes interface {
	Stop(ctx context.Context, sessionID string) error
}

type Service struct {
	repo      *Repository
	layout    storage.Layout
	datasets  Datasets
	sandboxes Sandboxes
}

func NewService(repo *Repository, layout storage.Layout, datasets Datasets, sandboxes Sandboxes) *Service {
	return &Service{repo: repo, layout: layout, datasets: datasets, sandboxes: sandboxes}
}

// Create makes a session row and its directories.
func (s *Service) Create(ctx context.Context) (uuid.UUID, error) {
	sess, err := s.repo.Create(ctx, uuid.New())
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.layout.EnsureSession(sess.ID.String()); err != nil {
		return uuid.Nil, err
	}
	return sess.ID, nil
}

// Get returns ErrNotFound when the session does not exist.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (db.Session, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) Touch(ctx context.Context, id uuid.UUID) error { return s.repo.Touch(ctx, id) }

// View describes a session for the browser: its tables and messages.
func (s *Service) View(ctx context.Context, sess db.Session) (View, error) {
	tables, err := s.datasets.List(ctx, sess.ID)
	if err != nil {
		return View{}, err
	}
	msgs, err := s.repo.ListMessages(ctx, sess.ID)
	if err != nil {
		return View{}, err
	}
	view := View{ID: sess.ID.String(), Datasets: tables, Messages: []MessageView{}}
	for _, m := range msgs {
		view.Messages = append(view.Messages, MessageView{
			ID: m.ID.String(), Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt.Time,
		})
	}
	return view, nil
}

// RunCleanup deletes sessions idle for TTL (rows, files, sandbox) until ctx ends.
func (s *Service) RunCleanup(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		ids, err := s.repo.ListExpired(ctx, time.Now().Add(-TTL))
		if err != nil {
			slog.Error("list expired sessions", "err", err)
		}
		for _, id := range ids {
			_ = s.sandboxes.Stop(ctx, id.String())
			if err := s.layout.RemoveSession(id.String()); err != nil {
				slog.Error("remove session files", "session", id, "err", err)
				continue
			}
			if err := s.repo.Delete(ctx, id); err != nil {
				slog.Error("delete session", "session", id, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
