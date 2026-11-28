// Command api is the Go service: the public API for the browser, the
// internal tool API for the agent service, and the sandbox manager.
//
// This file builds the infrastructure (database, Docker sandbox, config) and
// hands it to server.New, which wires the domains; each domain lives in its own
// package under internal/ (session, dataset, chat, run, tool, chart, ...).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/budget"
	"dataanalyst/api/internal/platform/config"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/server"
	"dataanalyst/api/internal/platform/storage"
	"dataanalyst/api/internal/platform/telemetry"
	"dataanalyst/api/internal/sandbox"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"
)

func main() {
	slog.SetDefault(slog.New(telemetry.LogHandler{Handler: slog.NewJSONHandler(os.Stdout, nil)}))
	if err := runServer(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runServer() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := config.Load()

	shutdownTracing, err := telemetry.Setup(ctx, cfg.TracingEndpoint, cfg.TracingHidePayloads)
	if err != nil {
		return err
	}
	defer func() { // flush the last spans
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flush)
	}()
	if cfg.TracingEndpoint != "" {
		slog.Info("tracing on", "endpoint", cfg.TracingEndpoint, "hide_payloads", cfg.TracingHidePayloads)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	pricing, err := budget.LoadPricing(cfg.PricingFile)
	if err != nil {
		return err
	}
	if _, ok := pricing[cfg.Model]; !ok {
		return errors.New("LLM_MODEL " + cfg.Model + " has no price in pricing.yaml")
	}

	layout := storage.Layout{Root: cfg.DataDir}
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	sb, err := sandbox.New(ctx, docker, sandbox.Config{
		Image:        cfg.SandboxImage,
		Runtime:      cfg.SandboxRuntime,
		Layout:       layout,
		HostRoot:     cfg.HostDataDir,
		MaxSandboxes: cfg.MaxSandboxes,
		CallTimeout:  cfg.SandboxTimeout,
		IdleTimeout:  15 * time.Minute,
		MemoryBytes:  cfg.SandboxMemory,
		NanoCPUs:     1_000_000_000,
		PidsLimit:    128,
	})
	if err != nil {
		return err
	}
	defer sb.Shutdown(context.Background())
	slog.Info("sandbox ready", "runtime", sb.Runtime())
	go sb.Run(ctx)

	app := server.New(server.Deps{
		Pool: pool, Layout: layout, Sandbox: sb, Pricing: pricing, Now: time.Now, Config: cfg,
		Agent: agent.New(cfg.AgentURL, cfg.InternalToken),
	})
	if err := app.Runs.FailStale(ctx); err != nil { // runs cut off by a restart
		return err
	}
	go app.Sessions.RunCleanup(ctx)

	public := &http.Server{Addr: cfg.PublicAddr, Handler: app.Public(), ReadHeaderTimeout: 10 * time.Second}
	internal := &http.Server{Addr: cfg.InternalAddr, Handler: app.Internal(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	for _, s := range []*http.Server{public, internal} {
		go func() {
			slog.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdown)
	_ = internal.Shutdown(shutdown)
	return nil
}
