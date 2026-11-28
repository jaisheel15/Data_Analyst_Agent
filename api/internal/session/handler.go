package session

import (
	"errors"
	"net/http"

	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/httpx"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const ctxKey = "session"

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register adds POST /sessions to api, and returns the group every
// session-scoped route hangs off (/sessions/:id, with the session loaded).
func (h *Handler) Register(api *gin.RouterGroup) *gin.RouterGroup {
	api.POST("/sessions", h.create)
	scoped := api.Group("/sessions/:id", h.Load())
	scoped.GET("", h.get)
	return scoped
}

// Load is middleware that resolves :id to a session, or answers 404.
func (h *Handler) Load() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			httpx.Error(c, http.StatusNotFound, "session not found")
			c.Abort()
			return
		}
		sess, err := h.svc.Get(c.Request.Context(), id)
		if errors.Is(err, ErrNotFound) {
			httpx.Error(c, http.StatusNotFound, "session not found")
			c.Abort()
			return
		}
		if err != nil {
			httpx.InternalError(c, "get session", err)
			c.Abort()
			return
		}
		c.Set(ctxKey, sess)
		c.Next()
	}
}

// From returns the session that Load put on the request.
func From(c *gin.Context) db.Session { return c.MustGet(ctxKey).(db.Session) }

func (h *Handler) create(c *gin.Context) {
	id, err := h.svc.Create(c.Request.Context())
	if err != nil {
		httpx.InternalError(c, "create session", err)
		return
	}
	httpx.JSON(c, http.StatusCreated, map[string]string{"id": id.String()})
}

func (h *Handler) get(c *gin.Context) {
	view, err := h.svc.View(c.Request.Context(), From(c))
	if err != nil {
		httpx.InternalError(c, "describe session", err)
		return
	}
	httpx.JSON(c, http.StatusOK, view)
}
