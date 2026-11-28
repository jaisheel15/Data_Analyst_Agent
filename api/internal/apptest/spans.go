package apptest

import (
	"testing"

	"dataanalyst/api/internal/platform/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Spans records every span the app makes during one test.
type Spans struct {
	t   *testing.T
	rec *tracetest.SpanRecorder
}

// RecordSpans installs an in-memory tracer provider for this test. Call it
// before New, so the agent client picks it up.
func RecordSpans(t *testing.T) *Spans {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec),
		sdktrace.WithResource(telemetry.Resource())))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	telemetry.SetHidePayloads(false)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		telemetry.SetHidePayloads(false)
	})
	return &Spans{t: t, rec: rec}
}

// Ended returns the finished spans, oldest first.
func (s *Spans) Ended() []sdktrace.ReadOnlySpan { return s.rec.Ended() }

// Named returns the one finished span called name.
func (s *Spans) Named(name string) sdktrace.ReadOnlySpan {
	s.t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, sp := range s.rec.Ended() {
		if sp.Name() == name {
			found = append(found, sp)
		}
	}
	if len(found) != 1 {
		names := []string{}
		for _, sp := range s.rec.Ended() {
			names = append(names, sp.Name())
		}
		s.t.Fatalf("want one %q span, got %d; spans: %v", name, len(found), names)
	}
	return found[0]
}

// Attr returns a span attribute's value, or an empty Value if it is missing.
func Attr(sp sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range sp.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// HasAttr reports whether a span carries key at all.
func HasAttr(sp sdktrace.ReadOnlySpan, key string) bool {
	for _, kv := range sp.Attributes() {
		if string(kv.Key) == key {
			return true
		}
	}
	return false
}
