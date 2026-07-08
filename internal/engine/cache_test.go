package engine

import (
	"net/http"
	"strings"
	"testing"
)

// TestDeterministicMessagePrefix validates that the offload pipeline
// doesn't reorder messages or tools — critical for provider prompt caching.
func TestDeterministicMessagePrefix(t *testing.T) {
	// Simulate a 3-turn conversation: user → assistant tool → tool result → ...
	baseMessages := []Message{
		{Role: RoleUser, Content: "Read go.mod and find the Go version"},
		{Role: RoleAssistant, Content: "", ToolCalls: []ToolCall{
			{ID: "call_1", Name: "read", Arguments: map[string]any{"path": "go.mod"}},
		}},
		{Role: RoleTool, ToolCallID: "call_1", Content: strings.Repeat("go 1.21\n", 100)}, // large
		{Role: RoleUser, Content: "Now check if this is recent enough for k8s deployment"},
	}

	pipeline := New()

	// Run 3 turns — message prefix up to the first tool result index
	// should remain byte-identical across turns.
	var prefixes [3][]byte

	for turn := 0; turn < 3; turn++ {
		chat := &ChatRequest{
			Messages: make([]Message, len(baseMessages)),
		}
		copy(chat.Messages, baseMessages)

		result := pipeline.RunBeforeRequest(nil, &http.Request{}, chat)
		if result == nil {
			t.Fatal("pipeline returned nil")
		}

		// Capture prefix: first 3 messages (user, assistant, tool result)
		// as a string to compare across turns.
		var prefix strings.Builder
		for i := 0; i < 3 && i < len(result.Messages); i++ {
			prefix.WriteString(string(result.Messages[i].Role))
			prefix.WriteString(":")
			prefix.WriteString(result.Messages[i].Content)
			prefix.WriteString("\n")
		}
		prefixes[turn] = []byte(prefix.String())
	}

	// All 3 turns must produce identical prefixes.
	for turn := 1; turn < 3; turn++ {
		if string(prefixes[turn]) != string(prefixes[0]) {
			t.Errorf("turn %d prefix differs from turn 0:\ngot:  %q\nwant: %q",
				turn, prefixes[turn], prefixes[0])
		}
	}
}
