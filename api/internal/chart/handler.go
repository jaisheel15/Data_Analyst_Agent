package chart

import (
	"errors"
	"net/http"

	"dataanalyst/api/internal/platform/httpx"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(api *gin.RouterGroup) { api.GET("/charts/:id", h.get) }

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusNotFound, "chart not found")
		return
	}
	path, err := h.svc.Path(c.Request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "chart not found")
		return
	}
	if err != nil {
		httpx.InternalError(c, "get chart", err)
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Cache-Control", "private, max-age=86400")
	http.ServeFile(c.Writer, c.Request, path)
}
