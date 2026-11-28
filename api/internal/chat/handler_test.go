package chat_test

// Seam 7: POST /api/sessions/{id}/messages (SSE), with a fake agent service.

import (
	"context"
	"dataanalyst/api/internal/apptest"
	"dataanalyst/api/internal/platform/config"
	"dataanalyst/api/internal/session"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/sandbox"
)

func TestAgentEventsAreRelayedInOrderWithChartsAndCost(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.Agent.SetRun(func(_ context.Context, req agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: agent.EventStepStart, Step: 1})
		emit(agent.Event{Type: agent.EventToolCall, Step: 1, Tool: "run_python", Input: json.RawMessage(`{"code":"plot"}`)})
		emit(agent.Event{Type: agent.EventToolResult, Step: 1, Tool: "run_python", Summary: "chart", ChartIDs: []string{"c1"}})
		emit(agent.Event{Type: agent.EventAnswerDelta, Text: "North "})
		emit(agent.Event{Type: agent.EventAnswerDelta, Text: "leads."})
		emit(apptest.DoneEvent("North leads."))
	})
	resp, events := h.Ask(sid, "which region leads?")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got := apptest.Types(events); got != "step_start,tool_call,tool_result,chart,answer_delta,answer_delta,done" {
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
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.Ask(sid, "how many rows?")
	_, raw := h.Do("GET", "/api/sessions/"+sid, nil, "")
	var view session.View
	apptest.MustJSON(t, raw, &view)
	if len(view.Messages) != 2 || view.Messages[0].Content != "how many rows?" ||
		view.Messages[1].Role != "assistant" || view.Messages[1].Content != "fine" {
		t.Fatalf("messages = %+v", view.Messages)
	}
}

func TestRunRequestCarriesSchemaModelAndLimits(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.MustUpload(sid, "sales.csv", []byte(apptest.SalesCSV))
	h.Ask(sid, "total?")
	req := h.Agent.LastRequest(t)
	if req.SessionID != sid || req.Question != "total?" || req.RunID == "" {
		t.Fatalf("request = %+v", req)
	}
	if len(req.Schema) != 1 || req.Schema[0].Name != "sales" || len(req.Schema[0].SampleRows) != 3 {
		t.Fatalf("schema = %+v", req.Schema)
	}
	if req.Model != (agent.Model{Provider: "anthropic", Name: "test-model"}) {
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
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(_ string, req sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Columns: []string{"x"}, Rows: [][]any{{req.Code}}, TotalRows: 1}, nil
	}
	sid := h.NewSession()
	h.Agent.SetRun(func(_ context.Context, req agent.RunRequest, emit func(agent.Event)) {
		for _, sql := range []string{"select 'first'", "select 'second'"} {
			resp, _ := h.CallTool("run_sql", agent.ToolRequest{RunID: req.RunID, SessionID: req.SessionID,
				Input: json.RawMessage(`{"sql":"` + sql + `"}`)}, apptest.InternalToken)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("tool call: %d", resp.StatusCode)
			}
		}
		emit(apptest.DoneEvent("two queries"))
	})
	h.Ask(sid, "first question")
	h.Agent.SetRun(nil)
	h.Ask(sid, "now by region")

	hist := h.Agent.LastRequest(t).History
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
	h := apptest.New(t, nil)
	sid := h.NewSession()
	release := make(chan struct{})
	started := make(chan struct{})
	h.Agent.SetRun(func(_ context.Context, _ agent.RunRequest, emit func(agent.Event)) {
		close(started)
		<-release
		emit(apptest.DoneEvent("done"))
	})
	go h.Ask(sid, "slow question")
	<-started
	resp, _ := h.Ask(sid, "impatient question")
	close(release)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestNoFreeSandboxGets503(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Full = true
	resp, _ := h.Ask(h.NewSession(), "q")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestDailyBudgetStopsChatButNotEvals(t *testing.T) {
	h := apptest.New(t, func(c *config.Config) { c.DailyChatBudgetUSD = 0.20 })
	sid := h.NewSession()
	h.Ask(sid, "one") // $0.15
	h.Ask(sid, "two") // $0.30 total: over the cap
	resp, _ := h.Ask(sid, "three")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp, _ := h.Ask(sid, "eval question", "X-Eval-Token", "test-eval-token"); resp.StatusCode != http.StatusOK {
		t.Fatalf("eval status = %d, want 200", resp.StatusCode)
	}
	// The next UTC day the cap resets.
	h.Now = h.Now.Add(24 * time.Hour)
	if resp, _ := h.Ask(sid, "tomorrow"); resp.StatusCode != http.StatusOK {
		t.Fatalf("next day status = %d, want 200", resp.StatusCode)
	}
}

func TestWithdrawnPreambleIsRelayedSoTheBrowserCanClearIt(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.Agent.SetRun(func(_ context.Context, req agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: agent.EventAnswerDelta, Text: "Let me look. "})
		emit(agent.Event{Type: agent.EventAnswerReset})
		emit(agent.Event{Type: agent.EventAnswerDelta, Text: "Done."})
		emit(apptest.DoneEvent("Done."))
	})
	_, events := h.Ask(sid, "q")
	if got := apptest.Types(events); got != "answer_delta,answer_reset,answer_delta,done" {
		t.Fatalf("events = %s", got)
	}
}

