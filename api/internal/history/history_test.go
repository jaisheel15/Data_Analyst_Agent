package history_test

// Seam 3: history.Build, a pure function from stored turns to the history
// sent to the agent service.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"dataanalyst/api/internal/history"
)

func turns(n int) []history.StoredTurn {
	out := make([]history.StoredTurn, n)
	for i := range out {
		out[i] = history.StoredTurn{
			Question: fmt.Sprintf("q%d", i),
			Answer:   fmt.Sprintf("a%d", i),
			Steps: []history.StoredStep{{
				Tool:   "run_sql",
				Input:  json.RawMessage(`{"sql":"select 1"}`),
				Output: "1",
			}},
		}
	}
	return out
}

func TestLastThreeTurnsKeepToolStepsOlderOnlyQuestionAndAnswer(t *testing.T) {
	got := history.Build(turns(5))
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i, turn := range got {
		if turn.Question != fmt.Sprintf("q%d", i) || turn.Answer != fmt.Sprintf("a%d", i) {
			t.Errorf("turn %d out of order: %+v", i, turn)
		}
		wantSteps := 0
		if i >= 2 {
			wantSteps = 1
		}
		if len(turn.Steps) != wantSteps {
			t.Errorf("turn %d has %d steps, want %d", i, len(turn.Steps), wantSteps)
		}
	}
	if string(got[4].Steps[0].Input) != `{"sql":"select 1"}` {
		t.Errorf("step input = %s", got[4].Steps[0].Input)
	}
}

func TestLongToolOutputsAreCutToOneKilobyte(t *testing.T) {
	ts := turns(1)
	ts[0].Steps[0].Output = strings.Repeat("x", 3000)
	out := history.Build(ts)[0].Steps[0].Output
	if !strings.HasPrefix(out, strings.Repeat("x", 1024)) {
		t.Fatal("output does not keep the first 1024 bytes")
	}
	if !strings.HasSuffix(out, "[truncated 1976 more bytes]") {
		t.Fatalf("missing truncation note: %q", out[1024:])
	}
}

func TestAtMostTenTurnsAreSent(t *testing.T) {
	got := history.Build(turns(12))
	if len(got) != 10 {
		t.Fatalf("len = %d, want 10", len(got))
	}
	if got[0].Question != "q2" || got[9].Question != "q11" {
		t.Fatalf("wrong window: first=%s last=%s", got[0].Question, got[9].Question)
	}
}

func TestNoTurnsGivesEmptyNonNilHistory(t *testing.T) {
	got := history.Build(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty slice (serialises as [])", got)
	}
}
