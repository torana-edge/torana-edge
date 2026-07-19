// Package e2e is a black-box, config-file-driven integration suite for Torana
// (private-nucleus#25). Unlike the white-box tests in internal/proxy (which
// construct proxy.Config in Go), these write a real config.json to disk, load it
// via provider.Load, boot the full proxy.Server on a real port, and drive it
// with an HTTP client — exercising the actual config-parse → wire path a user
// hits. Workflows: standard + streamed completions, a 429 → fallback chain, and
// the /health and /stats endpoints, each with a latency budget.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/proxy"

	// Register the wire-format adapters, exactly as cmd/torana does.
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// boot writes cfgJSON to a temp config.json, loads it through provider.Load, and
// starts a proxy.Server on a free port. Returns the base URL.
func boot(t *testing.T, cfgJSON string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	provCfg, err := provider.Load(cfgPath)
	if err != nil {
		t.Fatalf("provider.Load: %v", err)
	}
	srv, err := proxy.New(proxy.Config{Port: "0", Providers: provCfg})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return "http://" + ln.Addr().String()
}

func post(t *testing.T, url, body string) (*http.Response, []byte, time.Duration) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-e2e")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b, time.Since(start)
}

// TestStandardCompletion: a non-streaming completion round-trips through a
// file-loaded config and returns the upstream body, within a latency budget.
func TestStandardCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer upstream.Close()

	base := boot(t, fmt.Sprintf(`{
		"port": 8080,
		"providers": {"oai": {"url": %q, "format": "openai"}}
	}`, upstream.URL))

	resp, body, dur := post(t, base+"/provider/oai/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"content":"pong"`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if dur > 5*time.Second {
		t.Errorf("latency budget exceeded: %v", dur)
	}
}

// TestStreamedCompletion: an SSE completion streams through unbroken and carries
// the [DONE] sentinel.
func TestStreamedCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"po"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"ng"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		} {
			io.WriteString(w, "data: "+f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	base := boot(t, fmt.Sprintf(`{"providers": {"oai": {"url": %q, "format": "openai"}}}`, upstream.URL))

	resp, body, dur := post(t, base+"/provider/oai/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q, want event-stream", ct)
	}
	s := string(body)
	if !strings.Contains(s, `"content":"po"`) || !strings.Contains(s, `"content":"ng"`) || !strings.Contains(s, "[DONE]") {
		t.Fatalf("streamed body incomplete: %s", s)
	}
	if dur > 5*time.Second {
		t.Errorf("latency budget exceeded: %v", dur)
	}
}

// TestFailoverChain: the primary returns 429, and the request succeeds against
// the configured fallback — driven entirely by the file-loaded fallback list.
func TestFailoverChain(t *testing.T) {
	var primaryHits, fallbackHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"from-fallback"},"finish_reason":"stop"}]}`)
	}))
	defer fallback.Close()

	base := boot(t, fmt.Sprintf(`{
		"providers": {
			"primary": {"url": %q, "format": "openai", "fallback": ["backup"]},
			"backup":  {"url": %q, "format": "openai"}
		}
	}`, primary.URL, fallback.URL))

	resp, body, _ := post(t, base+"/provider/primary/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"ping"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 via fallback, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "from-fallback") {
		t.Fatalf("response did not come from fallback: %s", body)
	}
	if primaryHits.Load() == 0 || fallbackHits.Load() == 0 {
		t.Errorf("expected both primary and fallback to be hit (primary=%d fallback=%d)",
			primaryHits.Load(), fallbackHits.Load())
	}
}

// TestHealthAndStats: the operational endpoints answer on a booted server.
func TestHealthAndStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	base := boot(t, fmt.Sprintf(`{"providers": {"oai": {"url": %q, "format": "openai"}}}`, upstream.URL))

	// /health
	hr, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := io.ReadAll(hr.Body)
	hr.Body.Close()
	if hr.StatusCode != http.StatusOK || !strings.Contains(string(hb), `"ok"`) {
		t.Fatalf("/health: status=%d body=%s", hr.StatusCode, hb)
	}

	// Drive one request so /stats has something to count.
	post(t, base+"/provider/oai/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"ping"}]}`)

	sr, err := http.Get(base + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := io.ReadAll(sr.Body)
	sr.Body.Close()
	var stats map[string]any
	if err := json.Unmarshal(sb, &stats); err != nil {
		t.Fatalf("/stats not JSON: %v (%s)", err, sb)
	}
	if _, ok := stats["total_requests"]; !ok {
		t.Errorf("/stats missing total_requests: %s", sb)
	}
}
