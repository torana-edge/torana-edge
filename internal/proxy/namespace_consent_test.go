package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestNamespaceConsentDoesNotExecuteOrExposeConfirmation(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.Plugins = provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-http-server"}, AllowUnapproved: true}
	server, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	registry, err := server.currentNamespaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newNamespaceAccessPolicy(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed := false
	dispatch := operationDispatch{policy: policy, propose: server.proposeNamespaceOperation, execute: func(context.Context, operationCall) (any, *mcpserver.DomainError, error) {
		executed = true
		return nil, nil, nil
	}}
	input := json.RawMessage(`{"namespace":"test-http-server","operation":"_disable"}`)
	unbound, err := dispatch.invoke(context.Background(), input, plugin.MCPBinding{})
	if err != nil || unbound.Error == nil || unbound.Error.Code != "unbound_conversation" {
		t.Fatalf("unbound=%+v %v", unbound, err)
	}
	binding := plugin.MCPBinding{Bound: true, ConversationID: "host-session", CallID: "host-call"}
	result, err := dispatch.invoke(context.Background(), input, binding)
	if err != nil || !result.OK || result.Status != "pending_confirmation" || executed {
		t.Fatalf("result=%+v executed=%v err=%v", result, executed, err)
	}
	items, err := server.suggestions.List(binding.ConversationID, "torana", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("suggestions=%+v %v", items, err)
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{items[0].Code, items[0].ID, binding.ConversationID, binding.CallID} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("model response leaked %q: %s", private, encoded)
		}
	}
	// Identical input against the same snapshot refreshes one proposal, never
	// creates another consent code or mutates the enabled pipeline.
	if _, err := dispatch.invoke(context.Background(), input, binding); err != nil {
		t.Fatal(err)
	}
	again, _ := server.suggestions.List(binding.ConversationID, "torana", 0)
	if len(again) != 1 || again[0].ID != items[0].ID || again[0].Code != items[0].Code {
		t.Fatalf("refresh=%+v", again)
	}
	if len(server.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatal("proposal changed configuration")
	}
	entry := registry.entries["test-http-server"]
	entry.Digest = "sha256:stale"
	registry.entries[entry.Name] = entry
	stale, err := dispatch.invoke(context.Background(), input, binding)
	if err != nil || stale.Error == nil || stale.Error.Code != "stale_digest" {
		t.Fatalf("stale=%+v %v", stale, err)
	}
}
