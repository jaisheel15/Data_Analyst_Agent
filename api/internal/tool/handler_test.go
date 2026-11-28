package tool_test

// Seam 6: POST /internal/tools/{name} over HTTP, with a fake sandbox.

import (
	"dataanalyst/api/internal/apptest"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/sandbox"
)

func toolReq(sid, runID, input string) agent.ToolRequest {
	return agent.ToolRequest{RunID: runID, SessionID: sid, Input: json.RawMessage(input)}
}

func TestToolCallsNeedTheInternalToken(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	run := h.StartRun(sid)
	for _, token := range []string{"", "wrong"} {
		resp, _ := h.CallTool("run_sql", toolReq(sid, run, `{"sql":"select 1"}`), token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
}

func TestToolCallsMustBelongToARunningRunOfThatSession(t *testing.T) {
	h := apptest.New(t, nil)
	sid, other := h.NewSession(), h.NewSession()
	run := h.StartRun(sid)
	cases := map[string]agent.ToolRequest{
		"unknown run":   toolReq(sid, "00000000-0000-0000-0000-000000000000", `{"sql":"select 1"}`),
		"other session": toolReq(other, run, `{"sql":"select 1"}`),
	}
	for name, req := range cases {
		resp, _ := h.CallTool("run_sql", req, apptest.InternalToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, resp.StatusCode)
		}
	}
	if resp, _ := h.CallTool("drop_tables", toolReq(sid, run, `{}`), apptest.InternalToken); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tool: status %d, want 404", resp.StatusCode)
	}
}

func TestRunSQLFormatsRowsForTheModel(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(_ string, req sandbox.Request) (sandbox.Result, error) {
		if req.Mode != "sql" || req.Code != "select n from t" || req.MaxRows != 50 {
			t.Errorf("sandbox got %+v", req)
		}
		return sandbox.Result{Columns: []string{"n", "label"}, Rows: [][]any{{1, "a"}, {2, nil}},
			TotalRows: 5, Truncated: true}, nil
	}
	sid := h.NewSession()
	resp, out := h.CallTool("run_sql", toolReq(sid, h.StartRun(sid), `{"sql":"select n from t"}`), apptest.InternalToken)
	if resp.StatusCode != http.StatusOK || out.Error != "" {
		t.Fatalf("status %d, error %q", resp.StatusCode, out.Error)
	}
	want := "n\tlabel\n1\ta\n2\tNULL\n(showing 2 of 5 rows)"
	if out.Output != want {
		t.Fatalf("output =\n%s\nwant\n%s", out.Output, want)
	}
}

func TestFailedCodeComesBackAsAToolErrorNotAnHTTPError(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "partial\n", Error: "Traceback ...\nKeyError: 'x'"}, nil
	}
	sid := h.NewSession()
	resp, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"d['x']"}`), apptest.InternalToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(out.Error, "KeyError") || !strings.Contains(out.Output, "partial") {
		t.Fatalf("got %+v", out)
	}
}

func TestBusySandboxesAreReportedToTheAgent(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{}, sandbox.ErrAtCapacity
	}
	sid := h.NewSession()
	_, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"1"}`), apptest.InternalToken)
	if !strings.Contains(out.Error, "busy") {
		t.Fatalf("error = %q", out.Error)
	}
}

