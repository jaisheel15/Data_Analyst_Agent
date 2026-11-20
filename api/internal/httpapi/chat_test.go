package httpapi_test

// Seam 7: POST /api/sessions/{id}/messages (SSE), with a fake agent service.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/httpapi"
	"dataanalyst/api/internal/sandbox"
)

type sse struct {
	Type string
	Data map[string]any
}

// ask posts a question and reads the whole SSE stream.
func (h *harness) ask(sid, question string, headers ...string) (*http.Response, []sse) {
	h.t.Helper()
	return h.askCtx(context.Background(), sid, question, headers...)
}

func (h *harness) askCtx(ctx context.Context, sid, question string, headers ...string) (*http.Response, []sse) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{"question": question})
	req, _ := http.NewRequestWithContext(ctx, "POST", h.public.URL+"/api/sessions/"+sid+"/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []sse
	sc := bufio.NewScanner(resp.Body)
	var cur sse
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
		case line == "" && cur.Type != "":
			events = append(events, cur)
			cur = sse{}
		}
	}
	return resp, events
}

func types(events []sse) string {
	var ts []string
	for _, e := range events {
		ts = append(ts, e.Type)
	}
	return strings.Join(ts, ",")
}

func TestAgentEventsAreRelayedInOrderWithChartsAndCost(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	h.agent.setRun(func(_ context.Context, req contract.RunRequest, emit func(contract.Event)) {
		emit(contract.Event{Type: contract.EventStepStart, Step: 1})
		emit(contract.Event{Type: contract.EventToolCall, Step: 1, Tool: "run_python", Input: json.RawMessage(`{"code":"plot"}`)})
		emit(contract.Event{Type: contract.EventToolResult, Step: 1, Tool: "run_python", Summary: "chart", ChartIDs: []string{"c1"}})
		emit(contract.Event{Type: contract.EventAnswerDelta, Text: "North "})
		emit(contract.Event{Type: contract.EventAnswerDelta, Text: "leads."})
		emit(doneEvent("North leads."))
	})
	resp, events := h.ask(sid, "which region leads?")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got := types(events); got != "step_start,tool_call,tool_result,chart,answer_delta,answer_delta,done" {
		t.Fatalf("events = %s", got)
	}
	if events[3].Data["url"] != "/api/charts/c1" {
		t.Errorf("chart event = %v", events[3].Data)
	}
	done := events[6].Data
	if done["answer"] != "North leads." || done["status"] != "done" {
		t.Errorf("done = %v", done)
	}
	if c, _ := done["cost_usd"].(float64); c < 0.149 || c > 0.151 {
		t.Errorf("cost_usd = %v, want 0.15", done["cost_usd"])
	}
}

func TestQuestionAndAnswerAreSavedOnTheSession(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	h.ask(sid, "how many rows?")
	_, raw := h.do("GET", "/api/sessions/"+sid, nil, "")
	var view httpapi.SessionView
	mustJSON(t, raw, &view)
	if len(view.Messages) != 2 || view.Messages[0].Content != "how many rows?" ||
		view.Messages[1].Role != "assistant" || view.Messages[1].Content != "fine" {
		t.Fatalf("messages = %+v", view.Messages)
	}
}

func TestRunRequestCarriesSchemaModelAndLimits(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	h.mustUpload(sid, "sales.csv", []byte(salesCSV))
	h.ask(sid, "total?")
	req := h.agent.lastRequest(t)
	if req.SessionID != sid || req.Question != "total?" || req.RunID == "" {
		t.Fatalf("request = %+v", req)
	}
	if len(req.Schema) != 1 || req.Schema[0].Name != "sales" || len(req.Schema[0].SampleRows) != 3 {
		t.Fatalf("schema = %+v", req.Schema)
	}
	if req.Model != (contract.Model{Provider: "anthropic", Name: "test-model"}) {
		t.Errorf("model = %+v", req.Model)
	}
	if req.Limits.MaxSteps != 10 || req.Limits.MaxTokens != 4096 {
		t.Errorf("limits = %+v", req.Limits)
	}
	if req.History == nil || len(req.History) != 0 {
		t.Errorf("first question should have empty history, got %+v", req.History)
	}
}

