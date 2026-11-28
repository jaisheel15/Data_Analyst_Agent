// Package apptest is the shared Harness for HTTP tests: the real domains wired
// on a real Postgres, with a fake sandbox and a fake agent service.
package apptest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/budget"
	"dataanalyst/api/internal/platform/config"
	"dataanalyst/api/internal/platform/db"
	"dataanalyst/api/internal/platform/server"
	"dataanalyst/api/internal/platform/storage"
	"dataanalyst/api/internal/sandbox"
	"dataanalyst/api/internal/testenv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const InternalToken = "test-internal-token"

// FakeSandbox returns scripted results and records requests.
type FakeSandbox struct {
	mu       sync.Mutex
	Requests []sandbox.Request
	Result   func(sessionID string, req sandbox.Request) (sandbox.Result, error)
	Full     bool // HasCapacity reports false for new sessions
}

func (f *FakeSandbox) Exec(_ context.Context, sessionID string, req sandbox.Request) (sandbox.Result, error) {
	f.mu.Lock()
	f.Requests = append(f.Requests, req)
	f.mu.Unlock()
	if f.Result != nil {
		return f.Result(sessionID, req)
	}
	return sandbox.Result{Stdout: "ok"}, nil
}

func (f *FakeSandbox) HasCapacity(string) bool            { return !f.Full }
func (f *FakeSandbox) Stop(context.Context, string) error { return nil }

// FakeAgent is a fake agent service over HTTP. It speaks the real NDJSON
// protocol, so the real agent client is exercised too. run gets the decoded
// request, the request context (cancelled when Go hangs up), and an emit func.
type FakeAgent struct {
	mu   sync.Mutex
	reqs []agent.RunRequest
	Raw  [][]byte // request bodies exactly as Go sent them
	Hdrs []http.Header
	run  func(ctx context.Context, req agent.RunRequest, emit func(agent.Event))
	srv  *httptest.Server

	Unhealthy atomic.Bool // makes GET /health answer 503
}

func newFakeAgent(t *testing.T) *FakeAgent {
	f := &FakeAgent{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if f.Unhealthy.Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
			}
			return
		}
		if r.Header.Get("X-Internal-Token") != InternalToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req agent.RunRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.Raw = append(f.Raw, raw)
		f.Hdrs = append(f.Hdrs, r.Header.Clone())
		run := f.run
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		emit := func(ev agent.Event) {
			line, _ := json.Marshal(ev)
			w.Write(append(line, '\n'))
			w.(http.Flusher).Flush()
		}
		if run == nil {
			emit(DoneEvent("fine"))
			return
		}
		run(r.Context(), req, emit)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func DoneEvent(answer string) agent.Event {
	return agent.Event{Type: agent.EventDone, Answer: answer, Status: "done",
		Usage: &agent.Usage{Provider: "anthropic", Model: "test-model",
			InputTokens: 1000, OutputTokens: 100, Steps: 1}} // $0.10 + $0.05 at test prices
}

func (f *FakeAgent) SetRun(run func(ctx context.Context, req agent.RunRequest, emit func(agent.Event))) {
	f.mu.Lock()
	f.run = run
	f.mu.Unlock()
}

// LastHeaders returns the headers of the last POST /runs.
func (f *FakeAgent) LastHeaders(t *testing.T) http.Header {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Hdrs) == 0 {
		t.Fatal("agent was never called")
	}
	return f.Hdrs[len(f.Hdrs)-1]
}

func (f *FakeAgent) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *FakeAgent) LastRequest(t *testing.T) agent.RunRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("agent was never called")
	}
	return f.reqs[len(f.reqs)-1]
}

type Harness struct {
	T        *testing.T
	Pool     *pgxpool.Pool
	Public   *httptest.Server
	Internal *httptest.Server
	Layout   storage.Layout
	Sandbox  *FakeSandbox
	Agent    *FakeAgent
	Now      time.Time
}

