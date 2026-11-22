package httpapi_test

import (
	"net/http"
	"testing"
)

func TestHealthReportsTheAgentService(t *testing.T) {
	h := newHarness(t, nil)
	get := func() int {
		resp, err := http.Get(h.public.URL + "/api/health")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("healthy stack: status = %d, want 200", code)
	}
	h.agent.unhealthy.Store(true)
	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("agent down: status = %d, want 503", code)
	}
}
