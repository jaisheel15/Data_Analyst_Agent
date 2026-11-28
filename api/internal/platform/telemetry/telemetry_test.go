package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"dataanalyst/api/internal/platform/telemetry"

	"go.opentelemetry.io/otel/trace"
)

func TestLogLinesInsideASpanCarryItsTraceAndSpanIDs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(telemetry.LogHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))

	log.With("run", "r1").InfoContext(ctx, "hello")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != tid.String() || line["span_id"] != sid.String() || line["run"] != "r1" {
		t.Fatalf("log line = %v", line)
	}

	buf.Reset()
	log.InfoContext(context.Background(), "no span")
	if bytes.Contains(buf.Bytes(), []byte("trace_id")) {
		t.Errorf("a line outside any span got a trace_id: %s", buf.String())
	}
}

func TestTruncateNeverSplitsACharacter(t *testing.T) {
	got := telemetry.Truncate("aé", 2) // é is 2 bytes; cutting at 2 would split it
	if got != "a…[truncated]" {
		t.Fatalf("got %q", got)
	}
}
