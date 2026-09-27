package proxy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestConfirmedPluginMutationRechecksSnapshotAndPolicy(t *testing.T) {
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
	dispatch := operationDispatch{policy: policy, propose: server.proposeNamespaceOperation}
	binding := plugin.MCPBinding{Bound: true, ConversationID: "confirmed-session", CallID: "call"}
	result, err := dispatch.invoke(context.Background(), json.RawMessage(`{"namespace":"test-http-server","operation":"_disable"}`), binding)
	if err != nil || !result.OK {
		t.Fatalf("proposal=%+v %v", result, err)
	}
	items, err := server.suggestions.List(binding.ConversationID, "torana", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	id := items[0].ID
	result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, id, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("unaccepted executed: %+v %v", result, err)
	}
	if _, err := server.suggestions.ResolveCode(binding.ConversationID, items[0].Code, "accepted", "agent_api", 0); err != nil {
		t.Fatal(err)
	}
	server.configMu.Lock()
	server.config.Providers.Port++
	server.configMu.Unlock()
	result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, id, nil)
	if err != nil || result.Error == nil || result.Error.Code != "conflict" {
		t.Fatalf("stale accepted: %+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Port--
	server.configMu.Unlock()
	result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, id, []string{"test-http-server"})
	if err != nil || result.Error == nil || result.Error.Code != "access_denied" {
		t.Fatalf("changed floor ignored: %+v %v", result, err)
	}
	result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, id, nil)
	if err != nil || !result.OK || result.Status != "applied" {
		t.Fatalf("apply=%+v %v", result, err)
	}
	if len(server.GetConfig().Providers.Plugins.Order) != 0 {
		t.Fatal("accepted disable not applied")
	}
	changes, err := server.suggestions.ListChanges(binding.ConversationID)
	if err != nil || len(changes) != 1 || changes[0].Status != "applied" {
		t.Fatalf("history=%+v %v", changes, err)
	}
	result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, id, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("replayed execution=%+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Port++
	server.configMu.Unlock()
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].ID, nil)
	if err != nil || result.Error == nil || result.Error.Code != "conflict" {
		t.Fatalf("stale undo=%+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Port--
	server.configMu.Unlock()
	server.configMu.Lock()
	server.config.Providers.Plugins.Protected = []string{"test-http-server"}
	server.configMu.Unlock()
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].ID, nil)
	if err != nil || result.Error == nil || result.Error.Code != "conflict" {
		t.Fatalf("new protection did not invalidate undo: %+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Plugins.Protected = nil
	server.configMu.Unlock()
	result, err = server.undoConfirmedPluginChange(context.Background(), "other-session", items[0].ID, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("cross-session undo=%+v %v", result, err)
	}
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].ID, nil)
	if err != nil || !result.OK || result.Status != "undone" {
		t.Fatalf("undo=%+v %v", result, err)
	}
	if len(server.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatal("undo did not restore the enabled plugin")
	}
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].ID, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("duplicate undo=%+v %v", result, err)
	}
}

