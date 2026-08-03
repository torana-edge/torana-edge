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
	got, err := (&BlockKindTracker{}).FromPBStreamEvent(ToPBStreamEvent(ev))
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
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
	got, err := (&BlockKindTracker{}).FromPBStreamEvent(ToPBStreamEvent(ev))
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	if got.SignatureDelta == nil || *got.SignatureDelta != "STANDALONE_SIG" {
		t.Errorf("signature delta lost through pb round-trip: %v", got.SignatureDelta)
	}
}

func textSigOf(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.Text != nil {
			return b.Text.Signature
		}
	}
	return ""
}

func trailOf(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.TrailingSignature != nil {
			return b.TrailingSignature.Signature
		}
	}
	return ""
}

func mustReq(t *testing.T, raw string) engine.RequiredJSONObject {
	t.Helper()
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestToolCallSignatureRoundTrip(t *testing.T) {
	chat := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
			ID: "a1", Name: "f", Arguments: mustReq(t, `{"x": 1.0}`), Signature: "REQ_SIG",
		}}}},
	}}
	got, err := FromPBChatRequest(ToPBChatRequest(chat))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Blocks) != 1 {
		t.Fatal("tool call lost")
	}
	if got.Messages[0].Blocks[0].ToolUse.Signature != "REQ_SIG" {
		t.Errorf("request-side tool call signature lost: %q", got.Messages[0].Blocks[0].ToolUse.Signature)
	}
}

// TestContentSignatureRoundTrip guards Message.content_signature (pb field 12,
// request-domain SignatureScopeSameMessage) through the protobuf boundary: the
// WASM pipeline round-trips every request message through it, so a Gemini/Code
// Assist thoughtSignature beside non-thought text must survive or replayed
// history loses the content binding on the next turn.
func TestContentSignatureRoundTrip(t *testing.T) {
	chat := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "answer", Signature: "CONTENT_SIG"}}}},
	}}
	got, err := FromPBChatRequest(ToPBChatRequest(chat))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatal("message lost")
	}
	if textSigOf(got.Messages[0]) != "CONTENT_SIG" {
		t.Errorf("request-side content signature lost: %q", textSigOf(got.Messages[0]))
	}
}

// TestTrailingSignatureRoundTrip guards Message.trailing_signature (pb field 11,
// request-domain SignatureScopeTrailingStandalone) through the protobuf
// boundary: the WASM pipeline round-trips every request message through it, so
// a Gemini/Code Assist trailing signature-only part must survive or replayed
// history loses the binding on the next turn.
func TestTrailingSignatureRoundTrip(t *testing.T) {
	chat := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "answer"}},
			{TrailingSignature: &engine.TrailingSignatureBlock{Signature: "TRAIL_SIG"}},
		}},
	}}
	got, err := FromPBChatRequest(ToPBChatRequest(chat))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatal("message lost")
	}
	if trailOf(got.Messages[0]) != "TRAIL_SIG" {
		t.Errorf("request-side trailing signature lost: %q", trailOf(got.Messages[0]))
	}
}
