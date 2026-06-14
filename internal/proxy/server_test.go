package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestProxyPassThrough verifies that a request sent to the Torana proxy
// reaches the upstream and the response is returned to the caller unchanged.
func TestProxyPassThrough(t *testing.T) {
	// 1. Fake upstream – a simple HTTP server that echoes back what it received.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from upstream\n"))
		w.Write([]byte("method: " + r.Method + "\n"))
		w.Write([]byte("path: " + r.URL.Path + "\n"))
	}))
	defer upstream.Close()

	// 2. Torana proxy pointed at the fake upstream.
	cfg := Config{
		Port:        "0", // OS picks a free port
		UpstreamURL: upstream.URL,
		Provider:    "anthropic",
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Bind to a random port so tests don't collide.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	proxyURL := "http://" + ln.Addr().String()

	// 3. Send a request through the proxy.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyURL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude"}`))
	if err != nil {
		t.Fatalf("POST to proxy: %v", err)
	}
	defer resp.Body.Close()

	// 4. Assert the response came through the proxy from upstream.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("X-Upstream header missing – proxy may not have forwarded to upstream")
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "hello from upstream") {
		t.Error("response body missing upstream content – proxy may not be forwarding")
	}
	if !strings.Contains(bodyStr, "path: /v1/messages") {
		t.Error("request path not preserved by proxy")
	}
	if !strings.Contains(bodyStr, "method: POST") {
		t.Error("request method not preserved by proxy")
	}
}

// TestProxyHealthEndpoint verifies that the proxy itself responds to /health
// when we add an explicit route (future-proofing — today there is no /health,
// so we verify a non-upstream path still gets forwarded).
func TestProxyNonExistentPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("upstream-404"))
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", UpstreamURL: upstream.URL, Provider: "openai"}
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

	resp, err := http.Get(proxyURL + "/some/random/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream-404") {
		t.Error("unexpected body – proxy should forward everything upstream")
	}
}

// TestConfigValidation verifies that an invalid upstream URL is caught at
// construction time, not later when the first request arrives.
func TestConfigValidation(t *testing.T) {
	cfg := Config{
		Port:        "8080",
		UpstreamURL: "not-a-valid-url%%%",
		Provider:    "anthropic",
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}
}
