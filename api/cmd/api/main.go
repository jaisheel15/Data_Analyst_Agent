// Command api is the Go service: the public API for the browser, the
// internal tool API for the agent service, and the sandbox manager.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"dataanalyst/api/internal/agentclient"
	"dataanalyst/api/internal/cost"
	"dataanalyst/api/internal/db"
	"dataanalyst/api/internal/httpapi"
	"dataanalyst/api/internal/sandbox"
	"dataanalyst/api/internal/storage"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"
)

const sessionTTL = 24 * time.Hour

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	q := db.New(pool)
	if err := q.FailStaleRuns(ctx); err != nil { // runs cut off by a restart
		return err
	}

	pricing, err := cost.LoadPricing(env("PRICING_FILE", "/config/pricing.yaml"))
	if err != nil {
		return err
	}
	model := mustEnv("LLM_MODEL")
	if _, ok := pricing[model]; !ok {
		return errors.New("LLM_MODEL " + model + " has no price in pricing.yaml")
	}

	layout := storage.Layout{Root: env("DATA_DIR", "/data")}
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	sb, err := sandbox.New(ctx, docker, sandbox.Config{
		Image:        env("SANDBOX_IMAGE", "daa-sandbox:latest"),
		Runtime:      env("SANDBOX_RUNTIME", "runsc"),
		Layout:       layout,
		HostRoot:     env("HOST_DATA_DIR", layout.Root),
		MaxSandboxes: envInt("MAX_SANDBOXES", 3),
		CallTimeout:  time.Duration(envInt("SANDBOX_TIMEOUT_SECONDS", 30)) * time.Second,
		IdleTimeout:  15 * time.Minute,
		MemoryBytes:  int64(envInt("SANDBOX_MEMORY_MB", 1024)) << 20,
		NanoCPUs:     1_000_000_000,
		PidsLimit:    128,
	})
	if err != nil {
		return err
	}
	defer sb.Shutdown(context.Background())
	slog.Info("sandbox ready", "runtime", sb.Runtime())
	go sb.Run(ctx)

	token := mustEnv("INTERNAL_TOKEN")
	srv := httpapi.New(httpapi.Deps{
		Pool:    pool,
		Layout:  layout,
		Sandbox: sb,
		Agent:   agentclient.New(env("AGENT_URL", "http://agent:8000"), token),
		Pricing: pricing,
		Config: httpapi.Config{
			InternalToken:      token,
			EvalToken:          os.Getenv("EVAL_TOKEN"),
			MaxUploadBytes:     int64(envInt("MAX_UPLOAD_MB", 25)) << 20,
			DailyChatBudgetUSD: envFloat("DAILY_CHAT_BUDGET_USD", 0.50),
			Provider:           env("LLM_PROVIDER", "anthropic"),
			Model:              model,
			MaxSteps:           envInt("MAX_STEPS", 10),
			MaxTokens:          envInt("MAX_TOKENS", 4096),
		},
	})
	go cleanupSessions(ctx, q, layout, sb)

	public := &http.Server{Addr: env("PUBLIC_ADDR", ":8080"), Handler: srv.Public(), ReadHeaderTimeout: 10 * time.Second}
	internal := &http.Server{Addr: env("INTERNAL_ADDR", ":8081"), Handler: srv.Internal(), ReadHeaderTimeout: 10 * time.Second}
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

// cleanupSessions deletes sessions idle for sessionTTL: rows, files, sandbox.
func cleanupSessions(ctx context.Context, q *db.Queries, layout storage.Layout, sb *sandbox.Manager) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		ids, err := q.ListExpiredSessions(ctx, pgTime(time.Now().Add(-sessionTTL)))
		if err != nil {
			slog.Error("list expired sessions", "err", err)
		}
		for _, id := range ids {
			_ = sb.Stop(ctx, id.String())
			if err := layout.RemoveSession(id.String()); err != nil {
				slog.Error("remove session files", "session", id, "err", err)
				continue
			}
			if err := q.DeleteSession(ctx, id); err != nil {
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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required environment variable", "key", key)
		os.Exit(1)
	}
	return v
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	f, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil {
		return def
	}
	return f
}
