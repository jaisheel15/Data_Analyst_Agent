package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/cost"
	"dataanalyst/api/internal/db"
	"dataanalyst/api/internal/httpapi"
	"dataanalyst/api/internal/sandbox"
	"dataanalyst/api/internal/storage"
	"dataanalyst/api/internal/testenv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const internalToken = "test-internal-token"

// fakeSandbox returns scripted results and records requests.
type fakeSandbox struct {
	mu       sync.Mutex
	requests []sandbox.Request
	result   func(sessionID string, req sandbox.Request) (sandbox.Result, error)
	full     bool // HasCapacity reports false for new sessions
}

func (f *fakeSandbox) Exec(_ context.Context, sessionID string, req sandbox.Request) (sandbox.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.result != nil {
		return f.result(sessionID, req)
	}
	return sandbox.Result{Stdout: "ok"}, nil
}

func (f *fakeSandbox) HasCapacity(string) bool          { return !f.full }
func (f *fakeSandbox) Stop(context.Context, string) error { return nil }

// fakeAgent plays a scripted agent: run gets the request and an emit func.
type fakeAgent struct {
	mu   sync.Mutex
	reqs []contract.RunRequest
	run  func(ctx context.Context, req contract.RunRequest, emit func(contract.Event) error) error
}

func (f *fakeAgent) Run(ctx context.Context, req contract.RunRequest, emit func(contract.Event) error) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.run == nil {
		return emit(contract.Event{Type: contract.EventDone, Answer: "fine", Status: "done",
			Usage: &contract.Usage{Model: "test-model", InputTokens: 1000, OutputTokens: 100}})
	}
	return f.run(ctx, req, emit)
}

func (f *fakeAgent) lastRequest(t *testing.T) contract.RunRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("agent was never called")
	}
	return f.reqs[len(f.reqs)-1]
}

type harness struct {
	t        *testing.T
	pool     *pgxpool.Pool
	srv      *httpapi.Server
	public   *httptest.Server
	internal *httptest.Server
	layout   storage.Layout
	sandbox  *fakeSandbox
	agent    *fakeAgent
	now      time.Time
}

func newHarness(t *testing.T, tweak func(*httpapi.Config)) *harness {
	t.Helper()
	pool := testenv.Postgres(t)
	layout := storage.Layout{Root: t.TempDir()}
	h := &harness{t: t, pool: pool, layout: layout, sandbox: &fakeSandbox{}, agent: &fakeAgent{},
		now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	cfg := httpapi.Config{
		InternalToken:      internalToken,
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
	h.srv = httpapi.New(httpapi.Deps{
		Pool:    pool,
		Layout:  layout,
		Sandbox: h.sandbox,
		Agent:   h.agent,
		Pricing: cost.Pricing{"test-model": {InputPerMTok: 100, OutputPerMTok: 500}},
		Now:     func() time.Time { return h.now },
		Config:  cfg,
	})
	h.public = httptest.NewServer(h.srv.Public())
	h.internal = httptest.NewServer(h.srv.Internal())
	t.Cleanup(h.public.Close)
	t.Cleanup(h.internal.Close)
	return h
}

func (h *harness) do(method, path string, body io.Reader, contentType string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.public.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (h *harness) newSession() string {
	h.t.Helper()
	resp, raw := h.do("POST", "/api/sessions", nil, "")
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create session: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	mustJSON(h.t, raw, &out)
	return out.ID
}

func (h *harness) upload(sessionID, filename string, content []byte) (*http.Response, []byte) {
	h.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		h.t.Fatal(err)
	}
	part.Write(content)
	w.Close()
	return h.do("POST", "/api/sessions/"+sessionID+"/datasets", &buf, w.FormDataContentType())
}

func (h *harness) mustUpload(sessionID, filename string, content []byte) contract.Table {
	h.t.Helper()
	resp, raw := h.upload(sessionID, filename, content)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("upload %s: %d %s", filename, resp.StatusCode, raw)
	}
	var out struct {
		Table contract.Table `json:"table"`
	}
	mustJSON(h.t, raw, &out)
	return out.Table
}

func (h *harness) callTool(tool string, req contract.ToolRequest, token string) (*http.Response, contract.ToolResponse) {
	h.t.Helper()
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", h.internal.URL+"/internal/tools/"+tool, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("X-Internal-Token", token)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out contract.ToolResponse
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		mustJSON(h.t, raw, &out)
	}
	return resp, out
}

func mustJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var _ = filepath.Join

// startRun puts a running run in place, as the chat endpoint would. Setup only:
// tests observe results through the HTTP APIs, never by reading the database.
func (h *harness) startRun(sessionID string) string {
	h.t.Helper()
	q := db.New(h.pool)
	ctx := context.Background()
	sid := uuid.MustParse(sessionID)
	msg, err := q.CreateMessage(ctx, db.CreateMessageParams{
		ID: uuid.New(), SessionID: sid, Role: "user", Content: "question"})
	if err != nil {
		h.t.Fatal(err)
	}
	run, err := q.CreateRun(ctx, db.CreateRunParams{ID: uuid.New(), SessionID: sid,
		MessageID: msg.ID, Purpose: "chat", Provider: "anthropic", Model: "test-model"})
	if err != nil {
		h.t.Fatal(err)
	}
	return run.ID.String()
}
