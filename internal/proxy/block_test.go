package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestRenderProviderError: the block error body is shaped like the caller's
// provider so the agent harness parses it like any upstream API error.
func TestRenderProviderError(t *testing.T) {
	const msg = "PII detected"
	cases := map[string]func(m map[string]any) bool{
		"openai": func(m map[string]any) bool {
			e, _ := m["error"].(map[string]any)
			return e != nil && e["message"] == msg && e["type"] == "pii"
		},
		"anthropic": func(m map[string]any) bool {
			if m["type"] != "error" {
				return false
			}
			e, _ := m["error"].(map[string]any)
			return e != nil && e["message"] == msg && e["type"] == "pii"
		},
		"gemini": func(m map[string]any) bool {
			e, _ := m["error"].(map[string]any)
			return e != nil && e["message"] == msg && e["code"].(float64) == 422
		},
		"bedrock": func(m map[string]any) bool {
			return m["message"] == msg
		},
	}
	for format, check := range cases {
		t.Run(format, func(t *testing.T) {
			body := renderProviderError(format, 422, "pii", msg)
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if !check(m) {
				t.Fatalf("%s envelope wrong: %s", format, body)
			}
		})
	}
}

// blockE2E spins up a proxy loading the given example plugin against an upstream
// that counts how many times it is called, then posts a request containing the
// trigger word.
func blockE2E(t *testing.T, order []string) (status int, body []byte, upstreamHits *int32) {
	t.Helper()
	requireWASM(t, "../../examples/plugins/"+order[0]+"/plugin.wasm")

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	srv, err := New(Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
			Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: order},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"please blockme now"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, &hits
}

// TestRequestVetoE2E: a plugin holding env.block_request rejects the request —
// the client gets the custom 422 + provider-shaped error and the upstream is
// never called.
func TestRequestVetoE2E(t *testing.T) {
	status, body, hits := blockE2E(t, []string{"test-blocker"})
	if status != 422 {
		t.Fatalf("status = %d, want 422; body=%s", status, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, body)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil || !strings.Contains(e["message"].(string), "test-blocker") {
		t.Fatalf("unexpected error body: %s", body)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream was called %d times; a blocked request must never reach upstream", n)
	}
}

// TestRequestVetoRequiresGrant: a plugin that sets _block without declaring
// env.block_request is ignored — the request is forwarded upstream.
func TestRequestVetoRequiresGrant(t *testing.T) {
	status, body, hits := blockE2E(t, []string{"test-blocker-nogrant"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ungranted block must be ignored); body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream called %d times, want 1 (request should pass through)", n)
	}
}
