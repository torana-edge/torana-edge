package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// Runs with the real pii bundle built by torana-plugins CI, not a guest stub.
func TestPIIHumanReleaseSurvivesRestartWithoutRescan(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii")
	dataDir := t.TempDir()
	t.Setenv("TORANA_DATA_DIR", dataDir)
	var mu sync.Mutex
	var last string
	scans := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"model":"test","stop_reason":"end_turn"}`)
	}))
	defer upstream.Close()
	scanner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		scans++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"pii\":true,\"findings\":[{\"type\":\"unspecified\",\"line\":1}]}"}}]}`)
	}))
	defer scanner.Close()
	digest, err := plugin.BundleDigestForDir(bundles + "/pii")
	if err != nil {
		t.Fatal(err)
	}
	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"ant":     {URL: upstream.URL, Format: "anthropic"},
			"scanner": {URL: scanner.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Plugins: provider.PluginsConfig{Dir: bundles, Order: []string{"pii"}, Config: map[string]json.RawMessage{"pii": json.RawMessage(`{"tools":["*"]}`)},
			Approvals: map[string]provider.PluginApproval{"pii": {Digest: digest, Permissions: manifestPermissions(bundles + "/pii"), FailureMode: "block", ModelServices: map[string]provider.PluginModelServiceApproval{"scanner": {Provider: "scanner", Model: "test", TimeoutMS: 90000, MaxTokens: 512, MaxInputBytes: 1 << 20, MaxCallsPerMinute: 60, MaxTokensPerHour: 100000}}}}},
	}
	newServer := func() *Server {
		t.Helper()
		s, err := New(Config{Port: "8080", ConfigPath: filepath.Join(dataDir, "config.json"), Providers: cfg})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := newServer()
	defer func() { s.Shutdown(context.Background()) }()
	const original = "ordinary synthetic README content for human review"
	body := anthropicToolResultConvo([]map[string]any{{"type": "text", "text": original, "cache_control": map[string]string{"type": "ephemeral"}}})
	post := func() string {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/provider/ant/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	withheld := post()
	if strings.Contains(withheld, original) || !strings.Contains(withheld, "redactions.request_release") {
		t.Fatalf("result was not withheld: %s", withheld)
	}
	items, _, err := s.resultReleases.List("")
	if err != nil || len(items) != 1 {
		t.Fatalf("registrations=%+v %v", items, err)
	}
	item := items[0]
	policy, err := s.mcpPolicy()
	if err != nil {
		t.Fatal(err)
	}
	d := operationDispatch{policy: policy, propose: s.proposeNamespaceOperation}
	input, _ := json.Marshal(map[string]any{"namespace": "torana", "operation": "redactions.request_release", "input": map[string]string{"reference": item.Reference}})
	result, err := d.invoke(context.Background(), input, plugin.MCPBinding{Bound: true, ConversationID: item.Conversation, CallID: "mcp-review"})
	if err != nil || !result.OK || result.Status != "pending" || result.Consent != nil {
		t.Fatalf("review=%+v %v", result, err)
	}
	httpServer := httptest.NewServer(s.Handler())
	if err := reviewResultDecision(t, httpServer.URL, item.Reference, "pending", "approve"); err != nil {
		t.Fatal(err)
	}
	httpServer.Close()
	allowed := post()
	if !strings.Contains(allowed, original) || strings.Contains(allowed, "redactions.request_release") || !strings.Contains(allowed, "cache_control") {
		t.Fatalf("original/cache carrier not restored: %s", allowed)
	}
	s.Shutdown(context.Background())
	s = newServer()
	if resumed := post(); resumed != allowed {
		t.Fatalf("restart changed the allowed request prefix:\nallowed: %s\nresumed: %s", allowed, resumed)
	}
	httpServer = httptest.NewServer(s.Handler())
	defer httpServer.Close()
	if err := reviewResultDecision(t, httpServer.URL, item.Reference, "approved", "revoke"); err != nil {
		t.Fatal(err)
	}
	if revoked := post(); revoked != withheld {
		t.Fatal("revocation did not restore the exact earlier replacement")
	}
	mu.Lock()
	defer mu.Unlock()
	if scans != 1 {
		t.Fatalf("approval/restart/revocation rescanned: %d", scans)
	}
}
