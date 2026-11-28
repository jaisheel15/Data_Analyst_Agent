package server

import (
	"context"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/budget"
	"dataanalyst/api/internal/chart"
	"dataanalyst/api/internal/chat"
	"dataanalyst/api/internal/dataset"
	"dataanalyst/api/internal/health"
	"dataanalyst/api/internal/platform/config"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/storage"
	"dataanalyst/api/internal/run"
	"dataanalyst/api/internal/sandbox"
	"dataanalyst/api/internal/session"
	"dataanalyst/api/internal/tool"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sandbox is everything the domains need from sandbox.Manager.
type Sandbox interface {
	Exec(ctx context.Context, sessionID string, req sandbox.Request) (sandbox.Result, error)
	HasCapacity(sessionID string) bool
	Stop(ctx context.Context, sessionID string) error
}

// Deps are the infrastructure pieces the domains are built on.
type Deps struct {
	Pool    *pgxpool.Pool
	Layout  storage.Layout
	Sandbox Sandbox
	Agent   *agent.Client
	Pricing budget.Pricing
	Now     func() time.Time
	Config  config.Config
}

// App is the wired-up service: every domain, ready to serve.
type App struct {
	Handlers Handlers
	Sessions *session.Service
	Runs     *run.Service
}

// New builds repositories, services and handlers for every domain.
func New(d Deps) *App {
	if d.Now == nil {
		d.Now = time.Now
	}
	cfg := d.Config
	q := db.New(d.Pool)

	runs := run.NewService(run.NewRepository(d.Pool, q), d.Pricing)
	datasets := dataset.NewService(dataset.NewRepository(q), d.Layout, cfg.MaxUploadBytes)
	sessions := session.NewService(session.NewRepository(q), d.Layout, datasets, d.Sandbox)
	charts := chart.NewService(chart.NewRepository(q), d.Layout)
	daily := budget.Budget{Limit: cfg.DailyChatBudgetUSD, Store: runs, Now: d.Now}
	chats := chat.NewService(runs, datasets, d.Sandbox, sessions, d.Pricing, daily, chat.Settings{
		Provider: cfg.Provider, Model: cfg.Model, MaxSteps: cfg.MaxSteps, MaxTokens: cfg.MaxTokens,
	})
	tools := tool.NewService(d.Sandbox, datasets, charts, runs, d.Layout, d.Now)

	return &App{
		Sessions: sessions,
		Runs:     runs,
		Handlers: Handlers{
			Health:        health.NewHandler(d.Agent),
			Session:       session.NewHandler(sessions),
			Dataset:       dataset.NewHandler(datasets, sessions, cfg.MaxUploadBytes),
			Chat:          chat.NewHandler(chats, d.Agent, runs, cfg.EvalToken, d.Now),
			Chart:         chart.NewHandler(charts),
			Tool:          tool.NewHandler(tools),
			InternalToken: cfg.InternalToken,
		},
	}
}
