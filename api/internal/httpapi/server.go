// Package httpapi is the Go service's HTTP layer: the public API used by the
// browser and the internal API used only by the Python agent service. They
// are separate handlers so they can listen on separate ports.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/cost"
	"dataanalyst/api/internal/datasets"
	"dataanalyst/api/internal/db"
	"dataanalyst/api/internal/sandbox"
	"dataanalyst/api/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sandbox is the part of sandbox.Manager the HTTP layer uses.
type Sandbox interface {
	Exec(ctx context.Context, sessionID string, req sandbox.Request) (sandbox.Result, error)
	HasCapacity(sessionID string) bool
	Stop(ctx context.Context, sessionID string) error
}

// AgentRunner starts a run on the agent service and streams its events.
type AgentRunner interface {
	Run(ctx context.Context, req contract.RunRequest, emit func(contract.Event) error) error
}

type Config struct {
	InternalToken      string
	EvalToken          string
	MaxUploadBytes     int64
	DailyChatBudgetUSD float64
	Provider           string
	Model              string
	MaxSteps           int
	MaxTokens          int
}

type Deps struct {
	Pool    *pgxpool.Pool
	Layout  storage.Layout
	Sandbox Sandbox
	Agent   AgentRunner
	Pricing cost.Pricing
	Now     func() time.Time
	Config  Config
}

type Server struct {
	Deps
	q      *db.Queries
	loader datasets.Loader
	budget cost.Budget
}

func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	q := db.New(d.Pool)
	return &Server{
		Deps:   d,
		q:      q,
		loader: datasets.Loader{Layout: d.Layout, MaxBytes: d.Config.MaxUploadBytes},
		budget: cost.Budget{Limit: d.Config.DailyChatBudgetUSD, Store: spendStore{q}, Now: d.Now},
	}
}

// Public is the browser-facing API.
func (s *Server) Public() http.Handler {
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Post("/sessions", s.createSession)
		r.Get("/sessions/{id}", s.getSession)
		r.Post("/sessions/{id}/datasets", s.uploadDataset)
		r.Post("/sessions/{id}/messages", s.postMessage)
		r.Get("/charts/{id}", s.getChart)
	})
	return r
}

// Internal is the API for the agent service. Never expose it publicly.
func (s *Server) Internal() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requireInternalToken)
	r.Post("/internal/tools/{tool}", s.runTool)
	return r
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// session loads the {id} session or writes a 404.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (db.Session, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return db.Session{}, false
	}
	sess, err := s.q.GetSession(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "session not found")
		return db.Session{}, false
	}
	if err != nil {
		s.internalError(w, "get session", err)
		return db.Session{}, false
	}
	return sess, true
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type spendStore struct{ q *db.Queries }

func (s spendStore) SpentSince(ctx context.Context, since time.Time) (float64, error) {
	return s.q.SpentSince(ctx, pgTime(since))
}
