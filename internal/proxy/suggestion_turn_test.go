package proxy

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestUserTurnSignatureIgnoresToolResultContinuations(t *testing.T) {
	chat := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "first"}}}},
	}}
	first := userTurnSignature(chat)
	if first == "" {
		t.Fatal("first user message has no signature")
	}
	chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{
		{Text: &engine.TextBlock{Text: "tool context"}},
		{ToolResult: &engine.ToolResultBlock{}},
	}})
	if got := userTurnSignature(chat); got != first {
		t.Fatalf("tool continuation changed user turn: %q != %q", got, first)
	}
	chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "next"}}}})
	if got := userTurnSignature(chat); got == first {
		t.Fatal("new user message reused prior signature")
	}
}
