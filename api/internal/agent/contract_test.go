package agent_test

// Seam 9 (Go side): what Go sends and returns matches contracts/.

import (
	"bytes"
	"context"
	"dataanalyst/api/internal/apptest"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/sandbox"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func contractSchema(t *testing.T, def string) *jsonschema.Schema {
	t.Helper()
	f, err := os.Open("../../../contracts/internal-api.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	const url = "https://data-analyst-agent.local/contracts/internal-api.schema.json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(url + "#/$defs/" + def)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, raw []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Errorf("payload does not match the contract: %v\n%s", err, raw)
	}
}

func TestRunRequestsMatchTheContract(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.MustUpload(sid, "sales.csv", []byte(apptest.SalesCSV))
	h.Agent.SetRun(func(_ context.Context, req agent.RunRequest, emit func(agent.Event)) {
		h.CallTool("run_sql", agent.ToolRequest{RunID: req.RunID, SessionID: req.SessionID,
			Input: json.RawMessage(`{"sql":"select 1"}`)}, apptest.InternalToken)
		emit(apptest.DoneEvent("one"))
	})
	h.Ask(sid, "first")  // empty history
	h.Ask(sid, "second") // history with a tool step
	s := contractSchema(t, "RunRequest")
	for _, raw := range h.Agent.Raw {
		validate(t, s, raw)
	}
}

func TestToolResponsesMatchTheContract(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	run := h.StartRun(sid)
	s := contractSchema(t, "ToolResponse")
	results := []sandbox.Result{{Stdout: "ok"}, {Error: "boom"}, {Columns: []string{"a"}, Rows: [][]any{{1}}, TotalRows: 1}}
	for i, tool := range []string{"run_python", "run_python", "run_sql"} {
		res := results[i]
		h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) { return res, nil }
		body, _ := json.Marshal(agent.ToolRequest{RunID: run, SessionID: sid, Input: json.RawMessage(`{"code":"1","sql":"select 1"}`)})
		req, _ := http.NewRequest("POST", h.Internal.URL+"/internal/tools/"+tool, bytes.NewReader(body))
		req.Header.Set("X-Internal-Token", apptest.InternalToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		validate(t, s, raw)
	}
}

func TestContractValidatorRejectsBadPayloads(t *testing.T) {
	s := contractSchema(t, "ToolResponse")
	v, _ := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(`{"output":"x"}`))) // no chart_ids
	if err := s.Validate(v); err == nil {
		t.Fatal("validator accepted a payload missing a required field")
	}
}
