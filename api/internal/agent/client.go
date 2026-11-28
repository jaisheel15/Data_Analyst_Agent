// Package agent holds the contract with the Python agent service (contract.go)
// and the client that calls it (this file): POST /runs, reading
// the streamed NDJSON events one line at a time.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// ErrIncompleteStream means the stream ended without a done or error event.
var ErrIncompleteStream = errors.New("agent stream ended without a done or error event")

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	// No overall timeout: runs stream for as long as the agent works. The
	// caller's context (the browser request) bounds the run.
	// The transport sends the Run's traceparent so the agent's spans join its
	// trace. Health checks stay out of traces.
	transport := otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/health" }))
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Transport: transport}}
}

// Run starts a run and calls emit for each event, in order. Cancelling ctx
// closes the stream, which tells the agent service to stop.
func (c *Client) Run(ctx context.Context, req RunRequest, emit func(Event) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/runs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")
	httpReq.Header.Set("X-Internal-Token", c.Token)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return fmt.Errorf("agent service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("agent service returned %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	finished := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("agent service sent invalid event: %w", err)
		}
		if ev.Type == EventDone || ev.Type == EventError {
			finished = true
		}
		if err := emit(ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("agent stream: %w", err)
	}
	if !finished {
		return ErrIncompleteStream
	}
	return nil
}

// Ping checks the agent service is up.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent health: %d", resp.StatusCode)
	}
	return nil
}
