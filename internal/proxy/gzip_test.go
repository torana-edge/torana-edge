package proxy

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestCompressedJSONResponseStillProcessed: a compressed upstream body must
// never bypass the response pipeline. Regression: Claude Code sends
// Accept-Encoding: gzip; the transport forwarded it, DeepSeek returned
// gzipped JSON, json.Unmarshal failed silently, and every response hook was
// skipped — plugin-injected tool-call fields leaked back to the harness.
// Two layers are pinned here: the Director forces Accept-Encoding: identity
// upstream, and the JSON path decompresses gzip anyway if an upstream
// ignores that.
func TestCompressedJSONResponseStillProcessed(t *testing.T) {
	sawEncoding := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawEncoding <- r.Header.Get("Accept-Encoding")
		// Ignore the negotiated encoding and compress regardless (the
		// defense-in-depth path).
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		io.WriteString(zw, `{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
		zw.Close()
	}))
	t.Cleanup(up.Close)

	srv, err := New(Config{
		Providers: provider.Config{
			Providers: map[string]provider.Provider{"oai": {URL: up.URL, Format: "openai"}},
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
	base := "http://" + ln.Addr().String()

	req, _ := http.NewRequest("POST", base+"/provider/oai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	// The caller negotiates compression — exactly what Claude Code does.
	req.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if enc := <-sawEncoding; enc != "identity" {
		t.Fatalf("Director must force Accept-Encoding: identity upstream, sent %q", enc)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("client received Content-Encoding %q for a decompressed body", enc)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("client body is not plain JSON (%v): %q", err, body)
	}

	// The decompressed body was actually parsed by the response path:
	// provider-reported usage reached the host meters.
	sresp, err := http.Get(base + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer sresp.Body.Close()
	var stats struct {
		TotalTokensIn  int `json:"total_tokens_in"`
		TotalTokensOut int `json:"total_tokens_out"`
	}
	if err := json.NewDecoder(sresp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.TotalTokensIn != 7 || stats.TotalTokensOut != 3 {
		t.Fatalf("usage not metered from decompressed body: in=%d out=%d", stats.TotalTokensIn, stats.TotalTokensOut)
	}
}
