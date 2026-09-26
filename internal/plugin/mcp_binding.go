package plugin

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type mcpBindingKey struct{}

// MCPBinding is evidence supplied by the host dispatcher, never copied from
// caller headers or model arguments. Unbound calls carry no identity fields.
type MCPBinding struct {
	Bound          bool
	ConversationID string
	CallID         string
}

func validBindingIdentity(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

// WithMCPBinding attaches verified evidence for the HTTP dispatch boundary.
// A guest cannot create a Go context value; ordinary HTTP callers cannot set
// this private context key by supplying similarly named headers.
func WithMCPBinding(ctx context.Context, binding MCPBinding) (context.Context, error) {
	if binding.Bound {
		if !validBindingIdentity(binding.ConversationID) || !validBindingIdentity(binding.CallID) {
			return ctx, fmt.Errorf("bound MCP calls require valid host conversation and call identities")
		}
	} else if binding.ConversationID != "" || binding.CallID != "" {
		return ctx, fmt.Errorf("unbound MCP calls cannot carry identities")
	}
	return context.WithValue(ctx, mcpBindingKey{}, binding), nil
}

func injectMCPBindingHeaders(ctx context.Context, filtered map[string][]string) {
	binding, exists := ctx.Value(mcpBindingKey{}).(MCPBinding)
	if !exists {
		return
	}
	filtered["X-Torana-MCP-Binding"] = []string{"unbound"}
	if binding.Bound {
		filtered["X-Torana-MCP-Binding"] = []string{"bound"}
		filtered["X-Torana-Conversation-Id"] = []string{binding.ConversationID}
		filtered["X-Torana-Tool-Use-Id"] = []string{binding.CallID}
	}
}
