// Package history turns stored conversation turns into the bounded history
// sent to the agent service with each run.
package history

import (
	"encoding/json"
	"fmt"

	"dataanalyst/api/internal/contract"
)

const (
	FullTurns     = 3    // most recent turns sent with their tool steps
	MaxTurns      = 10   // turns sent at all; older ones are dropped
	MaxStepOutput = 1024 // bytes of each step's output kept
)

// StoredTurn is one finished question/answer pair, oldest first.
type StoredTurn struct {
	Question string
	Answer   string
	Steps    []StoredStep
}

type StoredStep struct {
	Tool   string
	Input  json.RawMessage
	Output string
}

// Build keeps the last MaxTurns turns. The last FullTurns of those carry
// compact tool steps; earlier ones carry only the question and answer.
func Build(turns []StoredTurn) []contract.Turn {
	if len(turns) > MaxTurns {
		turns = turns[len(turns)-MaxTurns:]
	}
	out := make([]contract.Turn, 0, len(turns))
	for i, t := range turns {
		turn := contract.Turn{Question: t.Question, Answer: t.Answer, Steps: []contract.Step{}}
		if i >= len(turns)-FullTurns {
			for _, s := range t.Steps {
				turn.Steps = append(turn.Steps, contract.Step{
					Tool: s.Tool, Input: s.Input, Output: cut(s.Output, MaxStepOutput),
				})
			}
		}
		out = append(out, turn)
	}
	return out
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n[truncated %d more bytes]", len(s)-n)
}
