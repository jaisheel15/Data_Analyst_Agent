// Package telemetry is the Go half of tracing: one trace per Run, shared with
// the Python agent service through W3C traceparent headers.
//
// Tracing is off unless TRACING_OTLP_ENDPOINT is set. Spans are created either
// way (they are cheap no-ops without an exporter), so callers never check.
package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// Project groups both services' spans in one Phoenix project.
	Project     = "data-analyst-agent"
	ServiceName = "analyst-api"
	// MaxPayload is the most bytes of any prompt, code or output kept on a span.
	MaxPayload = 2048
)

// OpenInference attribute names, so Phoenix shows inputs, outputs and kinds.
const (
	AttrKind   = "openinference.span.kind"
	AttrInput  = "input.value"
	AttrOutput = "output.value"
)

var hidePayloads atomic.Bool

// Start begins a span from the current global provider. It looks the provider
// up on every call (a package-level Tracer would stay bound to whichever
// provider was installed first), so tests can install their own recorder.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer("dataanalyst/api").Start(ctx, name, opts...)
}

// Setup installs the propagator and, when endpoint is set (a full OTLP/HTTP
// URL such as http://phoenix:6006/v1/traces), an exporting provider. The
// returned func flushes buffered spans; call it on shutdown.
func Setup(ctx context.Context, endpoint string, hide bool) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	hidePayloads.Store(hide)
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(Resource()),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Resource names this service and its Phoenix project.
func Resource() *resource.Resource {
	return resource.NewSchemaless(
		attribute.String("service.name", ServiceName),
		attribute.String("openinference.project.name", Project),
	)
}

// SetHidePayloads turns payload recording off (true) or on. For tests; Setup sets it in production.
func SetHidePayloads(hide bool) { hidePayloads.Store(hide) }

// Payload records a prompt, code or output on span, cut to MaxPayload bytes,
// unless TRACING_HIDE_PAYLOADS is on.
func Payload(span trace.Span, key, value string) {
	if hidePayloads.Load() || value == "" {
		return
	}
	span.SetAttributes(attribute.String(key, Truncate(value, MaxPayload)))
}

// Truncate keeps at most n bytes of s without splitting a UTF-8 character.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…[truncated]"
}

// Fail marks span as a fault (a Tool failure or a failed Run). The message
// can quote the agent's code or data (a stderr tail), so it is a payload too.
func Fail(span trace.Span, msg string) {
	if hidePayloads.Load() {
		msg = "failed (details hidden: TRACING_HIDE_PAYLOADS)"
	}
	span.SetStatus(codes.Error, Truncate(msg, MaxPayload))
}

// Continue is Gin middleware for the internal API: it picks up the caller's
// trace from the traceparent header and wraps the request in a server span.
func Continue() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}
		ctx, span := Start(ctx, c.Request.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", route)))
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		status := c.Writer.Status()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, strings.TrimSpace(c.Errors.String()))
		}
	}
}

// LogHandler adds trace_id and span_id to every record logged with a context
// that carries a span, so a log line leads to its trace.
type LogHandler struct{ slog.Handler }

func (h LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{h.Handler.WithAttrs(attrs)}
}

func (h LogHandler) WithGroup(name string) slog.Handler { return LogHandler{h.Handler.WithGroup(name)} }
