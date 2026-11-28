// Package httpx holds the small Gin helpers every domain handler shares:
// JSON responses, the {"error": "..."} body, the internal-token middleware
// and a server-sent-events writer.
package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// JSON writes v as application/json.
func JSON(c *gin.Context, status int, v any) {
	c.Header("Content-Type", "application/json")
	c.Status(status)
	_ = json.NewEncoder(c.Writer).Encode(v)
}

// Error writes the standard {"error": msg} body.
func Error(c *gin.Context, status int, msg string) {
	JSON(c, status, map[string]string{"error": msg})
}

// InternalError logs err and answers 500 without leaking details.
func InternalError(c *gin.Context, what string, err error) {
	slog.ErrorContext(c.Request.Context(), what, "err", err)
	Error(c, http.StatusInternalServerError, "internal error")
}

// RequireToken rejects requests without the shared X-Internal-Token.
func RequireToken(token string) gin.HandlerFunc {
	want := []byte(token)
	return func(c *gin.Context) {
		got := []byte(c.GetHeader("X-Internal-Token"))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			Error(c, http.StatusUnauthorized, "invalid internal token")
			c.Abort()
			return
		}
		c.Next()
	}
}

// SSE is a server-sent-events stream on a Gin response.
type SSE struct{ c *gin.Context }

// StartSSE sends the event-stream headers and returns the writer.
func StartSSE(c *gin.Context) *SSE {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()
	return &SSE{c: c}
}

// Send writes one event and flushes it to the client.
func (s *SSE) Send(event string, data any) {
	raw, _ := json.Marshal(data)
	fmt.Fprintf(s.c.Writer, "event: %s\ndata: %s\n\n", event, raw)
	s.c.Writer.Flush()
}
