package dataset

import (
	"errors"
	"net/http"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/httpx"
	"dataanalyst/api/internal/session"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc            *Service
	sessions       *session.Service
	maxUploadBytes int64
}

// NewHandler takes the upload size limit in bytes (multipart overhead is added on top).
func NewHandler(svc *Service, sessions *session.Service, maxUploadBytes int64) *Handler {
	return &Handler{svc: svc, sessions: sessions, maxUploadBytes: maxUploadBytes}
}

// Register adds the routes under /sessions/:id (the session is already loaded).
func (h *Handler) Register(scoped *gin.RouterGroup) {
	scoped.POST("/datasets", h.upload)
}

func (h *Handler) upload(c *gin.Context) {
	sess := session.From(c)
	// Allow multipart overhead on top of the file itself.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxUploadBytes+1<<20)
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Error(c, http.StatusRequestEntityTooLarge, ErrTooLarge.Error())
			return
		}
		httpx.Error(c, http.StatusBadRequest, `expected a multipart form with a "file" field`)
		return
	}
	defer file.Close()

	ctx := c.Request.Context()
	table, err := h.svc.Upload(ctx, sess.ID, header.Filename, file)
	switch {
	case errors.Is(err, ErrTooLarge):
		httpx.Error(c, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, ErrUnsupported):
		httpx.Error(c, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, ErrEmpty), errors.Is(err, ErrBadFile):
		httpx.Error(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrBusy):
		httpx.Error(c, http.StatusConflict, err.Error())
	case err != nil:
		httpx.InternalError(c, "load dataset", err)
	default:
		_ = h.sessions.Touch(ctx, sess.ID)
		httpx.JSON(c, http.StatusCreated, map[string]agent.Table{"table": table})
	}
}
