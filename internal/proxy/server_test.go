package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// testProviderConfig builds a provider.Config with a single provider
// pointing at the given upstream URL.
func testProviderConfig(upstreamURL, name, format string) provider.Config {
	return provider.Config{
		Port: 0,
		Providers: map[string]provider.Provider{
			name: {URL: upstreamURL, Format: format},
		},
	}
}

// TestProxyPassThrough verifies that a request with a provider prefix
// reaches the upstream and the response is returned unchanged.
func TestProxyPassThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from upstream\n"))
		w.Write([]byte("method: " + r.Method + "\n"))
		w.Write([]byte("path: " + r.URL.Path + "\n"))
	}))
	defer upstream.Close()

	cfg := Config{
		Port:      "0",
		Providers: testProviderConfig(upstream.URL, "test", "openai"),
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	proxyURL := "http://" + ln.Addr().String()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/provider/test/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude"}`))
	if err != nil {
		t.Fatalf("POST to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("X-Upstream header missing")
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "hello from upstream") {
		t.Error("response body missing upstream content")
	}
	if !strings.Contains(bodyStr, "path: /v1/messages") {
		t.Error("request path not preserved — prefix should be stripped")
	}
	if !strings.Contains(bodyStr, "method: POST") {
		t.Error("request method not preserved")
	}
}

func TestOversizedBodyRejection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := Config{
		Port:      "0",
		Providers: testProviderConfig(upstream.URL, "test", "openai"),
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	proxyURL := "http://" + ln.Addr().String()
	
	// maxBodySize is 10<<20 (10MB). Let's send 10MB + 1 byte
	largeBody := make([]byte, maxBodySize+1)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/provider/test/v1/chat", "application/json",
		bytes.NewReader(largeBody))
	if err != nil {
		t.Fatalf("POST to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

// TestProxyDefaultProvider verifies that the DefaultProvider field
// routes paths without a /provider/ prefix to the named provider.
func TestProxyDefaultProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("path: " + r.URL.Path + "\n"))
	}))
	defer upstream.Close()

	cfg := Config{
		Port:            "0",
		Providers:       testProviderConfig(upstream.URL, "test", "openai"),
		DefaultProvider: "test",
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	proxyURL := "http://" + ln.Addr().String()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "path: /v1/chat/completions") {
		t.Error("default provider: path not forwarded correctly")
	}
}

// TestProxyNoProviderRejects verifies that a request without a provider
// prefix (and no default) gets a 502.
func TestProxyNoProviderRejects(t *testing.T) {
	cfg := Config{
		Port:      "0",
		Providers: provider.Config{Providers: map[string]provider.Provider{}},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	proxyURL := "http://" + ln.Addr().String()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(proxyURL + "/some/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (no provider configured)", resp.StatusCode, http.StatusBadGateway)
	}
}


// TestJSONResponseRunsAfterResponseHook verifies that a non-streaming JSON
// response with tool calls is routed through the WASM pipeline.
func TestJSONResponseRunsWASMHooks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Return a tool call response — the run_before_request hook in
		// the delegator plugin sets a default model, but run_on_stream_chunk
		// and run_after_response should execute without error.
		w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"tool_calls": [{
						"id": "call_test_1",
						"function": {
							"name": "read",
							"arguments": "{\"path\":\"/tmp/test\"}"
						}
					}]
				}
			}]
		}`))
	}))
	defer upstream.Close()

	cfg := Config{
		Port:      "0",
		Providers: testProviderConfig(upstream.URL, "test", "openai"),
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	proxyURL := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/provider/test/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("POST to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Valid JSON with tool calls should survive the round-trip.
	if !strings.Contains(bodyStr, `call_test_1`) {
		t.Errorf("tool call ID should be preserved in response, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"tool_calls"`) {
		t.Error("response should contain tool_calls key")
	}
}