func TestEvalRunsDoNotCountTowardsTheChatBudget(t *testing.T) {
	h := apptest.New(t, func(c *config.Config) { c.DailyChatBudgetUSD = 0.20 })
	sid := h.NewSession()
	for range 3 {
		h.Ask(sid, "eval", "X-Eval-Token", "test-eval-token")
	}
	if resp, _ := h.Ask(sid, "chat"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDisconnectCancelsTheRunAndFreesTheSession(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	agentStopped := make(chan struct{})
	started := make(chan struct{})
	h.Agent.SetRun(func(ctx context.Context, _ agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: agent.EventStepStart, Step: 1})
		close(started)
		<-ctx.Done() // the agent service sees Go hang up
		close(agentStopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go h.AskCtx(ctx, sid, "long question")
	<-started
	cancel()
	select {
	case <-agentStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("agent service was not told to stop")
	}
	h.Agent.SetRun(nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := h.Ask(sid, "next question")
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
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.Agent.SetRun(func(_ context.Context, _ agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: agent.EventError, Error: "model provider unavailable"})
	})
	_, events := h.Ask(sid, "q")
	if apptest.Types(events) != "error" || events[0].Data["error"] != "model provider unavailable" {
		t.Fatalf("events = %+v", events)
	}
	h.Agent.SetRun(func(context.Context, agent.RunRequest, func(agent.Event)) {}) // stream ends with nothing
	_, events = h.Ask(sid, "q2")
	if apptest.Types(events) != "error" {
		t.Fatalf("incomplete stream should end in an error event, got %s", apptest.Types(events))
	}
	h.Agent.SetRun(nil)
	if resp, _ := h.Ask(sid, "q3"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestModelWithoutAPriceIsRefusedBeforeCallingTheAgent(t *testing.T) {
	h := apptest.New(t, func(c *config.Config) { c.Model = "unpriced-model" })
	resp, _ := h.Ask(h.NewSession(), "q")
	if resp.StatusCode != http.StatusInternalServerError || h.Agent.Calls() != 0 {
		t.Fatalf("status = %d, agent calls = %d", resp.StatusCode, h.Agent.Calls())
	}
}

func TestEmptyQuestionIsRejected(t *testing.T) {
	h := apptest.New(t, nil)
	if resp, _ := h.Ask(h.NewSession(), "   "); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

var (
	_ = os.Stat
	_ = filepath.Join
)

func TestDisconnectingStillChargesTheUsageReportedSoFar(t *testing.T) {
	h := apptest.New(t, func(c *config.Config) { c.DailyChatBudgetUSD = 0.10 })
	sid := h.NewSession()
	started := make(chan struct{})
	stopped := make(chan struct{})
	h.Agent.SetRun(func(ctx context.Context, _ agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: "usage", Usage: &agent.Usage{Model: "test-model", InputTokens: 2000}}) // $0.20
		close(started)
		<-ctx.Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go h.AskCtx(ctx, sid, "expensive question, then close the tab")
	<-started
	cancel()
	<-stopped
	h.Agent.SetRun(nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := h.Ask(sid, "next")
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
	h := apptest.New(t, nil)
	h.Agent.SetRun(func(_ context.Context, _ agent.RunRequest, emit func(agent.Event)) {
		emit(agent.Event{Type: "usage", Usage: &agent.Usage{Model: "test-model", InputTokens: 10}})
		emit(apptest.DoneEvent("ok"))
	})
	_, events := h.Ask(h.NewSession(), "q")
	if apptest.Types(events) != "done" {
		t.Fatalf("events = %s", apptest.Types(events))
	}
}
