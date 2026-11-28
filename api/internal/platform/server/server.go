// Package server wires the domains into the two HTTP engines: the public API
// the browser uses and the internal API only the agent service may call.
package server

import (
	"dataanalyst/api/internal/chart"
	"dataanalyst/api/internal/chat"
	"dataanalyst/api/internal/dataset"
	"dataanalyst/api/internal/health"
	"dataanalyst/api/internal/platform/httpx"
	"dataanalyst/api/internal/platform/telemetry"
	"dataanalyst/api/internal/session"
	"dataanalyst/api/internal/tool"

	"github.com/gin-gonic/gin"
)

// Handlers are the domain handlers to mount.
type Handlers struct {
	Health  *health.Handler
	Session *session.Handler
	Dataset *dataset.Handler
	Chat    *chat.Handler
	Chart   *chart.Handler
	Tool    *tool.Handler

	InternalToken string
}

// Public is the browser-facing API, mounted at /api.
func (a *App) Public() *gin.Engine { return Public(a.Handlers) }

// Internal is the API for the agent service. Never expose it publicly.
func (a *App) Internal() *gin.Engine { return Internal(a.Handlers) }

func newEngine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(gin.Recovery())
	return e
}

// Public is the browser-facing API, mounted at /api.
func Public(h Handlers) *gin.Engine {
	e := newEngine()
	api := e.Group("/api")
	h.Health.Register(api)
	h.Chart.Register(api)
	scoped := h.Session.Register(api) // POST /sessions and /sessions/:id
	h.Dataset.Register(scoped)
	h.Chat.Register(scoped)
	return e
}

// Internal is the API for the agent service. Never expose it publicly.
func Internal(h Handlers) *gin.Engine {
	e := newEngine()
	// Tool calls continue the agent's trace. The public API has no such
	// middleware: only a Run starts a trace (see chat.Handler).
	e.Use(telemetry.Continue())
	g := e.Group("", httpx.RequireToken(h.InternalToken))
	h.Tool.Register(g)
	return e
}
