// Package contract holds the Go side of the internal API between this service
// and the Python agent service. The source of truth is contracts/internal-api.schema.json;
// contract tests check both services against it.
package contract

import "encoding/json"

// ---- Go → Python: POST /runs ----

type RunRequest struct {
	RunID     string  `json:"run_id"`
	SessionID string  `json:"session_id"`
	Question  string  `json:"question"`
	Model     Model   `json:"model"`
	Schema    []Table `json:"schema"`
	History   []Turn  `json:"history"`
	Limits    Limits  `json:"limits"`
}

// Model is chosen by Go so pricing is checked before a run starts.
type Model struct {
	Provider string `json:"provider"` // "anthropic" or "deepseek"
	Name     string `json:"name"`
}

type Table struct {
	Name       string   `json:"name"`
	Rows       int64    `json:"rows"`
	Columns    []Column `json:"columns"`
	SampleRows [][]any  `json:"sample_rows"`
}

type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Turn struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Steps    []Step `json:"steps"`
}

type Step struct {
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
}

type Limits struct {
	MaxSteps  int `json:"max_steps"`
	MaxTokens int `json:"max_tokens"`
}

// ---- Python → Go: streamed NDJSON events, one per line ----

const (
	EventStepStart   = "step_start"
	EventToolCall    = "tool_call"
	EventToolResult  = "tool_result"
	EventAnswerDelta = "answer_delta"
	EventUsage       = "usage" // running totals after each model call; not sent to browsers
	EventDone        = "done"
	EventError       = "error"
)

// Event is one line of the /runs stream. Fields not used by a type are omitted.
type Event struct {
	Type     string          `json:"type"`
	Step     int             `json:"step,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Summary  string          `json:"summary,omitempty"`
	Error    string          `json:"error,omitempty"`
	ChartIDs []string        `json:"chart_ids,omitempty"`
	Text     string          `json:"text,omitempty"`
	Answer   string          `json:"answer,omitempty"`
	Status   string          `json:"status,omitempty"` // done: "done" or "budget_exhausted"
	Usage    *Usage          `json:"usage,omitempty"`
}

type Usage struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"` // uncached input only
	OutputTokens     int64  `json:"output_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`      // cache reads
	CacheWriteTokens int64  `json:"cache_write_tokens"` // cache writes
	Steps            int    `json:"steps"`
}

// ---- Python → Go: POST /internal/tools/{name} ----

type ToolRequest struct {
	RunID     string          `json:"run_id"`
	SessionID string          `json:"session_id"`
	Input     json.RawMessage `json:"input"`
}

type ToolResponse struct {
	// Output is the text the LLM sees, already truncated.
	Output   string   `json:"output"`
	Error    string   `json:"error,omitempty"`
	ChartIDs []string `json:"chart_ids"`
}

// Tool names.
const (
	ToolDescribeDataset = "describe_dataset"
	ToolRunSQL          = "run_sql"
	ToolRunPython       = "run_python"
)
