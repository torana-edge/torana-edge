package mcpserver

import (
	"github.com/torana-edge/torana-edge/internal/engine"
	"testing"
)

func TestToolPresenceUsesOnlyConfiguredServerNames(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"torana_search", "present"}, {"mcp__torana__torana_search", "present"},
		{"local_proxy__torana_search", "present"}, {"mcp__other__torana_search", "absent"},
		{"prefix_torana_search", "absent"}, {"torana_describe", "absent"}, {"read_file", "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolPresence(&engine.ChatRequest{Tools: []engine.ToolDef{{Name: tc.name}}}, []string{"torana", "local_proxy"}); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	if ToolPresence(nil, nil) != "not_observed" || ToolPresence(&engine.ChatRequest{}, nil) != "not_observed" {
		t.Fatal("missing catalog claimed absence")
	}
}
