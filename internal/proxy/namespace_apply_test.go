package proxy

import (
	"context"
	"encoding/json"
	"testing"

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
	result, err = server.applyConfirmedStandardOperation(context.Background(), binding.ConversationID, id, nil, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("unaccepted executed: %+v %v", result, err)
	}
	if _, err := server.suggestions.ResolveCode(binding.ConversationID, items[0].Code, "accepted", "directive", 0); err != nil {
		t.Fatal(err)
	}
	server.configMu.Lock()
	server.config.Providers.Port++
	server.configMu.Unlock()
	result, err = server.applyConfirmedStandardOperation(context.Background(), binding.ConversationID, id, nil, nil)
	if err != nil || result.Error == nil || result.Error.Code != "conflict" {
		t.Fatalf("stale accepted: %+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Port--
	server.configMu.Unlock()
	result, err = server.applyConfirmedStandardOperation(context.Background(), binding.ConversationID, id, []string{"test-http-server"}, nil)
	if err != nil || result.Error == nil || result.Error.Code != "access_denied" {
		t.Fatalf("changed floor ignored: %+v %v", result, err)
	}
	result, err = server.applyConfirmedStandardOperation(context.Background(), binding.ConversationID, id, nil, nil)
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
	result, err = server.applyConfirmedStandardOperation(context.Background(), binding.ConversationID, id, nil, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("replayed execution=%+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Port++
	server.configMu.Unlock()
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].Code, nil)
	if err != nil || result.Error == nil || result.Error.Code != "conflict" {
		t.Fatalf("stale undo=%+v %v", result, err)
	}
	server.configMu.Lock()
	server.config.Providers.Port--
	server.configMu.Unlock()
	result, err = server.undoConfirmedPluginChange(context.Background(), "other-session", items[0].Code, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("cross-session undo=%+v %v", result, err)
	}
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].Code, nil)
	if err != nil || !result.OK || result.Status != "undone" {
		t.Fatalf("undo=%+v %v", result, err)
	}
	if len(server.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatal("undo did not restore the enabled plugin")
	}
	result, err = server.undoConfirmedPluginChange(context.Background(), binding.ConversationID, items[0].Code, nil)
	if err != nil || result.Error == nil || result.Error.Code != "not_found" {
		t.Fatalf("duplicate undo=%+v %v", result, err)
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
