package plugin

import (
	"context"
	"reflect"
	"testing"
)

func TestMCPBindingHeadersCannotBeForgedByCaller(t *testing.T) {
	raw := map[string][]string{"x-torana-mcp-binding": {"bound"}, "X-Torana-Conversation-Id": {"victim"}, "X-Torana-Tool-Use-Id": {"forged"}, "Content-Type": {"application/json"}}
	for _, grant := range []bool{false, true} {
		for _, binding := range []MCPBinding{{}, {Bound: true, ConversationID: "verified", CallID: "actual"}} {
			ctx, err := WithMCPBinding(context.Background(), binding)
			if err != nil {
				t.Fatal(err)
			}
			filtered := filterHTTPHeaders(raw, grant)
			injectMCPBindingHeaders(ctx, filtered)
			want := map[string][]string{"Content-Type": {"application/json"}, "X-Torana-MCP-Binding": {"unbound"}}
			if binding.Bound {
				want["X-Torana-MCP-Binding"] = []string{"bound"}
				want["X-Torana-Conversation-Id"] = []string{"verified"}
				want["X-Torana-Tool-Use-Id"] = []string{"actual"}
			}
			if !reflect.DeepEqual(filtered, want) {
				t.Fatalf("grant=%t headers=%v", grant, filtered)
			}
		}
		filtered := filterHTTPHeaders(raw, grant)
		injectMCPBindingHeaders(context.Background(), filtered)
		if len(filtered) != 1 {
			t.Fatal("ordinary dispatch accepted forged MCP evidence")
		}
	}
}

func TestMCPBindingRejectsPartialOrUnsafeIdentity(t *testing.T) {
	for _, binding := range []MCPBinding{{Bound: true}, {Bound: true, ConversationID: "one"}, {ConversationID: "one", CallID: "two"}, {Bound: true, ConversationID: "one\nforged", CallID: "two"}, {Bound: true, ConversationID: "one", CallID: string([]byte{255})}} {
		if _, err := WithMCPBinding(context.Background(), binding); err == nil {
			t.Fatalf("unsafe binding accepted: %+v", binding)
		}
	}
}
