package chat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/httpx"
	"dataanalyst/api/internal/platform/telemetry"
	"dataanalyst/api/internal/run"
	"dataanalyst/api/internal/session"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// AgentRunner starts a run on the agent service and streams its events.
type AgentRunner interface {
	Run(ctx context.Context, req agent.RunRequest, emit func(agent.Event) error) error
}

type Handler struct {
	svc       *Service
	agent     AgentRunner
	runs      Runs
	evalToken string
	now       func() time.Time
}

func NewHandler(svc *Service, agent AgentRunner, runs Runs, evalToken string, now func() time.Time) *Handler {
	return &Handler{svc: svc, agent: agent, runs: runs, evalToken: evalToken, now: now}
}

// Register adds the routes under /sessions/:id (the session is already loaded).
func (h *Handler) Register(scoped *gin.RouterGroup) {
	scoped.POST("/messages", h.post)
}

type askBody struct {
	Question string `json:"question"`
	// Provider and Model override the configured model. Honoured only for
	// eval requests, so the eval harness can compare providers.
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// post runs the agent on a question and streams its work as SSE.
func (h *Handler) post(c *gin.Context) {
	sess := session.From(c)
	var body askBody
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)).Decode(&body); err != nil {
		httpx.Error(c, http.StatusBadRequest, `expected JSON {"question": "..."}`)
		return
	}
	r, req, err := h.svc.Start(c.Request.Context(), Question{
		SessionID: sess.ID, Text: body.Question, Eval: h.isEval(c),
		Provider: body.Provider, Model: body.Model,
	})
	if err != nil {
		h.startError(c, err)
		return
	}
	h.stream(c, r, req)
}

func (h *Handler) startError(c *gin.Context, err error) {
	var unpriced ModelNotPricedError
	switch {
	case errors.Is(err, ErrBadQuestion):
		httpx.Error(c, http.StatusBadRequest, err.Error())
	case errors.As(err, &unpriced):
		slog.Error("refusing run: model has no price", "model", unpriced.Model)
		httpx.Error(c, http.StatusInternalServerError, err.Error())
	case errors.Is(err, ErrOverBudget):
		httpx.Error(c, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ErrNoCapacity):
		httpx.Error(c, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrRunInProgress):
		httpx.Error(c, http.StatusConflict, err.Error())
	default:
		httpx.InternalError(c, "start question", err)
	}
}

