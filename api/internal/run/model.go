package run

import (
	"time"

	"dataanalyst/api/internal/agent"
)

// Status values stored in agent_runs.status.
const (
	StatusRunning         = "running"
	StatusDone            = "done"
	StatusError           = "error"
	StatusCancelled       = "cancelled"
	StatusBudgetExhausted = "budget_exhausted"
)

// Outcome is how a run ended, as recorded by Service.Finish.
type Outcome struct {
	Status  string
	Answer  string
	ErrText string
	Usage   *agent.Usage
	Latency time.Duration
}
