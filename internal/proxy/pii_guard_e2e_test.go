package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// This crosses the real provider adapter, compiled official WASM guest, host
// result envelope, write-grant verifier, and upstream serializer. It prevents
// an in-memory unit test from passing when a guest mutates its request but
// accidentally returns pass-through, which discards that mutation at the ABI.
func TestPIIGuardCompiledGuestForwardsRecoverableToolError(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii_guard")

	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bridgeUpstreamJSON(bridge.OpenAIChat, false)))
	}))
	t.Cleanup(upstream.Close)

	digest, err := plugin.BundleDigestForDir(bundles + "/pii_guard")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins: provider.PluginsConfig{
			Dir: bundles, Order: []string{"pii_guard"},
			Config: map[string]json.RawMessage{"pii_guard": json.RawMessage(`{"tools":["*"]}`)},
			Approvals: map[string]provider.PluginApproval{"pii_guard": {
				Digest: digest, Permissions: manifestPermissions(bundles + "/pii_guard"), FailureMode: "block",
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(listener)

	const secret = "PAYMENT_API_KEY=sk_test_fixture_secret_123456"
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Post("http://"+listener.Addr().String()+"/provider/oai/v1/chat/completions", "application/json", strings.NewReader(toolConvo(secret)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	select {
	case wire := <-received:
		if strings.Contains(wire, secret) || !strings.Contains(wire, "Sensitive output withheld") {
			t.Fatalf("compiled guest mutation was lost or leaked: %s", wire)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive the recoverable request")
	}
}

func TestPIIGuardCompiledGuestCrossProtocolForwardsRecoverableToolError(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii_guard")
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bridgeUpstreamJSON(bridge.OpenAIChat, false)))
	}))
	t.Cleanup(upstream.Close)
	digest, err := plugin.BundleDigestForDir(bundles + "/pii_guard")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Providers: provider.Config{
		Providers: map[string]provider.Provider{"p": {
			URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"},
			Bridge: &provider.BridgeConfig{Client: bridge.Anthropic, Upstream: bridge.OpenAIChat, Model: "upstream-model"},
		}},
		Plugins: provider.PluginsConfig{
			Dir: bundles, Order: []string{"pii_guard"},
			Config: map[string]json.RawMessage{"pii_guard": json.RawMessage(`{"tools":["*"]}`)},
			Approvals: map[string]provider.PluginApproval{"pii_guard": {
				Digest: digest, Permissions: manifestPermissions(bundles + "/pii_guard"), FailureMode: "block",
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	body := `{"model":"client-model","max_tokens":64,"messages":[
		{"role":"user","content":"read the file"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"PAYMENT_API_KEY=sk_test_fixture_secret_123456"}]}
	]}`
	response, err := http.Post(proxy.URL+"/provider/p/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	select {
	case wire := <-received:
		if strings.Contains(wire, "sk_test_fixture_secret_123456") || !strings.Contains(wire, "Sensitive output withheld") {
			t.Fatalf("cross-protocol guard leaked or lost diagnostic: %s", wire)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridged upstream did not receive recoverable request")
	}
}

func TestPIIGuardCompiledGuestProtectsCodexCustomToolOutput(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii_guard")
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-2","object":"response","status":"completed","model":"m","output":[{"type":"message","id":"msg-1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"continued","annotations":[]}]}]}`))
	}))
	t.Cleanup(upstream.Close)
	digest, err := plugin.BundleDigestForDir(bundles + "/pii_guard")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}},
		Plugins: provider.PluginsConfig{
			Dir: bundles, Order: []string{"pii_guard"},
			Config: map[string]json.RawMessage{"pii_guard": json.RawMessage(`{"tools":["*"]}`)},
			Approvals: map[string]provider.PluginApproval{"pii_guard": {
				Digest: digest, Permissions: manifestPermissions(bundles + "/pii_guard"), FailureMode: "block",
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	body := `{"model":"m","input":[
		{"type":"custom_tool_call","call_id":"call-1","name":"exec","input":"cat .keys"},
		{"type":"custom_tool_call_output","call_id":"call-1","output":[
			{"type":"input_text","text":"Script completed"},
			{"type":"input_text","text":"PAYMENT_API_KEY=sk_test_custom_tool_secret_123456"}
		]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"injected harness metadata"}]}
	],"client_metadata":{"thread_id":"thread-stable"}}`
	response, err := http.Post(proxy.URL+"/provider/oai/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	select {
	case wire := <-received:
		if strings.Contains(wire, "sk_test_custom_tool_secret_123456") || !strings.Contains(wire, "Sensitive output withheld") {
			t.Fatalf("Codex custom tool output leaked or lost diagnostic: %s", wire)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive protected Codex request")
	}
}
