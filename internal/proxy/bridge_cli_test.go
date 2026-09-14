package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// Exercise the real CLI and running host, including persistence and inference:
// merely accepting bridge JSON in the CLI would not prove runtime parity.
func TestBridgeCLIConfigParity(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		protocol := bridge.OpenAIChat
		switch r.URL.Path {
		case "/v1/chat/completions":
		case "/v1/messages":
			protocol = bridge.Anthropic
		case "/v1/responses":
			protocol = bridge.OpenAIResponses
		case "/v1beta/models/cli-upstream:generateContent":
			protocol = bridge.Gemini
		case "/v1internal:generateContent":
			protocol = bridge.GeminiCodeAssist
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		chat, err := bridge.ParseRequest(protocol, body, r.URL.Path)
		if err != nil {
			t.Errorf("invalid upstream request: %v", err)
		} else if chat.Model != "cli-upstream" || (protocol == bridge.Anthropic && (chat.MaxTokens == nil || *chat.MaxTokens != 64)) {
			t.Errorf("configured model/token default not applied: %s", body)
		}
		if protocol == bridge.GeminiCodeAssist && !strings.Contains(string(body), `"project":"cli-project"`) {
			t.Errorf("configured Code Assist project not applied: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bridgeUpstreamJSON(protocol, false))
	}))
	defer upstream.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	native := provider.Provider{URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}
	cfg := provider.DefaultConfig()
	cfg.Providers = map[string]provider.Provider{"p": native}
	srv, err := New(Config{Port: "8080", ConfigPath: configPath, Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	type snapshot struct {
		Revision string                     `json:"revision"`
		Config   map[string]json.RawMessage `json:"config"`
	}
	get := func() snapshot {
		t.Helper()
		raw, err := runControlCLI(t, proxy.URL, "", "config", "get")
		if err != nil {
			t.Fatal(err)
		}
		var s snapshot
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	apply := func(s snapshot) error {
		t.Helper()
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		_, err = runControlCLI(t, proxy.URL, string(raw), "config", "apply", "--file", "-", "--yes")
		return err
	}
	for _, b := range []provider.BridgeConfig{
		{Client: bridge.Anthropic, Upstream: bridge.OpenAIChat, Model: "cli-upstream"},
		{Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic, Model: "cli-upstream", MaxTokens: 64},
		{Client: bridge.OpenAIChat, Upstream: bridge.GeminiCodeAssist, Model: "cli-upstream", Project: "cli-project"},
		{Client: bridge.Gemini, Upstream: bridge.OpenAIResponses, Model: "cli-upstream"},
		{Client: bridge.GeminiCodeAssist, Upstream: bridge.Gemini, Model: "cli-upstream"},
	} {
		t.Run(string(b.Upstream), func(t *testing.T) {
			edit := get()
			want := native
			want.Format, want.Bridge = b.Upstream.Format(), &b
			edit.Config["providers"], _ = json.Marshal(map[string]provider.Provider{"p": want})
			if err := apply(edit); err != nil {
				t.Fatal(err)
			}
			stored, err := provider.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			var exported map[string]provider.Provider
			if err := json.Unmarshal(get().Config["providers"], &exported); err != nil {
				t.Fatal(err)
			}
			for where, got := range map[string]provider.Provider{"live": srv.GetConfig().Providers.Providers["p"], "stored": stored.Providers["p"], "CLI": exported["p"]} {
				if got.Bridge == nil || *got.Bridge != b || got.Format != want.Format || got.Auth != want.Auth {
					t.Errorf("%s lost bridge settings: %+v", where, got)
				}
			}
			if err := apply(edit); err == nil || !strings.Contains(err.Error(), "fresh snapshot") {
				t.Fatalf("stale bridge edit was not refused: %v", err)
			}
			body := bridgeClientBody(b.Client, false, false)
			if b.Upstream == bridge.Anthropic {
				body = strings.Replace(body, `,"max_output_tokens":128`, "", 1)
			}
			status, raw, _ := callBridge(t, proxy, b.Client, body, false)
			if status != http.StatusOK || !strings.Contains(string(raw), "It is sunny") {
				t.Fatalf("CLI-configured bridge: status=%d body=%s", status, raw)
			}
		})
	}
	if calls.Load() != 5 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	// The host must apply the same credential-family validation as config
	// files. Rejection must leave both the runtime and managed store intact.
	before := get()
	invalid := native
	invalid.Bridge = &provider.BridgeConfig{Client: bridge.Anthropic, Upstream: bridge.OpenAIChat}
	invalid.Auth.Mode = "caller"
	bad := get()
	bad.Config["providers"], _ = json.Marshal(map[string]provider.Provider{"p": invalid})
	if err := apply(bad); err == nil {
		t.Fatal("CLI bypassed bridge credential validation")
	}
	if after := get(); after.Revision != before.Revision || string(after.Config["providers"]) != string(before.Config["providers"]) {
		t.Fatal("invalid bridge edit changed configuration")
	}
	persisted, err := provider.Load(configPath)
	if err != nil || persisted.Providers["p"].Bridge == nil || *persisted.Providers["p"].Bridge != *srv.GetConfig().Providers.Providers["p"].Bridge {
		t.Fatalf("invalid bridge edit changed the stored configuration: %v", err)
	}
	// Omitting an unmanaged field preserves it. Explicit null is the same
	// removal operation accepted by the API and must be expressible in CLI JSON.
	removal := get()
	var providers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(removal.Config["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	providers["p"]["bridge"] = json.RawMessage(`null`)
	removal.Config["providers"], _ = json.Marshal(providers)
	if err := apply(removal); err != nil {
		t.Fatal(err)
	}
	stored, err := provider.Load(configPath)
	if err != nil || stored.Providers["p"].Bridge != nil || srv.GetConfig().Providers.Providers["p"].Bridge != nil {
		t.Fatalf("bridge removal did not persist: err=%v stored=%+v", err, stored.Providers["p"])
	}
}
