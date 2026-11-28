package tool_test

// Tracing: a tool call continues the agent's trace, and only a Tool failure is an error.

import (
	"net/http"
	"strings"
	"testing"

	"dataanalyst/api/internal/apptest"
	"dataanalyst/api/internal/platform/telemetry"
	"dataanalyst/api/internal/sandbox"

	"go.opentelemetry.io/otel/codes"
)

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
)

func TestAToolCallContinuesTheAgentsTrace(t *testing.T) {
	spans := apptest.RecordSpans(t)
	h := apptest.New(t, nil)
	sid := h.NewSession()
	run := h.StartRun(sid)
	resp, _ := h.CallTool("run_sql", toolReq(sid, run, `{"sql":"select 1"}`), apptest.InternalToken,
		"traceparent", "00-"+callerTrace+"-"+callerSpan+"-01")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	server := spans.Named("POST /internal/tools/:tool")
	if server.Parent().SpanID().String() != callerSpan || !server.Parent().IsRemote() {
		t.Errorf("server span parent = %v, want the agent's span %s", server.Parent().SpanID(), callerSpan)
	}
	tool := spans.Named("tool.run_sql")
	if tool.SpanContext().TraceID().String() != callerTrace {
		t.Errorf("tool span is in trace %s, want %s", tool.SpanContext().TraceID(), callerTrace)
	}
	if tool.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("tool span is not a child of the server span")
	}
	if got := apptest.Attr(tool, "run.id").AsString(); got != run {
		t.Errorf("run.id = %q, want %q", got, run)
	}
	if got := apptest.Attr(tool, "input.value").AsString(); !strings.Contains(got, "select 1") {
		t.Errorf("input.value = %q", got)
	}
}

func TestToolErrorsAreNotSpanErrorsButToolFailuresAre(t *testing.T) {
	cases := map[string]struct {
		result    sandbox.Result
		err       error
		wantError bool
	}{
		"tool error: bad code":     {sandbox.Result{Error: "KeyError: 'x'"}, nil, false},
		"tool failure: timeout":    {sandbox.Result{Error: "timed out", TimedOut: true}, nil, true},
		"tool failure: oom":        {sandbox.Result{Error: "killed", Fault: true}, nil, true},
		"tool failure: no sandbox": {sandbox.Result{}, sandbox.ErrAtCapacity, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spans := apptest.RecordSpans(t)
			h := apptest.New(t, nil)
			h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) { return tc.result, tc.err }
			sid := h.NewSession()
			_, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"1"}`), apptest.InternalToken)
			if out.Error == "" {
				t.Fatal("the model should see an error either way")
			}
			sp := spans.Named("tool.run_python")
			if isErr := sp.Status().Code == codes.Error; isErr != tc.wantError {
				t.Errorf("span error = %v, want %v", isErr, tc.wantError)
			}
			if apptest.Attr(sp, "tool.error").AsString() == "" {
				t.Errorf("tool.error not recorded")
			}
			if got := apptest.Attr(sp, "tool.failed").AsBool(); got != tc.wantError {
				t.Errorf("tool.failed = %v, want %v", got, tc.wantError)
			}
		})
	}
}

func TestPayloadsAreTruncatedAndCanBeHidden(t *testing.T) {
	spans := apptest.RecordSpans(t)
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Stdout: strings.Repeat("x", 6000)}, nil
	}
	sid := h.NewSession()
	run := h.StartRun(sid)
	h.CallTool("run_python", toolReq(sid, run, `{"code":"print('x'*6000)"}`), apptest.InternalToken)
	if n := len(apptest.Attr(spans.Named("tool.run_python"), "output.value").AsString()); n == 0 || n > telemetry.MaxPayload+20 {
		t.Errorf("output.value is %d bytes, want 1..%d", n, telemetry.MaxPayload)
	}

	hidden := apptest.RecordSpans(t)
	telemetry.SetHidePayloads(true)
	h.CallTool("run_python", toolReq(sid, run, `{"code":"secret"}`), apptest.InternalToken)
	sp := hidden.Named("tool.run_python")
	for _, key := range []string{"input.value", "output.value"} {
		if apptest.HasAttr(sp, key) {
			t.Errorf("%s recorded although payloads are hidden", key)
		}
	}
}
