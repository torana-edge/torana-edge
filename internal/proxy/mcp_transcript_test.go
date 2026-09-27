package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestTranscriptOperationCreatesOneConfirmationWithoutApplying(t *testing.T) {
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
	args, err := engine.ParseRequiredJSONObject([]byte(`{"namespace":"test-http-server","operation":"_disable"}`))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := server.mcpPolicy()
	if err != nil {
		t.Fatal(err)
	}
	dispatch := operationDispatch{policy: policy, execute: server.executeNamespaceOperation, propose: server.proposeNamespaceOperation, sealPending: server.sealTranscriptTicket}
	pendingResult, err := dispatch.invoke(context.Background(), args.Bytes(), plugin.MCPBinding{})
	if err != nil || !pendingResult.OK || pendingResult.Status != "pending" || pendingResult.Ticket == "" {
		t.Fatalf("pending=%+v err=%v", pendingResult, err)
	}
	pending, err := json.Marshal(pendingResult)
	if err != nil {
		t.Fatal(err)
	}
	request := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call-1", Name: "mcp__torana__torana_invoke", Arguments: args}}}},
		{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call-1", Content: []engine.ToolResultContentBlock{{Text: string(pending)}}}}}},
	}}
	server.observeTranscriptOperations(context.Background(), request, "conversation", []string{"torana"})
	items, err := server.suggestions.List("conversation", "torana", 0)
	if err != nil || len(items) != 1 || items[0].Status != "pending" {
		t.Fatalf("suggestions=%+v err=%v", items, err)
	}
	if len(server.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatal("transcript operation applied before user confirmation")
	}
	server.observeTranscriptOperations(context.Background(), request, "conversation", []string{"torana"})
	again, err := server.suggestions.List("conversation", "torana", 0)
	if err != nil || len(again) != 1 || again[0].ID != items[0].ID {
		t.Fatalf("replay created another confirmation: %+v err=%v", again, err)
	}
	if _, err := server.suggestions.ResolveID("conversation", items[0].ID, "dismissed", "test", 0); err != nil {
		t.Fatal(err)
	}
	server.observeTranscriptOperations(context.Background(), request, "conversation", []string{"torana"})
	afterDismiss, err := server.suggestions.List("conversation", "torana", 0)
	if err != nil || len(afterDismiss) != 1 || afterDismiss[0].Status != "dismissed" {
		t.Fatalf("dismissed transcript returned: %+v err=%v", afterDismiss, err)
	}

	secondResult, err := dispatch.invoke(context.Background(), args.Bytes(), plugin.MCPBinding{})
	if err != nil || secondResult.Ticket == "" {
		t.Fatalf("second pending=%+v err=%v", secondResult, err)
	}
	secondPending, _ := json.Marshal(secondResult)
	secondRequest := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call-2", Name: "mcp__torana__torana_invoke", Arguments: args}}}},
		{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call-2", Content: []engine.ToolResultContentBlock{{Text: string(secondPending)}}}}}},
	}}
	server.observeTranscriptOperations(context.Background(), secondRequest, "conversation", []string{"torana"})
	beforeApply, err := server.suggestions.List("conversation", "torana", 0)
	if err != nil || len(beforeApply) != 2 || beforeApply[1].Status != "pending" {
		t.Fatalf("second confirmation=%+v err=%v", beforeApply, err)
	}
	if _, err := server.suggestions.ResolveID("conversation", beforeApply[1].ID, "accepted", "test", 0); err != nil {
		t.Fatal(err)
	}
	if result, err := server.applyConfirmedOperation(context.Background(), "conversation", beforeApply[1].ID, nil); err != nil || !result.OK || result.Status != "applied" {
		t.Fatalf("apply result=%+v err=%v", result, err)
	}
	server.observeTranscriptOperations(context.Background(), secondRequest, "conversation", []string{"torana"})
	afterApply, err := server.suggestions.List("conversation", "torana", 0)
	if err != nil || len(afterApply) != 2 || afterApply[1].Status != "accepted" || afterApply[1].Outcome != "applied" {
		t.Fatalf("applied transcript returned: %+v err=%v", afterApply, err)
	}
}

func TestTranscriptTicketRejectsMismatchedAndReplayedInvocation(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	server, err := New(Config{Port: "8080", Providers: provider.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	input := namespaceInvokeInput{Namespace: "one", Operation: "write", Input: json.RawMessage(`{"value":1}`)}
	ticket, err := server.sealTranscriptTicket(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	matching, _ := json.Marshal(input)
	mismatchInput := input
	mismatchInput.Input = json.RawMessage(`{"value":2}`)
	mismatch, _ := json.Marshal(mismatchInput)
	if err := server.consumeTranscriptTicket(context.Background(), ticket, mismatch); err == nil {
		t.Fatal("mismatched invocation consumed ticket")
	}
	if err := server.consumeTranscriptTicket(context.Background(), ticket, matching); err != nil {
		t.Fatal(err)
	}
	if err := server.consumeTranscriptTicket(context.Background(), ticket, matching); err == nil {
		t.Fatal("replayed ticket accepted")
	}
	expiring, err := server.sealTranscriptTicket(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := server.secrets.Decrypt(expiring)
	if err != nil {
		t.Fatal(err)
	}
	var state transcriptTicketState
	if json.Unmarshal([]byte(plaintext), &state) != nil {
		t.Fatal("ticket state did not decode")
	}
	state.Expires = time.Now().Add(-time.Second).Unix()
	expiredPlaintext, _ := json.Marshal(state)
	expired, err := server.secrets.Encrypt(string(expiredPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.consumeTranscriptTicket(context.Background(), expired, matching); err == nil {
		t.Fatal("expired ticket accepted")
	}
	reorderedInput := namespaceInvokeInput{Namespace: "one", Operation: "write", Input: json.RawMessage(`{"a":9007199254740993,"b":2}`)}
	reorderedTicket, err := server.sealTranscriptTicket(context.Background(), reorderedInput)
	if err != nil {
		t.Fatal(err)
	}
	reordered := json.RawMessage(`{"input":{"b":2,"a":9007199254740993},"operation":"write","namespace":"one"}`)
	if err := server.consumeTranscriptTicket(context.Background(), reorderedTicket, reordered); err != nil {
		t.Fatalf("equivalent reordered arguments rejected: %v", err)
	}
}
