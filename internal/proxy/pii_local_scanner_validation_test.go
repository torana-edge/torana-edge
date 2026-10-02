//go:build torana_local_models

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// This opt-in check uses the real production model-service egress, not an SDK
// callback stub. Only synthetic tool output reaches an explicitly chosen local
// scanner. The primary provider and all approval state are test-owned.
func TestPIILocalScannerProductionEgress(t *testing.T) {
	scannerURL := os.Getenv("TORANA_TEST_LOCAL_SCANNER_URL")
	if scannerURL == "" {
		t.Skip("opt-in local model check: set TORANA_TEST_LOCAL_SCANNER_URL and TORANA_TEST_LOCAL_SCANNER_MODEL")
	}
	target, err := url.Parse(scannerURL)
	if err != nil || target.Scheme != "http" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		t.Fatal("local scanner must be an unauthenticated loopback HTTP origin")
	}
	ip := net.ParseIP(target.Hostname())
	if target.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("local scanner must be loopback; synthetic scan never targets a hosted model")
	}
	model := os.Getenv("TORANA_TEST_LOCAL_SCANNER_MODEL")
	if model == "" {
		t.Fatal("TORANA_TEST_LOCAL_SCANNER_MODEL is required")
	}
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii")
	digest, err := plugin.BundleDigestForDir(bundles + "/pii")
	if err != nil {
		t.Fatal(err)
	}

	const secret = "synthetic-launch-password-7Gx9!"
	var scans atomic.Int32
	var mu sync.Mutex
	var scannerAuth, scannerInput, scannerPath string
	var upstreamBodies []string
	forward := httputil.NewSingleHostReverseProxy(target)
	scanner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scans.Add(1)
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read scanner request", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		scannerAuth, scannerInput, scannerPath = r.Header.Get("Authorization"), string(body), r.URL.Path
		mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(scanner.Close)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read primary request", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		upstreamBodies = append(upstreamBodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","model":"synthetic-primary","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	}))
	t.Cleanup(primary.Close)

	srv, err := New(Config{Providers: provider.Config{
		Providers: map[string]provider.Provider{
			"primary": {URL: primary.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "caller"}},
			"scanner": {URL: scanner.URL + "/v1", Format: "openai", DefaultModel: model, Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Plugins: provider.PluginsConfig{
			Dir: bundles, Order: []string{"pii"},
			Config: map[string]json.RawMessage{"pii": json.RawMessage(`{"tools":["*"],"on_error":"block"}`)},
			Approvals: map[string]provider.PluginApproval{"pii": {
				Digest: digest, Permissions: manifestPermissions(bundles + "/pii"), FailureMode: "block",
				ModelServices: map[string]provider.PluginModelServiceApproval{"scanner": {
					Provider: "scanner", TimeoutMS: 90_000,
					MaxTokens: 512, MaxInputBytes: 1 << 20, MaxCallsPerMinute: 60, MaxTokensPerHour: 100_000,
				}},
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	client := &http.Client{Timeout: 100 * time.Second}
	messages := []map[string]any{
		{"role": "user", "content": "Review this synthetic configuration."},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "local_scan_call", "name": "Read", "input": map[string]any{"file_path": "synthetic-config.txt"}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "local_scan_call", "content": "DB_PASSWORD=" + secret, "cache_control": map[string]any{"type": "ephemeral"}}}},
	}
	for turn := 0; turn < 3; turn++ {
		body, marshalErr := json.Marshal(map[string]any{"model": "synthetic-primary", "max_tokens": 64, "messages": messages})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		req, requestErr := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/provider/primary/v1/messages", strings.NewReader(string(body)))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer synthetic-caller-not-for-scanner")
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %d: status=%d read=%v body=%s", turn, resp.StatusCode, readErr, responseBody)
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": "done"}, map[string]any{"role": "user", "content": "Continue without rereading the file."})
	}
	mu.Lock()
	defer mu.Unlock()
	if scans.Load() != 1 || len(upstreamBodies) != 3 {
		t.Fatalf("scans=%d primary requests=%d; want one real inference and three forwarded turns", scans.Load(), len(upstreamBodies))
	}
	var scannerRequest struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(scannerInput), &scannerRequest); err != nil {
		t.Fatal(err)
	}
	if scannerAuth != "" || !strings.Contains(scannerInput, secret) || scannerRequest.Model != model || scannerPath != "/v1/chat/completions" {
		t.Fatalf("production scanner egress must receive synthetic output and configured model, never caller auth; auth_present=%t", scannerAuth != "")
	}
	for turn, wire := range upstreamBodies {
		if strings.Contains(wire, secret) || !strings.Contains(wire, "Tool output withheld") || !strings.Contains(wire, "password") || strings.Contains(wire, "not a confirmed finding") || !strings.Contains(wire, `"is_error":true`) || !strings.Contains(wire, `"cache_control":{"type":"ephemeral"}`) {
			t.Fatalf("turn %d: real scanner must yield a recoverable password finding with preserved cache marker, no secret/failure-only diagnostic: %s", turn, wire)
		}
	}
	t.Logf("real local model %s: production default-derived model/path on upstream /v1, native Anthropic transformation, three turns, one inference, no caller-auth leak, preserved cache marker", model)
}
