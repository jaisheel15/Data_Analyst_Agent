package chat_test

// Tracing: one Run is one trace, rooted at chat.run, and the agent service joins it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/apptest"

	"go.opentelemetry.io/otel/codes"
)

func TestARunIsOneTraceRootedAtChatRunAndTheAgentJoinsIt(t *testing.T) {
	spans := apptest.RecordSpans(t)
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.Ask(sid, "which region leads?")

	root := spans.Named("chat.run")
	if root.Parent().IsValid() {
		t.Errorf("chat.run should be the root, has parent %v", root.Parent().SpanID())
	}
	req := h.Agent.LastRequest(t)
	for key, want := range map[string]string{
		"run.id": req.RunID, "session.id": sid, "run.purpose": "chat", "run.status": "done",
		"run.model": "test-model", "input.value": "which region leads?", "output.value": "fine",
		"openinference.span.kind": "AGENT",
	} {
		if got := apptest.Attr(root, key).Emit(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if c := apptest.Attr(root, "run.cost_usd").AsFloat64(); c < 0.149 || c > 0.151 {
		t.Errorf("run.cost_usd = %v, want 0.15", c)
	}
	if n := apptest.Attr(root, "run.token_count.total").AsInt64(); n != 1100 {
		t.Errorf("total tokens = %d, want 1100", n)
	}
	if root.Status().Code == codes.Error {
		t.Errorf("a done Run is not an error: %v", root.Status())
	}

	// The agent service was told to continue this trace.
	tp := h.Agent.LastHeaders(t).Get("traceparent")
	if !strings.Contains(tp, root.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q does not carry trace %s", tp, root.SpanContext().TraceID())
	}
}

func TestOnlyAFailedRunMarksItsTraceAsAnError(t *testing.T) {
	cases := map[string]struct {
		run       func(ctx context.Context, emit func(agent.Event))
		status    string
		wantError bool
	}{
		"budget exhausted": {func(_ context.Context, emit func(agent.Event)) {
			ev := apptest.DoneEvent("best effort")
			ev.Status = "budget_exhausted"
			emit(ev)
		}, "budget_exhausted", false},
		"agent error": {func(_ context.Context, emit func(agent.Event)) {
			emit(agent.Event{Type: agent.EventError, Error: "model exploded"})
		}, "error", true},
		"stream cut": {func(context.Context, func(agent.Event)) {}, "error", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spans := apptest.RecordSpans(t)
			h := apptest.New(t, nil)
			h.Agent.SetRun(func(ctx context.Context, _ agent.RunRequest, emit func(agent.Event)) { tc.run(ctx, emit) })
			h.Ask(h.NewSession(), "q?")
			root := spans.Named("chat.run")
			if got := apptest.Attr(root, "run.status").AsString(); got != tc.status {
				t.Errorf("run.status = %q, want %q", got, tc.status)
			}
			if isErr := root.Status().Code == codes.Error; isErr != tc.wantError {
				t.Errorf("error status = %v, want %v (%v)", isErr, tc.wantError, root.Status())
			}
		})
	}
}

func TestACancelledRunIsNotAnError(t *testing.T) {
	spans := apptest.RecordSpans(t)
	h := apptest.New(t, nil)
	started := make(chan struct{})
	h.Agent.SetRun(func(ctx context.Context, _ agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: agent.EventStepStart, Step: 1})
		close(started)
		<-ctx.Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	h.AskCtx(ctx, h.NewSession(), "q?")

	deadline := time.Now().Add(5 * time.Second)
	for !hasSpan(spans, "chat.run") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	root := spans.Named("chat.run")
	if got := apptest.Attr(root, "run.status").AsString(); got != "cancelled" {
		t.Errorf("run.status = %q, want cancelled", got)
	}
	if root.Status().Code == codes.Error {
		t.Errorf("a cancelled Run is not an error: %v", root.Status())
	}
}

func TestHealthChecksStayOutOfTraces(t *testing.T) {
	spans := apptest.RecordSpans(t)
	h := apptest.New(t, nil)
	h.Do("GET", "/api/health", nil, "")
	if n := len(spans.Ended()); n != 0 {
		t.Fatalf("health check made %d spans", n)
	}
}

func hasSpan(s *apptest.Spans, name string) bool {
	for _, sp := range s.Ended() {
		if sp.Name() == name {
			return true
		}
	}
	return false
}
