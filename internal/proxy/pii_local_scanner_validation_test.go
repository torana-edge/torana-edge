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

	"github.com/torana-edge/torana-edge/internal/bridge"
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
	for _, shape := range []bridge.Protocol{bridge.Anthropic, bridge.OpenAIChat, bridge.OpenAIResponses, bridge.Gemini, bridge.GeminiCodeAssist} {
		t.Run(string(shape), func(t *testing.T) { testPIILocalScannerShape(t, target, model, shape) })
	}
}

func testPIILocalScannerShape(t *testing.T, target *url.URL, model string, shape bridge.Protocol) {
	t.Helper()
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
		_, _ = io.WriteString(w, bridgeUpstreamJSON(shape, false))
	}))
	t.Cleanup(primary.Close)

	srv, err := New(Config{Providers: provider.Config{
		Providers: map[string]provider.Provider{
			"primary": {URL: primary.URL, Format: shape.Format(), Auth: provider.ProviderAuth{Mode: "caller"}},
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
	fixture, path, carriers := localScannerShapeFixture(shape)
	var document map[string]any
	if err := json.Unmarshal([]byte(strings.ReplaceAll(fixture, "SYNTHETIC_SECRET", secret)), &document); err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < 3; turn++ {
		body, marshalErr := json.Marshal(document)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		req, requestErr := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/provider/primary"+path, strings.NewReader(string(body)))
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
		appendLocalScannerTurn(document, shape)
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
	var firstReplacement string
	for turn, wire := range upstreamBodies {
		if strings.Contains(wire, secret) || !strings.Contains(wire, "Tool output withheld") || !strings.Contains(wire, "password") || strings.Contains(wire, "not a confirmed finding") {
			t.Fatalf("turn %d: real scanner must yield a recoverable password finding, no secret/failure-only diagnostic: %s", turn, wire)
		}
		if !strings.Contains(wire, "line 1 of this tool output") || !strings.Contains(wire, "`Read` output") || !strings.Contains(wire, "Reported lines are not verified file positions") {
			t.Fatalf("turn %d: finding must identify Read and output-relative line 1 without promising a file position: %s", turn, wire)
		}
		for _, carrier := range carriers {
			if !strings.Contains(wire, carrier) {
				t.Fatalf("turn %d: required error/cache/signature carrier %s lost: %s", turn, carrier, wire)
			}
		}
		var forwarded map[string]any
		if err := json.Unmarshal([]byte(wire), &forwarded); err != nil {
			t.Fatal(err)
		}
		var protectedSlot any
		switch shape {
		case bridge.Anthropic, bridge.OpenAIChat:
			protectedSlot = forwarded["messages"].([]any)[2]
		case bridge.OpenAIResponses:
			protectedSlot = forwarded["input"].([]any)[2]
		case bridge.Gemini, bridge.GeminiCodeAssist:
			if shape == bridge.GeminiCodeAssist {
				forwarded = forwarded["request"].(map[string]any)
			}
			protectedSlot = forwarded["contents"].([]any)[2]
		}
		replacement, err := json.Marshal(protectedSlot)
		if err != nil {
			t.Fatal(err)
		}
		if turn == 0 {
			firstReplacement = string(replacement)
		} else if string(replacement) != firstReplacement {
			t.Fatalf("turn %d: historical replacement changed, risking prompt-cache prefix drift", turn)
		}
	}
	t.Logf("real local model %s: production default-derived model/path on upstream /v1, native %s transformation, three turns, one inference, no caller-auth leak, preserved supported carriers; synthetic primary", model, shape)
}

func localScannerShapeFixture(shape bridge.Protocol) (string, string, []string) {
	switch shape {
	case bridge.Anthropic:
		return `{"model":"synthetic-primary","max_tokens":64,"messages":[{"role":"user","content":"Review this synthetic configuration."},{"role":"assistant","content":[{"type":"tool_use","id":"local_scan_call","name":"Read","input":{"file_path":"synthetic-config.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"local_scan_call","content":"DB_PASSWORD=SYNTHETIC_SECRET","cache_control":{"type":"ephemeral"}}]}]}`, "/v1/messages", []string{`"is_error":true`, `"cache_control":{"type":"ephemeral"}`}
	case bridge.OpenAIChat:
		return `{"model":"synthetic-primary","prompt_cache_key":"synthetic-cache-key","messages":[{"role":"user","content":"Review this synthetic configuration."},{"role":"assistant","tool_calls":[{"id":"local_scan_call","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"local_scan_call","content":"DB_PASSWORD=SYNTHETIC_SECRET"}]}`, "/v1/chat/completions", []string{`"prompt_cache_key":"synthetic-cache-key"`, `"tool_call_id":"local_scan_call"`}
	case bridge.OpenAIResponses:
		return `{"model":"synthetic-primary","prompt_cache_key":"synthetic-cache-key","input":[{"type":"message","role":"user","content":"Review this synthetic configuration."},{"type":"custom_tool_call","call_id":"local_scan_call","name":"Read","input":"synthetic-config.txt"},{"type":"custom_tool_call_output","call_id":"local_scan_call","output":"DB_PASSWORD=SYNTHETIC_SECRET"}],"client_metadata":{"thread_id":"synthetic-thread"}}`, "/v1/responses", []string{`"prompt_cache_key":"synthetic-cache-key"`, `"type":"custom_tool_call_output"`, `"call_id":"local_scan_call"`}
	default:
		fixture := `{"cachedContent":"cachedContents/synthetic","contents":[{"role":"user","parts":[{"text":"Review this synthetic configuration."}]},{"role":"model","parts":[{"thoughtSignature":"c3ludGhldGljLXNpZw==","functionCall":{"id":"local_scan_call","name":"Read","args":{"file_path":"synthetic-config.txt"}}}]},{"role":"user","parts":[{"functionResponse":{"id":"local_scan_call","name":"Read","response":{"output":"DB_PASSWORD=SYNTHETIC_SECRET"}}}]}]}`
		path := "/v1beta/models/synthetic-primary:generateContent"
		if shape == bridge.GeminiCodeAssist {
			fixture = `{"model":"synthetic-primary","project":"synthetic-project","request":` + fixture + `}`
			path = "/v1internal:generateContent"
		}
		return fixture, path, []string{`"cachedContent":"cachedContents/synthetic"`, `"thoughtSignature":"c3ludGhldGljLXNpZw=="`, `"error":`}
	}
}

func appendLocalScannerTurn(document map[string]any, shape bridge.Protocol) {
	key := "messages"
	assistant := map[string]any{"role": "assistant", "content": "done"}
	user := map[string]any{"role": "user", "content": "Continue without rereading the file."}
	switch shape {
	case bridge.OpenAIResponses:
		key = "input"
		assistant["type"], user["type"] = "message", "message"
	case bridge.Gemini, bridge.GeminiCodeAssist:
		key = "contents"
		if shape == bridge.GeminiCodeAssist {
			document = document["request"].(map[string]any)
		}
		assistant = map[string]any{"role": "model", "parts": []any{map[string]any{"text": "done"}}}
		user = map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Continue without rereading the file."}}}
	}
	document[key] = append(document[key].([]any), assistant, user)
}