func TestConfirmedGuestMutationAppliesAndUsesDeclaredUndo(t *testing.T) {
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
	dispatch := operationDispatch{policy: policy, propose: server.proposeNamespaceOperation}
	binding := plugin.MCPBinding{Bound: true, ConversationID: "guest-session", CallID: "guest-call"}
	input := json.RawMessage(`{"namespace":"test-http-server","operation":"value.set","input":{"value":"next"}}`)
	result, err := dispatch.invoke(context.Background(), input, binding)
	if err != nil || !result.OK || result.Status != "pending_confirmation" {
		t.Fatalf("proposal=%+v err=%v", result, err)
	}
	items, err := server.suggestions.List(binding.ConversationID, "torana", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if _, err := server.suggestions.ResolveCode(binding.ConversationID, items[0].Code, "accepted", "agent_api", 0); err != nil {
		t.Fatal(err)
	}
	result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, items[0].ID, nil)
	if err != nil || !result.OK || result.Status != "applied" {
		code := ""
		if result.Error != nil {
			code = result.Error.Code
		}
		t.Fatalf("apply=%+v code=%s err=%v", result, code, err)
	}
	value, found, err := server.pluginState.Get("test-http-server", "reversible/current")
	if err != nil || !found || value != "next" {
		t.Fatalf("state=%q found=%t err=%v", value, found, err)
	}
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].ID, nil)
	if err != nil || !result.OK || result.Status != "undone" {
		t.Fatalf("undo=%+v err=%v", result, err)
	}
	if value, found, err := server.pluginState.Get("test-http-server", "reversible/current"); err != nil || found {
		t.Fatalf("state remained after undo: %q found=%t err=%v", value, found, err)
	}
	direct, err := dispatch.invoke(context.Background(), json.RawMessage(`{"namespace":"test-http-server","operation":"value.set.undo","input":{"value":"next"}}`), binding)
	if err != nil || direct.Error == nil || direct.Error.Code != "access_denied" {
		t.Fatalf("model reached undo companion: %+v err=%v", direct, err)
	}

	binding.CallID = "guest-call-conflict"
	result, err = dispatch.invoke(context.Background(), json.RawMessage(`{"namespace":"test-http-server","operation":"value.set","input":{"value":"second"}}`), binding)
	if err != nil || result.Status != "pending_confirmation" {
		t.Fatalf("second proposal=%+v err=%v", result, err)
	}
	items, err = server.suggestions.List(binding.ConversationID, "torana", 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("second items=%+v err=%v", items, err)
	}
	second := items[1]
	if _, err := server.suggestions.ResolveCode(binding.ConversationID, second.Code, "accepted", "agent_api", 0); err != nil {
		t.Fatal(err)
	}
	if result, err = server.applyConfirmedOperation(context.Background(), binding.ConversationID, second.ID, nil); err != nil || !result.OK {
		t.Fatalf("second apply=%+v err=%v", result, err)
	}
	if err := server.pluginState.Set("test-http-server", "reversible/current", "later"); err != nil {
		t.Fatal(err)
	}
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, second.ID, nil)
	if err != nil || result.Error == nil || result.Error.Code != "plugin_failed" {
		t.Fatalf("conflicting undo=%+v err=%v", result, err)
	}
	if value, found, err := server.pluginState.Get("test-http-server", "reversible/current"); err != nil || !found || value != "later" {
		t.Fatalf("conflicting undo overwrote later state: %q found=%t err=%v", value, found, err)
	}
}

func TestModelDispatcherCannotSubmitUserUndoCode(t *testing.T) {
	registry, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := registry.entries["torana"]
	for i := range entry.Operations {
		if entry.Operations[i].ID == "changes.undo" {
			entry.Operations[i].Callable = true
		}
	}
	registry.entries[entry.Name] = entry
	policy, err := newNamespaceAccessPolicy(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	dispatch := operationDispatch{policy: policy, propose: func(context.Context, operationCall) (mcpserver.Result, error) {
		called = true
		return mcpserver.Result{}, nil
	}}
	result, err := dispatch.invoke(context.Background(), json.RawMessage(`{"namespace":"torana","operation":"changes.undo","input":{"code":"abcd"}}`), plugin.MCPBinding{Bound: true, ConversationID: "host", CallID: "call"})
	if err != nil || called || result.Error == nil || result.Error.Code != "access_denied" {
		t.Fatalf("model undo code reached handler: %+v %v", result, err)
	}
	// Even a relaxed input schema or model access cannot turn the generic
	// dispatcher into a user undo-code executor.
	for _, access := range []string{"read", "confirm", "never"} {
		for i := range entry.Operations {
			if entry.Operations[i].ID == "changes.undo" {
				entry.Operations[i].ModelAccess = access
			}
		}
		registry.entries[entry.Name] = entry
		result, err := dispatch.invoke(context.Background(), json.RawMessage(`{"namespace":"torana","operation":"changes.undo","input":{}}`), plugin.MCPBinding{Bound: true, ConversationID: "host", CallID: "call"})
		if err != nil || called || result.Error == nil || result.Error.Code != "access_denied" {
			t.Fatalf("undo dispatcher bypass via %s: %+v %v", access, result, err)
		}
	}
}

func TestOperationRevisionSurvivesSecretStoreReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := secret.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{secrets: store}
	cfg := provider.DefaultConfig()
	first, err := server.operationRevision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server.secrets, err = secret.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.operationRevision(cfg)
	if err != nil || first != second {
		t.Fatalf("revision changed on restart: %q %q %v", first, second, err)
	}
	cfg.Port++
	third, err := server.operationRevision(cfg)
	if err != nil || third == first {
		t.Fatal("configuration change did not change durable revision")
	}
}