// New wires the real app on a fresh Postgres. tweak may adjust the config.
func New(t *testing.T, tweak func(*config.Config)) *Harness {
	t.Helper()
	pool := testenv.Postgres(t)
	layout := storage.Layout{Root: t.TempDir()}
	h := &Harness{T: t, Pool: pool, Layout: layout, Sandbox: &FakeSandbox{}, Agent: newFakeAgent(t),
		Now: todayAtNoonUTC()}
	cfg := config.Config{
		InternalToken:      InternalToken,
		EvalToken:          "test-eval-token",
		MaxUploadBytes:     25 << 20,
		DailyChatBudgetUSD: 0.50,
		Provider:           "anthropic",
		Model:              "test-model",
		MaxSteps:           10,
		MaxTokens:          4096,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	app := server.New(server.Deps{
		Pool:    pool,
		Layout:  layout,
		Sandbox: h.Sandbox,
		Agent:   agent.New(h.Agent.srv.URL, InternalToken),
		Pricing: budget.Pricing{"test-model": {InputPerMTok: 100, OutputPerMTok: 500}},
		Now:     func() time.Time { return h.Now },
		Config:  cfg,
	})
	h.Public = httptest.NewServer(app.Public())
	h.Internal = httptest.NewServer(app.Internal())
	t.Cleanup(h.Public.Close)
	t.Cleanup(h.Internal.Close)
	return h
}

func (h *Harness) Do(method, path string, body io.Reader, contentType string) (*http.Response, []byte) {
	h.T.Helper()
	req, err := http.NewRequest(method, h.Public.URL+path, body)
	if err != nil {
		h.T.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (h *Harness) NewSession() string {
	h.T.Helper()
	resp, raw := h.Do("POST", "/api/sessions", nil, "")
	if resp.StatusCode != http.StatusCreated {
		h.T.Fatalf("create session: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	MustJSON(h.T, raw, &out)
	return out.ID
}

func (h *Harness) Upload(sessionID, filename string, content []byte) (*http.Response, []byte) {
	h.T.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		h.T.Fatal(err)
	}
	part.Write(content)
	w.Close()
	return h.Do("POST", "/api/sessions/"+sessionID+"/datasets", &buf, w.FormDataContentType())
}

func (h *Harness) MustUpload(sessionID, filename string, content []byte) agent.Table {
	h.T.Helper()
	resp, raw := h.Upload(sessionID, filename, content)
	if resp.StatusCode != http.StatusCreated {
		h.T.Fatalf("upload %s: %d %s", filename, resp.StatusCode, raw)
	}
	var out struct {
		Table agent.Table `json:"table"`
	}
	MustJSON(h.T, raw, &out)
	return out.Table
}

// CallTool posts a tool call to the internal API. headers are extra
// name, value pairs (for example a traceparent).
func (h *Harness) CallTool(tool string, req agent.ToolRequest, token string, headers ...string) (*http.Response, agent.ToolResponse) {
	h.T.Helper()
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", h.Internal.URL+"/internal/tools/"+tool, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("X-Internal-Token", token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		httpReq.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		h.T.Fatal(err)
	}
	defer resp.Body.Close()
	var out agent.ToolResponse
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		MustJSON(h.T, raw, &out)
	}
	return resp, out
}

func MustJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var _ = filepath.Join

// startRun puts a running run in place, as the chat endpoint would. Setup only:
// tests observe results through the HTTP APIs, never by reading the database.
func (h *Harness) StartRun(sessionID string) string {
	h.T.Helper()
	q := db.New(h.Pool)
	ctx := context.Background()
	sid := uuid.MustParse(sessionID)
	msg, err := q.CreateMessage(ctx, db.CreateMessageParams{
		ID: uuid.New(), SessionID: sid, Role: "user", Content: "question"})
	if err != nil {
		h.T.Fatal(err)
	}
	run, err := q.CreateRun(ctx, db.CreateRunParams{ID: uuid.New(), SessionID: sid,
		MessageID: msg.ID, Purpose: "chat", Provider: "anthropic", Model: "test-model"})
	if err != nil {
		h.T.Fatal(err)
	}
	return run.ID.String()
}

// todayAtNoonUTC is the fake clock's start. Runs are stamped by Postgres with the
// real time, so the fake clock has to start on the real UTC day or the budget
// tests would depend on the date they happen to run on.
func todayAtNoonUTC() time.Time {
	y, m, d := time.Now().UTC().Date()
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// SalesCSV is a small dataset used across the domain tests.
const SalesCSV = "region,amount,sold on\nnorth,10,2024-01-05\nsouth,20,2024-02-10\nnorth,5,2024-03-15\n"

// SSE is one server-sent event read from a chat stream.
type SSE struct {
	Type string
	Data map[string]any
}

// Ask posts a question and reads the whole SSE stream.
func (h *Harness) Ask(sid, question string, headers ...string) (*http.Response, []SSE) {
	h.T.Helper()
	return h.AskCtx(context.Background(), sid, question, headers...)
}

func (h *Harness) AskCtx(ctx context.Context, sid, question string, headers ...string) (*http.Response, []SSE) {
	h.T.Helper()
	body, _ := json.Marshal(map[string]string{"question": question})
	req, _ := http.NewRequestWithContext(ctx, "POST", h.Public.URL+"/api/sessions/"+sid+"/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		h.T.Error(err) // not Fatal: this also runs in goroutines
		return nil, nil
	}
	defer resp.Body.Close()
	var events []SSE
	sc := bufio.NewScanner(resp.Body)
	var cur SSE
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
		case line == "" && cur.Type != "":
			events = append(events, cur)
			cur = SSE{}
		}
	}
	return resp, events
}

// Types joins the event types, for order assertions.
func Types(events []SSE) string {
	var ts []string
	for _, e := range events {
		ts = append(ts, e.Type)
	}
	return strings.Join(ts, ",")
}
