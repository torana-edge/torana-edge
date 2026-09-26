package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/conversation"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestScopedCoreReadsCannotSelectOrLeakAnotherConversation(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = t.TempDir()
	s, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	registry, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newNamespaceAccessPolicy(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := operationDispatch{policy: policy, execute: s.executeNamespaceOperation}
	s.feed.Add(metrics.RequestEvent{ConversationID: "a", RequestedModel: "own-model", TokensIn: 5})
	s.feed.Add(metrics.RequestEvent{ConversationID: "b", RequestedModel: "other-private-model", TokensIn: 900})
	s.feed.Add(metrics.RequestEvent{RequestedModel: "unscoped-private-model", TokensIn: 800})
	s.conversations.Observe(conversation.Observation{ID: "a", TokensIn: 5, TokensOut: 2})
	s.conversations.Observe(conversation.Observation{ID: "b", TokensIn: 900, TokensOut: 700})
	for _, id := range []string{"a", "b"} {
		_, err := s.suggestions.Create(id, "logger", 1, &pb.SuggestArgs{Kind: "model_switch", Title: id + " title", Body: "private-body-" + id, DedupeKey: "private-dedupe-" + id})
		if err != nil {
			t.Fatal(err)
		}
	}
	items, _ := s.suggestions.List("a", "", 0)
	for _, operation := range []string{"feed.recent", "session.usage", "suggestions.list", "changes.list"} {
		t.Run(operation, func(t *testing.T) {
			raw, _ := json.Marshal(namespaceInvokeInput{Namespace: "torana", Operation: operation})
			result, err := dispatch.invoke(context.Background(), raw, plugin.MCPBinding{Bound: true, ConversationID: "a", CallID: "call"})
			if err != nil || !result.OK {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			encoded, _ := json.Marshal(result)
			for _, secret := range []string{"other-private-model", "unscoped-private-model", "b title", "private-body", "private-dedupe", items[0].Code} {
				if strings.Contains(string(encoded), secret) {
					t.Errorf("leaked %q in %s", secret, encoded)
				}
			}
			var value any
			_ = json.Unmarshal(encoded, &value)
			assertNoConfirmationCode(t, operation, value)
			if operation == "session.usage" && result.Result.(map[string]any)["tokens_in"] != int64(5) {
				t.Fatal("usage is not scoped")
			}
			result, err = dispatch.invoke(context.Background(), raw, plugin.MCPBinding{})
			if err != nil || result.Error == nil || result.Error.Code != "unbound_conversation" {
				t.Fatalf("unbound read=%+v err=%v", result, err)
			}
			selected, _ := json.Marshal(namespaceInvokeInput{Namespace: "torana", Operation: operation, Input: json.RawMessage(`{"conversation_id":"b"}`)})
			result, err = dispatch.invoke(context.Background(), selected, plugin.MCPBinding{Bound: true, ConversationID: "a", CallID: "call"})
			if err != nil || result.Error == nil || result.Error.Code != "invalid_input" {
				t.Fatalf("caller selected scope=%+v err=%v", result, err)
			}
		})
	}
}
