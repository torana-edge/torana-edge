package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestMCPConsentStateBindingAndSingleUse(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.MCP.Enabled = true
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
	ctx := context.Background()
	raw := json.RawMessage(`{"namespace":"test-http-server","operation":"_disable"}`)
	proposal, err := dispatch.invoke(ctx, raw, plugin.MCPBinding{Bound: true, ConversationID: "session", CallID: "call"})
	if err != nil || proposal.Consent == nil {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	state, err := server.sealMCPConsent(ctx, "torana_invoke", raw, proposal.Consent)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, tool    string
		raw           json.RawMessage
		state, action string
	}{
		{"tampered", "torana_invoke", raw, state + "x", "accept"},
		{"other-tool", "torana_describe", raw, state, "accept"},
		{"changed-arguments", "torana_invoke", json.RawMessage(`{"namespace":"test-http-server","operation":"_enable"}`), state, "accept"},
		{"unknown-action", "torana_invoke", raw, state, "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := server.resolveMCPConsent(ctx, tc.tool, tc.raw, tc.state, tc.action)
			if err != nil || result.Error == nil || result.Error.Code != "invalid_confirmation" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
	plaintext, _ := server.secrets.Decrypt(state)
	var expired mcpConsentState
	_ = json.Unmarshal([]byte(plaintext), &expired)
	expired.Expires = time.Now().Add(-time.Second).Unix()
	encoded, _ := json.Marshal(expired)
	expiredState, _ := server.secrets.Encrypt(string(encoded))
	result, err := server.resolveMCPConsent(ctx, "torana_invoke", raw, expiredState, "accept")
	if err != nil || result.Error == nil {
		t.Fatalf("expired=%+v %v", result, err)
	}
	result, err = server.resolveMCPConsent(ctx, "torana_invoke", raw, state, "cancel")
	if err != nil || !result.OK || result.Status != "pending_confirmation" || len(server.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatalf("cancel=%+v %v", result, err)
	}
	result, err = server.resolveMCPConsent(ctx, "torana_invoke", raw, state, "accept")
	if err != nil || !result.OK || result.Status != "applied" || len(server.GetConfig().Providers.Plugins.Order) != 0 {
		t.Fatalf("accept=%+v %v", result, err)
	}
	result, err = server.resolveMCPConsent(ctx, "torana_invoke", raw, state, "accept")
	if err != nil || result.Error == nil {
		t.Fatalf("replay=%+v %v", result, err)
	}
}

func TestOperatorOnlyConsentRetainsProposalWithoutHarnessApproval(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Consent = "operator_only"
	cfg.Plugins = provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-http-server"}, AllowUnapproved: true}
	s, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	registry, err := s.currentNamespaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newNamespaceAccessPolicy(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := operationDispatch{policy: policy, propose: s.proposeNamespaceOperation}
	raw := json.RawMessage(`{"namespace":"test-http-server","operation":"_disable"}`)
	result, err := dispatch.invoke(context.Background(), raw, plugin.MCPBinding{Bound: true, ConversationID: "session", CallID: "call"})
	if err != nil || result.Status != "pending_confirmation" || result.Consent != nil {
		t.Fatalf("proposal=%+v %v", result, err)
	}
	items, err := s.suggestions.List("session", "torana", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if _, err := s.sealMCPConsent(context.Background(), "torana_invoke", raw, &mcpserver.Consent{ID: items[0].ID, Conversation: "session"}); err == nil {
		t.Fatal("operator-only state sealed")
	}
	blocked, err := s.resolveMCPConsent(context.Background(), "torana_invoke", raw, "invalid", "accept")
	if err != nil || blocked.Error == nil || len(s.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatalf("accepted harness approval: %+v %v", blocked, err)
	}
	if _, err := s.suggestions.ResolveID("session", items[0].ID, "accepted", "agent_api", 0); err != nil {
		t.Fatal(err)
	}
	applied, err := s.applyConfirmedStandardOperation(context.Background(), "session", items[0].ID, nil)
	if err != nil || applied.Status != "applied" {
		t.Fatalf("operator refused: %+v %v", applied, err)
	}
}

func TestOfficialMCPConfirmationAppliesHostChange(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
			t.Setenv("TORANA_DATA_DIR", t.TempDir())
			cfg := provider.DefaultConfig()
			cfg.MCP.Enabled = true
			cfg.Plugins = provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-http-server"}, AllowUnapproved: true}
			s, err := New(Config{Port: "8080", Providers: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Shutdown(context.Background())
			registry, err := s.currentNamespaceRegistry()
			if err != nil {
				t.Fatal(err)
			}
			policy, err := newNamespaceAccessPolicy(registry, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			dispatch := operationDispatch{policy: policy, propose: s.proposeNamespaceOperation}
			handler, err := mcpserver.NewHandler(mcpserver.Options{Token: func() string { return "test" }, SealConsent: s.sealMCPConsent, ResolveConsent: s.resolveMCPConsent,
				Dispatch: func(ctx context.Context, _ string, raw json.RawMessage) (mcpserver.Result, error) {
					return dispatch.invoke(ctx, raw, plugin.MCPBinding{Bound: true, ConversationID: "session", CallID: "call"})
				}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			defer handler.Shutdown(context.Background())
			client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				if len(s.GetConfig().Providers.Plugins.Order) != 1 {
					t.Error("changed config before approval")
				}
				if req.Params.Message == "" {
					t.Error("no confirmation summary")
				}
				return &mcp.ElicitResult{Action: "accept"}, nil
			}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: mcpTestTransport{token: "test"}}}, &mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if session.InitializeResult().ProtocolVersion != version {
				t.Fatal("protocol downgraded")
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_invoke", Arguments: map[string]any{"namespace": "test-http-server", "operation": "_disable"}})
			if err != nil || result.IsError || len(s.GetConfig().Providers.Plugins.Order) != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			changes, err := s.suggestions.ListChanges("session")
			if err != nil || len(changes) != 1 || changes[0].Status != "applied" {
				t.Fatalf("history=%+v err=%v", changes, err)
			}
		})
	}
}
