package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startUsageProxy boots a proxy against the given upstream and returns its
// base URL plus a shutdown func.
func startUsageProxy(t *testing.T, upstreamURL string) string {
	t.Helper()
	srv, err := New(Config{Port: "0", Providers: testProviderConfig(upstreamURL, "test", "openai")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()); ln.Close() })
	return "http://" + ln.Addr().String()
}

// statsSnapshot fetches /stats and returns the decoded counters.
func statsSnapshot(t *testing.T, proxyURL string) map[string]any {
	t.Helper()
	resp, err := http.Get(proxyURL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}
	return m
}

const openaiUsageSSE = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
	"data: [DONE]\n\n"

// TestStreamUsageInjectedAndSuppressed: when an openai client streams WITHOUT
// asking for usage, the proxy opts in upstream (stream_options.include_usage),
// meters the tokens, and strips the usage frame from the client's stream — the
// client receives exactly the stream shape it asked for.
func TestStreamUsageInjectedAndSuppressed(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, openaiUsageSSE)
	}))
	defer upstream.Close()

	proxyURL := startUsageProxy(t, upstream.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/provider/test/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	mu.Lock()
	ub := upstreamBody
	mu.Unlock()
	if !strings.Contains(ub, `"stream_options"`) || !strings.Contains(ub, `"include_usage":true`) {
		t.Errorf("proxy should inject stream_options.include_usage upstream, sent: %s", ub)
	}
	if strings.Contains(string(body), "prompt_tokens") {
		t.Errorf("injected usage frame must be suppressed from the client stream:\n%s", body)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("client stream missing [DONE]:\n%s", body)
	}
	if !strings.Contains(string(body), "hi") {
		t.Errorf("client stream missing content:\n%s", body)
	}

	stats := statsSnapshot(t, proxyURL)
	if stats["total_tokens_in"].(float64) != 10 || stats["total_tokens_out"].(float64) != 5 {
		t.Errorf("tokens not metered: in=%v out=%v", stats["total_tokens_in"], stats["total_tokens_out"])
	}
}

// TestStreamUsagePassThroughWhenClientAsks: a client that opted into usage
// itself keeps receiving the usage frame, and the proxy doesn't double-inject.
func TestStreamUsagePassThroughWhenClientAsks(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, openaiUsageSSE)
	}))
	defer upstream.Close()

	proxyURL := startUsageProxy(t, upstream.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/provider/test/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-x","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	mu.Lock()
	ub := upstreamBody
	mu.Unlock()
	if strings.Count(ub, "include_usage") != 1 {
		t.Errorf("client's own stream_options should pass through un-duplicated: %s", ub)
	}
	if !strings.Contains(string(body), `"prompt_tokens":10`) {
		t.Errorf("client asked for usage — the frame must reach it:\n%s", body)
	}

	stats := statsSnapshot(t, proxyURL)
	if stats["total_tokens_in"].(float64) != 10 || stats["total_tokens_out"].(float64) != 5 {
		t.Errorf("tokens not metered: in=%v out=%v", stats["total_tokens_in"], stats["total_tokens_out"])
	}
}

// TestJSONResponseUsageRecorded: non-streaming responses have their usage
// object read (body untouched) and metered into /stats.
func TestJSONResponseUsageRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}))
	defer upstream.Close()

	proxyURL := startUsageProxy(t, upstream.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/provider/test/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"prompt_tokens":7`) {
		t.Errorf("usage object must survive in the response body:\n%s", body)
	}

	stats := statsSnapshot(t, proxyURL)
	if stats["total_tokens_in"].(float64) != 7 || stats["total_tokens_out"].(float64) != 3 {
		t.Errorf("tokens not metered: in=%v out=%v", stats["total_tokens_in"], stats["total_tokens_out"])
	}
}
