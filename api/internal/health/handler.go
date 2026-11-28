// Package health answers the readiness probe.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"dataanalyst/api/internal/platform/httpx"

	"github.com/gin-gonic/gin"
)

// Pinger checks a dependency is up (implemented by agent.Client).
type Pinger interface {
	Ping(ctx context.Context) error
}

type Handler struct{ agent Pinger }

func NewHandler(agent Pinger) *Handler { return &Handler{agent: agent} }

func (h *Handler) Register(api *gin.RouterGroup) { api.GET("/health", h.get) }

// get is ok only when the agent service answers too: the API alone cannot
// answer a question.
func (h *Handler) get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.agent.Ping(ctx); err != nil {
		slog.Warn("health: agent unreachable", "err", err)
		httpx.JSON(c, http.StatusServiceUnavailable, map[string]string{"status": "agent unavailable"})
		return
	}
	httpx.JSON(c, http.StatusOK, map[string]string{"status": "ok"})
}
