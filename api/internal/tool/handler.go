package tool

import (
	"encoding/json"
	"errors"
	"net/http"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/httpx"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register adds the tool route. internal must already require the internal token.
func (h *Handler) Register(internal *gin.RouterGroup) {
	internal.POST("/internal/tools/:tool", h.run)
}

func (h *Handler) run(c *gin.Context) {
	tool := c.Param("tool")
	if !IsTool(tool) {
		httpx.Error(c, http.StatusNotFound, "unknown tool "+tool)
		return
	}
	var req agent.ToolRequest
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)).Decode(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid tool request")
		return
	}
	ctx := c.Request.Context()
	r, err := h.svc.RunFor(ctx, req)
	switch {
	case errors.Is(err, ErrRunNotFound):
		httpx.Error(c, http.StatusNotFound, "run not found")
		return
	case errors.Is(err, ErrRunNotRunning):
		httpx.Error(c, http.StatusConflict, "run is not running")
		return
	case err != nil:
		httpx.InternalError(c, "get run", err)
		return
	}
	resp, err := h.svc.Execute(ctx, tool, r, req.Input)
	if err != nil {
		httpx.InternalError(c, "record tool call", err)
		return
	}
	httpx.JSON(c, http.StatusOK, resp)
}