func TestFollowUpGetsThePreviousTurnWithItsToolStepsInOrder(t *testing.T) {
	h := newHarness(t, nil)
	h.sandbox.result = func(_ string, req sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Columns: []string{"x"}, Rows: [][]any{{req.Code}}, TotalRows: 1}, nil
	}
	sid := h.newSession()
	h.agent.setRun(func(_ context.Context, req contract.RunRequest, emit func(contract.Event)) {
		for _, sql := range []string{"select 'first'", "select 'second'"} {
			resp, _ := h.callTool("run_sql", contract.ToolRequest{RunID: req.RunID, SessionID: req.SessionID,
				Input: json.RawMessage(`{"sql":"` + sql + `"}`)}, internalToken)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("tool call: %d", resp.StatusCode)
			}
		}
		emit(doneEvent("two queries"))
	})
	h.ask(sid, "first question")
	h.agent.setRun(nil)
	h.ask(sid, "now by region")

	hist := h.agent.lastRequest(t).History
	if len(hist) != 1 || hist[0].Question != "first question" || hist[0].Answer != "two queries" {
		t.Fatalf("history = %+v", hist)
	}
	steps := hist[0].Steps
	if len(steps) != 2 || !strings.Contains(string(steps[0].Input), "first") ||
		!strings.Contains(string(steps[1].Input), "second") || !strings.Contains(steps[1].Output, "second") {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestSecondQuestionWhileOneIsRunningGets409(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	release := make(chan struct{})
	started := make(chan struct{})
	h.agent.setRun(func(_ context.Context, _ contract.RunRequest, emit func(contract.Event)) {
		close(started)
		<-release
		emit(doneEvent("done"))
	})
	go h.ask(sid, "slow question")
	<-started
	resp, _ := h.ask(sid, "impatient question")
	close(release)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestNoFreeSandboxGets503(t *testing.T) {
	h := newHarness(t, nil)
	h.sandbox.full = true
	resp, _ := h.ask(h.newSession(), "q")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestDailyBudgetStopsChatButNotEvals(t *testing.T) {
	h := newHarness(t, func(c *httpapi.Config) { c.DailyChatBudgetUSD = 0.20 })
	sid := h.newSession()
	h.ask(sid, "one") // $0.15
	h.ask(sid, "two") // $0.30 total: over the cap
	resp, _ := h.ask(sid, "three")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp, _ := h.ask(sid, "eval question", "X-Eval-Token", "test-eval-token"); resp.StatusCode != http.StatusOK {
		t.Fatalf("eval status = %d, want 200", resp.StatusCode)
	}
	// The next UTC day the cap resets.
	h.now = h.now.Add(24 * time.Hour)
	if resp, _ := h.ask(sid, "tomorrow"); resp.StatusCode != http.StatusOK {
		t.Fatalf("next day status = %d, want 200", resp.StatusCode)
	}
}

func TestEvalRunsDoNotCountTowardsTheChatBudget(t *testing.T) {
	h := newHarness(t, func(c *httpapi.Config) { c.DailyChatBudgetUSD = 0.20 })
	sid := h.newSession()
	for range 3 {
		h.ask(sid, "eval", "X-Eval-Token", "test-eval-token")
	}
	if resp, _ := h.ask(sid, "chat"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDisconnectCancelsTheRunAndFreesTheSession(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	agentStopped := make(chan struct{})
	started := make(chan struct{})
	h.agent.setRun(func(ctx context.Context, _ contract.RunRequest, emit func(contract.Event)) {
		emit(contract.Event{Type: contract.EventStepStart, Step: 1})
		close(started)
		<-ctx.Done() // the agent service sees Go hang up
		close(agentStopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go h.askCtx(ctx, sid, "long question")
	<-started
	cancel()
	select {
	case <-agentStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("agent service was not told to stop")
	}
	h.agent.setRun(nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := h.ask(sid, "next question")
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still busy after disconnect: %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAgentFailureBecomesAnErrorEventAndFreesTheSession(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	h.agent.setRun(func(_ context.Context, _ contract.RunRequest, emit func(contract.Event)) {
		emit(contract.Event{Type: contract.EventError, Error: "model provider unavailable"})
	})
	_, events := h.ask(sid, "q")
	if types(events) != "error" || events[0].Data["error"] != "model provider unavailable" {
		t.Fatalf("events = %+v", events)
	}
	h.agent.setRun(func(context.Context, contract.RunRequest, func(contract.Event)) {}) // stream ends with nothing
	_, events = h.ask(sid, "q2")
	if types(events) != "error" {
		t.Fatalf("incomplete stream should end in an error event, got %s", types(events))
	}
	h.agent.setRun(nil)
	if resp, _ := h.ask(sid, "q3"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestModelWithoutAPriceIsRefusedBeforeCallingTheAgent(t *testing.T) {
	h := newHarness(t, func(c *httpapi.Config) { c.Model = "unpriced-model" })
	resp, _ := h.ask(h.newSession(), "q")
	if resp.StatusCode != http.StatusInternalServerError || h.agent.calls() != 0 {
		t.Fatalf("status = %d, agent calls = %d", resp.StatusCode, h.agent.calls())
	}
}

func TestEmptyQuestionIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	if resp, _ := h.ask(h.newSession(), "   "); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

var (
	_ = os.Stat
	_ = filepath.Join
)

func TestDisconnectingStillChargesTheUsageReportedSoFar(t *testing.T) {
	h := newHarness(t, func(c *httpapi.Config) { c.DailyChatBudgetUSD = 0.10 })
	sid := h.newSession()
	started := make(chan struct{})
	stopped := make(chan struct{})
	h.agent.setRun(func(ctx context.Context, _ contract.RunRequest, emit func(contract.Event)) {
		emit(contract.Event{Type: "usage", Usage: &contract.Usage{Model: "test-model", InputTokens: 2000}}) // $0.20
		close(started)
		<-ctx.Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go h.askCtx(ctx, sid, "expensive question, then close the tab")
	<-started
	cancel()
	<-stopped
	h.agent.setRun(nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := h.ask(sid, "next")
		if resp.StatusCode == http.StatusTooManyRequests {
			return // the cancelled run's $0.20 counted against the $0.10 cap
		}
		if resp.StatusCode == http.StatusOK || time.Now().After(deadline) {
			t.Fatalf("status = %d: the disconnected run's spend was not counted", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond) // 409 while the cancelled run is being finished
	}
}

func TestUsageEventsAreNotSentToTheBrowser(t *testing.T) {
	h := newHarness(t, nil)
	h.agent.setRun(func(_ context.Context, _ contract.RunRequest, emit func(contract.Event)) {
		emit(contract.Event{Type: "usage", Usage: &contract.Usage{Model: "test-model", InputTokens: 10}})
		emit(doneEvent("ok"))
	})
	_, events := h.ask(h.newSession(), "q")
	if types(events) != "done" {
		t.Fatalf("events = %s", types(events))
	}
}
