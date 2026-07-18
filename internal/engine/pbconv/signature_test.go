package pbconv

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestToolCallStartSignatureRoundTrip guards the fix for the Antigravity/Code
// Assist 400: the WASM pipeline round-trips every stream event through protobuf,
// so a Gemini thoughtSignature on ToolCallStart must survive it or replayed
// history loses the signature and the server rejects the next turn.
func TestToolCallStartSignatureRoundTrip(t *testing.T) {
	ev := &engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{
		Index: 0, ID: "call_1", Name: "list_dir", Signature: "THOUGHT_SIG_XYZ",
	}}
	got := FromPBStreamEvent(ToPBStreamEvent(ev))
	if got.ToolCallStart == nil {
		t.Fatal("tool call start lost")
	}
	if got.ToolCallStart.Signature != "THOUGHT_SIG_XYZ" {
		t.Errorf("signature lost through pb round-trip: %q", got.ToolCallStart.Signature)
	}
}

func TestSignatureDeltaRoundTrip(t *testing.T) {
	sig := "STANDALONE_SIG"
	ev := &engine.StreamEvent{SignatureDelta: &sig}
	got := FromPBStreamEvent(ToPBStreamEvent(ev))
	if got.SignatureDelta == nil || *got.SignatureDelta != "STANDALONE_SIG" {
		t.Errorf("signature delta lost through pb round-trip: %v", got.SignatureDelta)
	}
}

func TestToolCallSignatureRoundTrip(t *testing.T) {
	chat := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
			{ID: "a1", Name: "f", Arguments: map[string]any{"x": 1.0}, Signature: "REQ_SIG"},
		}},
	}}
	got := FromPBChatRequest(ToPBChatRequest(chat))
	if len(got.Messages) != 1 || len(got.Messages[0].ToolCalls) != 1 {
		t.Fatal("tool call lost")
	}
	if got.Messages[0].ToolCalls[0].Signature != "REQ_SIG" {
		t.Errorf("request-side tool call signature lost: %q", got.Messages[0].ToolCalls[0].Signature)
	}
}