func TestLongOutputIsTruncatedForTheModel(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Stdout: strings.Repeat("y", 20_000)}, nil
	}
	sid := h.NewSession()
	_, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"1"}`), apptest.InternalToken)
	if len(out.Output) > 8200 || !strings.Contains(out.Output, "[truncated") {
		t.Fatalf("output length %d, tail %q", len(out.Output), out.Output[len(out.Output)-60:])
	}
}

func TestChartsFromPythonAreKeptAndServed(t *testing.T) {
	h := apptest.New(t, nil)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	h.Sandbox.Result = func(sessionID string, _ sandbox.Request) (sandbox.Result, error) {
		if err := os.WriteFile(filepath.Join(h.Layout.OutDir(sessionID), "chart_1.png"), png, 0o644); err != nil {
			t.Fatal(err)
		}
		return sandbox.Result{Files: []string{"chart_1.png"}}, nil
	}
	sid := h.NewSession()
	_, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"plot"}`), apptest.InternalToken)
	if len(out.ChartIDs) != 1 {
		t.Fatalf("chart ids = %v", out.ChartIDs)
	}
	if !strings.Contains(out.Output, "chart saved") {
		t.Errorf("model should be told a chart was produced: %q", out.Output)
	}
	if apptest.FileExists(filepath.Join(h.Layout.OutDir(sid), "chart_1.png")) {
		t.Error("out dir should be emptied so the next call's chart_1.png is not confused with this one")
	}
	resp, body := h.Do("GET", "/api/charts/"+out.ChartIDs[0], nil, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || string(body) != string(png) {
		t.Fatalf("chart fetch: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestDescribeDatasetReadsTheRealSessionDatabase(t *testing.T) {
	h := apptest.New(t, nil)
	sid := h.NewSession()
	h.MustUpload(sid, "sales.csv", []byte(apptest.SalesCSV))
	run := h.StartRun(sid)
	_, out := h.CallTool("describe_dataset", toolReq(sid, run, `{}`), apptest.InternalToken)
	for _, want := range []string{"sales", "3 rows", "region VARCHAR", `"sold on" DATE`, "north"} {
		if !strings.Contains(out.Output, want) {
			t.Errorf("output missing %q:\n%s", want, out.Output)
		}
	}
	_, out = h.CallTool("describe_dataset", toolReq(sid, run, `{"table":"nope"}`), apptest.InternalToken)
	if !strings.Contains(out.Error, "no table named") {
		t.Errorf("error = %q", out.Error)
	}
}

func TestSymlinksInTheOutDirAreNeverServedAsCharts(t *testing.T) {
	h := apptest.New(t, nil)
	secret := filepath.Join(t.TempDir(), "secret.env")
	if err := os.WriteFile(secret, []byte("ANTHROPIC_API_KEY=sk-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Sandbox.Result = func(sessionID string, _ sandbox.Request) (sandbox.Result, error) {
		// What `os.symlink('/proc/self/environ', '/out/a.png')` in agent code produces.
		if err := os.Symlink(secret, filepath.Join(h.Layout.OutDir(sessionID), "a.png")); err != nil {
			t.Fatal(err)
		}
		return sandbox.Result{Files: []string{"a.png"}}, nil
	}
	sid := h.NewSession()
	_, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"x"}`), apptest.InternalToken)
	if len(out.ChartIDs) != 0 || strings.Contains(out.Output, "chart saved") {
		t.Fatalf("a symlink was accepted as a chart: %+v", out)
	}
	entries, _ := os.ReadDir(h.Layout.ChartsDir(sid))
	if len(entries) != 0 {
		t.Fatalf("charts dir holds %d entries, want 0", len(entries))
	}
}

func TestSQLNumbersReachTheModelInPlainNotation(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Columns: []string{"a", "b", "c", "d"},
			Rows: [][]any{{float64(1000000), float64(1234567), 0.5, float64(20231115)}}, TotalRows: 1}, nil
	}
	sid := h.NewSession()
	_, out := h.CallTool("run_sql", toolReq(sid, h.StartRun(sid), `{"sql":"select 1"}`), apptest.InternalToken)
	if !strings.Contains(out.Output, "1000000\t1234567\t0.5\t20231115") {
		t.Fatalf("output = %q", out.Output)
	}
}

func TestTruncationNeverSplitsAMultiByteCharacter(t *testing.T) {
	h := apptest.New(t, nil)
	h.Sandbox.Result = func(string, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "x" + strings.Repeat("€", 5000)}, nil // 8000 lands mid-character
	}
	sid := h.NewSession()
	resp, out := h.CallTool("run_python", toolReq(sid, h.StartRun(sid), `{"code":"1"}`), apptest.InternalToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !utf8.ValidString(out.Output) {
		t.Fatal("truncated output is not valid UTF-8")
	}
}