// stream relays agent events to the browser and records how the run ended.
// The whole Run is one trace, rooted at the chat.run span started here.
func (h *Handler) stream(c *gin.Context, r db.AgentRun, req agent.RunRequest) {
	ctx, span := startRunSpan(c.Request.Context(), r, req)
	defer span.End()
	sse := httpx.StartSSE(c)
	start := h.now()
	finished := false
	var lastUsage *agent.Usage // charged even if the run never reaches done
	err := h.agent.Run(ctx, req, func(ev agent.Event) error {
		switch ev.Type {
		case agent.EventUsage:
			lastUsage = ev.Usage
		case agent.EventStepStart:
			sse.Send(ev.Type, map[string]any{"step": ev.Step})
		case agent.EventToolCall:
			sse.Send(ev.Type, map[string]any{"step": ev.Step, "tool": ev.Tool, "input": ev.Input})
		case agent.EventToolResult:
			sse.Send(ev.Type, map[string]any{"step": ev.Step, "tool": ev.Tool, "summary": ev.Summary,
				"error": ev.Error, "chart_ids": orEmpty(ev.ChartIDs)})
			for _, id := range ev.ChartIDs {
				sse.Send("chart", map[string]string{"id": id, "url": "/api/charts/" + id})
			}
		case agent.EventAnswerDelta:
			sse.Send(ev.Type, map[string]string{"text": ev.Text})
		case agent.EventAnswerReset:
			sse.Send(ev.Type, map[string]string{})
		case agent.EventDone:
			finished = true
			status := run.StatusDone
			if ev.Status == run.StatusBudgetExhausted {
				status = run.StatusBudgetExhausted
			}
			o := run.Outcome{Status: status, Answer: ev.Answer, Usage: ev.Usage, Latency: h.now().Sub(start)}
			cost, msgID := h.runs.Finish(ctx, r, o)
			endRunSpan(span, o, cost)
			sse.Send(ev.Type, map[string]any{"answer": ev.Answer, "status": status, "cost_usd": cost,
				"steps": stepsOf(ev.Usage), "run_id": r.ID.String(), "message_id": msgID})
		case agent.EventError:
			finished = true
			usage := ev.Usage
			if usage == nil {
				usage = lastUsage
			}
			o := run.Outcome{Status: run.StatusError, ErrText: ev.Error, Usage: usage, Latency: h.now().Sub(start)}
			cost, _ := h.runs.Finish(ctx, r, o)
			endRunSpan(span, o, cost)
			sse.Send(ev.Type, map[string]string{"error": ev.Error})
		}
		return nil
	})
	switch {
	case finished:
	case ctx.Err() != nil:
		// The browser went away; the agent service saw the stream close.
		o := run.Outcome{Status: run.StatusCancelled, Usage: lastUsage, Latency: h.now().Sub(start)}
		cost, _ := h.runs.Finish(ctx, r, o)
		endRunSpan(span, o, cost)
	default:
		slog.ErrorContext(ctx, "agent run failed", "run", r.ID, "err", err)
		o := run.Outcome{Status: run.StatusError, ErrText: errText(err), Usage: lastUsage, Latency: h.now().Sub(start)}
		cost, _ := h.runs.Finish(ctx, r, o)
		endRunSpan(span, o, cost)
		sse.Send(agent.EventError, map[string]string{"error": "the agent stopped unexpectedly; please try again"})
	}
}

func startRunSpan(ctx context.Context, r db.AgentRun, req agent.RunRequest) (context.Context, trace.Span) {
	ctx, span := telemetry.Start(ctx, "chat.run", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String(telemetry.AttrKind, "AGENT"),
			attribute.String("run.id", r.ID.String()),
			attribute.String("session.id", r.SessionID.String()),
			attribute.String("run.purpose", r.Purpose),
			attribute.String("run.provider", req.Model.Provider),
			attribute.String("run.model", req.Model.Name),
		))
	telemetry.Payload(span, telemetry.AttrInput, req.Question)
	return ctx, span
}

// endRunSpan records how the Run ended. Only a failed Run is an error:
// budget_exhausted and cancelled are expected endings.
func endRunSpan(span trace.Span, o run.Outcome, cost float64) {
	span.SetAttributes(attribute.String("run.status", o.Status), attribute.Float64("run.cost_usd", cost))
	if u := o.Usage; u != nil {
		prompt := u.InputTokens + u.CachedTokens + u.CacheWriteTokens // InputTokens is the uncached part only
		span.SetAttributes(
			attribute.Int("run.steps", u.Steps),
			// run.* rather than OpenInference's llm.token_count.*: Phoenix already
			// sums those from the LLM spans below, and would count them twice.
			attribute.Int64("run.token_count.prompt", prompt),
			attribute.Int64("run.token_count.completion", u.OutputTokens),
			attribute.Int64("run.token_count.total", prompt+u.OutputTokens),
			attribute.Int64("run.token_count.cache_read", u.CachedTokens),
			attribute.Int64("run.token_count.cache_write", u.CacheWriteTokens),
		)
	}
	telemetry.Payload(span, telemetry.AttrOutput, o.Answer)
	if o.Status == run.StatusError {
		telemetry.Fail(span, o.ErrText)
	}
}

func (h *Handler) isEval(c *gin.Context) bool {
	want := []byte(h.evalToken)
	got := []byte(c.GetHeader("X-Eval-Token"))
	return len(want) > 0 && subtle.ConstantTimeCompare(got, want) == 1
}

func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func stepsOf(u *agent.Usage) int {
	if u == nil {
		return 0
	}
	return u.Steps
}

func errText(err error) string {
	if err == nil {
		return "agent stream ended unexpectedly"
	}
	return err.Error()
}
