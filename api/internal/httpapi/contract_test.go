package httpapi_test

// Seam 9 (Go side): what Go sends and returns matches contracts/.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"dataanalyst/api/internal/contract"
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
	h := newHarness(t, nil)
	sid := h.newSession()
	h.mustUpload(sid, "sales.csv", []byte(salesCSV))
	h.agent.setRun(func(_ context.Context, req contract.RunRequest, emit func(contract.Event)) {
		h.callTool("run_sql", contract.ToolRequest{RunID: req.RunID, SessionID: req.SessionID,
			Input: json.RawMessage(`{"sql":"select 1"}`)}, internalToken)
		emit(doneEvent("one"))
	})
	h.ask(sid, "first")  // empty history
	h.ask(sid, "second") // history with a tool step
	s := contractSchema(t, "RunRequest")
	for _, raw := range h.agent.raw {
		validate(t, s, raw)
	}
}

func TestToolResponsesMatchTheContract(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	run := h.startRun(sid)
	s := contractSchema(t, "ToolResponse")
	results := []sandbox.Result{{Stdout: "ok"}, {Error: "boom"}, {Columns: []string{"a"}, Rows: [][]any{{1}}, TotalRows: 1}}
	for i, tool := range []string{"run_python", "run_python", "run_sql"} {
		res := results[i]
		h.sandbox.result = func(string, sandbox.Request) (sandbox.Result, error) { return res, nil }
		body, _ := json.Marshal(contract.ToolRequest{RunID: run, SessionID: sid, Input: json.RawMessage(`{"code":"1","sql":"select 1"}`)})
		req, _ := http.NewRequest("POST", h.internal.URL+"/internal/tools/"+tool, bytes.NewReader(body))
		req.Header.Set("X-Internal-Token", internalToken)
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
