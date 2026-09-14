package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestBridgePluginsCanBlockAndRespondWithoutUpstream(t *testing.T) {
	for _, name := range []string{"test-responder", "test-blocker"} {
		requireWASM(t, "../../examples/plugins/"+name+"/plugin.wasm")
	}
	for _, client := range bridgeProtocols {
		t.Run(string(client), func(t *testing.T) {
			cfg := provider.Config{
				Providers: map[string]provider.Provider{"p": {
					URL: "http://127.0.0.1:1", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"},
					Bridge: &provider.BridgeConfig{Client: client, Upstream: bridge.OpenAIChat, Model: "local-model"},
				}},
				Plugins: provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-responder", "test-blocker"}, AllowUnapproved: true},
			}
			srv, err := New(Config{Providers: cfg})
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(srv.Handler())
			t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprint(streaming), func(t *testing.T) {
					request := strings.ReplaceAll(bridgeClientBody(client, false, streaming), "look up weather", "respondme please")
					status, raw, headers := callBridge(t, proxy, client, request, streaming)
					if status != 200 || !strings.Contains(string(raw), "canned response from test-responder") {
						t.Fatalf("status=%d body=%s", status, raw)
					}
					if streaming {
						if headers.Get("Content-Type") != "text/event-stream" {
							t.Fatalf("stream headers: %v", headers)
						}
						if client == bridge.OpenAIResponses && (!strings.Contains(string(raw), `"created_at"`) || !strings.Contains(string(raw), `"parallel_tool_calls"`)) {
							t.Fatalf("required stream envelope missing: %s", raw)
						}
					} else {
						var body map[string]json.RawMessage
						if json.Unmarshal(raw, &body) != nil {
							t.Fatalf("invalid JSON: %s", raw)
						}
						if client == bridge.OpenAIChat && body["created"] == nil {
							t.Fatalf("Chat timestamp missing: %s", raw)
						}
						if client == bridge.OpenAIResponses && (body["created_at"] == nil || body["parallel_tool_calls"] == nil || body["tools"] == nil || body["tool_choice"] == nil) {
							t.Fatalf("Responses envelope missing: %s", raw)
						}
					}
				})
			}
			request := strings.ReplaceAll(bridgeClientBody(client, false, false), "look up weather", "blockme please")
			status, raw, _ := callBridge(t, proxy, client, request, false)
			if status < 400 || !strings.Contains(string(raw), "test-blocker") {
				t.Fatalf("local block lost original reason: status=%d body=%s", status, raw)
			}
		})
	}
}
