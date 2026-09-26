package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestOperationDispatchReadsAndConsentHaveSeparatePaths(t *testing.T) {
	p := catalogTestPolicy(t, 1)
	executed, proposed := 0, 0
	d := operationDispatch{policy: p, execute: func(_ context.Context, call operationCall) (any, *mcpserver.DomainError, error) {
		executed++
		return map[string]any{"operation": call.Operation.ID}, nil, nil
	}, propose: func(_ context.Context, call operationCall) (mcpserver.Result, error) {
		proposed++
		return mcpserver.Result{OK: true, Status: "pending_confirmation", Summary: "Disable logger"}, nil
	}}
	for _, tc := range []struct{ operation, status string }{{"read.000", ""}, {"_disable", "pending_confirmation"}} {
		result, err := d.invoke(context.Background(), json.RawMessage(`{"namespace":"logger","operation":"`+tc.operation+`","input":{}}`), plugin.MCPBinding{})
		if err != nil || !result.OK || result.Status != tc.status {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if executed != 1 || proposed != 1 {
		t.Fatalf("writes reached execution: execute=%d propose=%d", executed, proposed)
	}
}

func TestOperationDispatchRejectsInputAndFloorsBeforeSideEffects(t *testing.T) {
	p := catalogTestPolicy(t, 1)
	count := 0
	d := operationDispatch{policy: p, execute: func(context.Context, operationCall) (any, *mcpserver.DomainError, error) {
		count++
		return nil, nil, nil
	}, propose: func(context.Context, operationCall) (mcpserver.Result, error) {
		count++
		return mcpserver.Result{}, nil
	}}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"namespace":"logs","operation":"read.000"}`, `{"namespace":"logger","operation":"_config.get"}`, `{"namespace":"logger","operation":"hidden"}`, `{"namespace":"logger","operation":"_status","input":{"extra":true}}`, `{"namespace":"logger","operation":"_status","conversation_id":"victim"}`, `{"namespace":"logger","operation":"_status","input":null}`, `{"namespace":"logger","operation":"_status","input":[]}`} {
		result, err := d.invoke(context.Background(), json.RawMessage(raw), plugin.MCPBinding{})
		if err != nil || result.OK || result.Error == nil {
			t.Fatalf("unsafe input accepted %s: %+v %v", raw, result, err)
		}
	}
	p.protected["logger"] = true
	for _, op := range []string{"_enable", "_disable", "_config.set", "_config.get"} {
		result, _ := d.invoke(context.Background(), json.RawMessage(`{"namespace":"logger","operation":"`+op+`"}`), plugin.MCPBinding{})
		if result.Error == nil || result.Error.Code != "access_denied" {
			t.Fatalf("protected operation reached consent: %s %+v", op, result)
		}
	}
	if count != 0 {
		t.Fatal("rejected call had side effects")
	}
}

func TestOperationDispatchRequiresVerifiedBindingAndRechecksDirective(t *testing.T) {
	p := catalogTestPolicy(t, 1)
	entry := p.registry.entries["logger"]
	op := plugin.AgentOperation{ID: "pin", Risk: "write", ModelAccess: "never", Directive: &plugin.AgentDirective{Command: "pin", UserDirect: true}, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
	entry.Operations = append(entry.Operations, namespaceOperation{ID: op.ID, Risk: op.Risk, ModelAccess: "never", ConversationBinding: "required", Source: "plugin", Callable: true, Guest: &op})
	p.registry.entries["logger"] = entry
	count := 0
	d := operationDispatch{policy: p, execute: func(_ context.Context, call operationCall) (any, *mcpserver.DomainError, error) {
		count++
		if call.Binding.ConversationID != "host-conversation" {
			t.Fatal("wrong bound conversation")
		}
		return nil, nil, nil
	}}
	call := namespaceDirectiveCall{Namespace: "logger", Operation: "pin", Input: json.RawMessage(`{}`), Confirm: false}
	result, _ := d.invokeDirective(context.Background(), call, plugin.MCPBinding{})
	if result.Error == nil || result.Error.Code != "unbound_conversation" || count != 0 {
		t.Fatalf("unbound write executed: %+v", result)
	}
	bound := plugin.MCPBinding{Bound: true, ConversationID: "host-conversation", CallID: "host-call"}
	result, err := d.invokeDirective(context.Background(), call, bound)
	if err != nil || !result.OK || count != 1 {
		t.Fatalf("explicit directive not executed: %+v %v", result, err)
	}
	// A caller cannot set Confirm=false to bypass a changed protection policy.
	p.protected["logger"] = true
	result, _ = d.invokeDirective(context.Background(), call, bound)
	if result.Error == nil || result.Error.Code != "access_denied" || count != 1 {
		t.Fatal("directive bypassed execution-time policy")
	}
}

func TestOperationDispatchDoesNotEchoSchemaOrFailedOutput(t *testing.T) {
	p := catalogTestPolicy(t, 1)
	entry := p.registry.entries["logger"]
	entry.Operations[7].Guest.InputSchema = json.RawMessage(`{"type":"object","properties":{"private-secret":{"type":"string"}},"required":["private-secret"]}`)
	p.registry.entries["logger"] = entry
	d := operationDispatch{policy: p, execute: func(context.Context, operationCall) (any, *mcpserver.DomainError, error) {
		return "sensitive-internal-output", nil, fmt.Errorf("internal failure")
	}}
	result, _ := d.invoke(context.Background(), json.RawMessage(`{"namespace":"logger","operation":"read.000","input":{}}`), plugin.MCPBinding{})
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private-secret") {
		t.Fatal("plugin-authored schema diagnostic leaked")
	}
	result, err := d.invoke(context.Background(), json.RawMessage(`{"namespace":"logger","operation":"_status"}`), plugin.MCPBinding{})
	if err == nil || result.Result != nil {
		t.Fatal("internal failure returned potentially sensitive output")
	}
}

func TestNamespaceExecutionCuratesModelVisibleHostReads(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = t.TempDir()
	server, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	r, err := server.currentNamespaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := operationDispatch{policy: p, execute: server.executeNamespaceOperation}
	// Reads must use the already-built policy snapshot, not rediscover files.
	server.configMu.Lock()
	server.config.Providers.Plugins.Dir = "/nonexistent/torana-model-read-must-not-scan"
	server.configMu.Unlock()
	for _, op := range []string{"system.status", "plugins.list", "stats.get"} {
		result, err := d.invoke(context.Background(), json.RawMessage(`{"namespace":"torana","operation":"`+op+`"}`), plugin.MCPBinding{})
		if err != nil || !result.OK {
			t.Fatalf("%s: %+v %v", op, result, err)
		}
		encoded, _ := json.Marshal(result)
		for _, forbidden := range []string{"credential", "approval", "config_path", "plugin_directory"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("host read leaked %s: %s", forbidden, encoded)
			}
		}
		assertNoConfirmationCode(t, op, result.Result)
	}
}

func TestNamespaceExecutionUsesDigestBoundGuestDispatcher(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.Plugins = provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-http-server"}, AllowUnapproved: true}
	server, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	r, err := server.currentNamespaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := operationDispatch{policy: p, execute: server.executeNamespaceOperation}
	input := json.RawMessage(`{"namespace":"test-http-server","operation":"status"}`)
	result, err := d.invoke(context.Background(), input, plugin.MCPBinding{Bound: true, ConversationID: "host", CallID: "call"})
	if err != nil || !result.OK {
		t.Fatalf("guest read: %+v %v", result, err)
	}
	encoded, _ := json.Marshal(result.Result)
	if !strings.Contains(string(encoded), `"status":"ready"`) {
		t.Fatalf("guest output missing: %s", encoded)
	}
	entry := r.entries["test-http-server"]
	entry.Digest = "sha256:stale"
	r.entries[entry.Name] = entry
	result, err = d.invoke(context.Background(), input, plugin.MCPBinding{})
	if err != nil || result.Error == nil || result.Error.Code != "stale_digest" || result.Result != nil {
		t.Fatalf("stale descriptor executed: %+v %v", result, err)
	}
}
