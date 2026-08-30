package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestUsageLoggerWritesRealPrivateFile closes the first-run showcase loop: a
// real provider response crosses the real proxy and official WASM bundle, and
// the bundle can append only its approved, content-free private record.
func TestUsageLoggerWritesRealPrivateFile(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "usage_logger")

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"response-secret","model":"served-model","choices":[{"message":{"role":"assistant","content":"response-secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":7}}}`)
	}))
	t.Cleanup(upstream.Close)

	srv, err := New(Config{
		ConfigPath: t.TempDir() + "/config.json",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{
				"test": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
			},
			Plugins: provider.PluginsConfig{
				Dir:             bundles,
				Order:           []string{"usage_logger"},
				AllowUnapproved: true,
			},
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
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	body := `{"model":"requested-model","messages":[{"role":"user","content":"request-secret"}]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/provider/test/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	responseBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), responseBody)
	}

	data, err := srv.pluginFiles.OperatorRead("usage_logger", "usage.jsonl")
	if err != nil {
		t.Fatalf("read usage log: %v", err)
	}
	if strings.Count(string(data), "\n") != 1 {
		t.Fatalf("usage log = %q, want exactly one JSON line", data)
	}
	var record struct {
		Timestamp        string `json:"timestamp"`
		RequestID        string `json:"request_id"`
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		Status           int32  `json:"status"`
		DurationMS       int64  `json:"duration_ms"`
		InputTokens      int32  `json:"input_tokens"`
		OutputTokens     int32  `json:"output_tokens"`
		CacheReadTokens  int32  `json:"cache_read_tokens"`
		CacheWriteTokens int32  `json:"cache_write_tokens"`
		UsageReported    bool   `json:"usage_reported"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		t.Fatalf("decode usage record: %v; data=%s", err, data)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("usage record has trailing JSON: %v; data=%s", err, data)
	}
	if _, err := strconv.ParseUint(record.RequestID, 10, 64); err != nil || record.RequestID == "0" {
		t.Fatalf("request_id = %q", record.RequestID)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.Timestamp); err != nil {
		t.Fatalf("timestamp = %q: %v", record.Timestamp, err)
	}
	if record.Provider != "test" || record.Model != "served-model" || record.Status != 200 ||
		record.DurationMS < 0 || record.InputTokens != 10 || record.OutputTokens != 3 ||
		record.CacheReadTokens != 7 || record.CacheWriteTokens != 0 || !record.UsageReported {
		t.Fatalf("usage record = %+v", record)
	}
	for _, secret := range []string{"request-secret", "response-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("usage log contains content %q: %s", secret, data)
		}
	}
}
