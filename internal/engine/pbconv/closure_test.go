package pbconv

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// messageCorpus is the ordered-body reference model: every block kind, with
// signatures, nested tool-result content, raw object wrappers, and cache
// breakpoints at explicit positions — the shapes the adapters and the SDK
// fingerprint must agree on.
func messageCorpus() []engine.Message {
	return []engine.Message{
		{Role: engine.RoleSystem, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "system prompt"}},
			{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustReqObj(`{"type":"ephemeral"}`)}},
		}},
		{Role: engine.RoleUser, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "hello"}},
			{Text: &engine.TextBlock{Text: "", Signature: "trailing-SIG"}},
			{TrailingSignature: &engine.TrailingSignatureBlock{Signature: "T"}},
		}},
		{Role: engine.RoleAssistant, Blocks: []engine.Block{
			{Thinking: &engine.ThinkingBlock{Text: "reasoning", Signature: "TSIG"}},
			{Text: &engine.TextBlock{Text: "answer", Signature: "CSIG"}},
			{ToolUse: &engine.ToolUseBlock{ID: "call_1", Name: "read", Arguments: mustReqObj(`{"path":"a.go"}`), Signature: "CALLSIG"}},
			{RedactedThinking: &engine.RedactedThinkingBlock{Data: "redacted-bytes"}},
		}},
		{Role: engine.RoleUser, Blocks: []engine.Block{
			{ToolResult: &engine.ToolResultBlock{
				ToolCallID: "call_1",
				ToolName:   "read",
				Content: []engine.ToolResultContentBlock{
					{Text: "package a"},
					{Unknown: &engine.UnknownBlock{Kind: "image", Payload: mustReqObj(`{"source":{"data":"abc"}}`)}},
					{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustReqObj(`{"type":"ephemeral","ttl":"1h"}`)}},
					{Text: "done"},
				},
			}},
			{Unknown: &engine.UnknownBlock{Kind: "custom_part", Payload: mustReqObj(`{"x":[1,2,3]}`)}},
		}},
	}
}

func mustReqObj(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

// TestMessageToPBDifferential pins byte equality between the engine's
// canonical projection (used by the shared SDK fingerprint in CachePrefixKey)
// and the pbconv request-path converter. The two implementations must never
// drift: a difference would make the cache key disagree with the actual
// request body the plugins see.
func TestMessageToPBDifferential(t *testing.T) {
	for i, m := range messageCorpus() {
		got := engine.MessageToPB(&m)
		want := toPBMessage(m)
		if !proto.Equal(got, want) {
			t.Fatalf("message %d: MessageToPB != toPBMessage\n  engine: %v\n  pbconv: %v", i, got, want)
		}
		gotRaw, _ := proto.Marshal(got)
		wantRaw, _ := proto.Marshal(want)
		if string(gotRaw) != string(wantRaw) {
			t.Fatalf("message %d: wire bytes differ: %x vs %x", i, gotRaw, wantRaw)
		}
	}
}

// TestPBMarshalClosure pins the request-path round trip: every corpus message
// survives ToPB -> FromPB byte-identically, and the canonical PB survives
// FromPB -> ToPB identically. This is the reference model the adapters and
// the fingerprint share.
func TestPBMarshalClosure(t *testing.T) {
	// Engine -> PB -> engine.
	for i, m := range messageCorpus() {
		pbMsg := toPBMessage(m)
		back, err := fromPBMessage(pbMsg)
		if err != nil {
			t.Fatalf("message %d: FromPB: %v", i, err)
		}
		if !messagesEqual(back, m) {
			t.Fatalf("message %d: engine round trip changed the message:\n  got  %+v\n  want %+v", i, back, m)
		}
	}
	// PB -> engine -> PB (over the corpus PB shapes).
	for i, m := range messageCorpus() {
		pbMsg := toPBMessage(m)
		back, err := fromPBMessage(pbMsg)
		if err != nil {
			t.Fatalf("message %d: FromPB: %v", i, err)
		}
		again := toPBMessage(back)
		if !proto.Equal(again, pbMsg) {
			t.Fatalf("message %d: PB round trip changed the bytes:\n  got  %v\n  want %v", i, again, pbMsg)
		}
	}
}

// messagesEqual compares engine messages structurally (blocks in order, all
// kinds, raw bytes verbatim).
func messagesEqual(a, b engine.Message) bool {
	if a.Role != b.Role || len(a.Blocks) != len(b.Blocks) {
		return false
	}
	for i := range a.Blocks {
		if !blockEqual(a.Blocks[i], b.Blocks[i]) {
			return false
		}
	}
	return true
}

func blockEqual(a, b engine.Block) bool {
	switch {
	case a.Text != nil || b.Text != nil:
		if a.Text == nil || b.Text == nil {
			return false
		}
		return a.Text.Text == b.Text.Text && a.Text.Signature == b.Text.Signature
	case a.Thinking != nil || b.Thinking != nil:
		if a.Thinking == nil || b.Thinking == nil {
			return false
		}
		return a.Thinking.Text == b.Thinking.Text && a.Thinking.Signature == b.Thinking.Signature
	case a.RedactedThinking != nil || b.RedactedThinking != nil:
		if a.RedactedThinking == nil || b.RedactedThinking == nil {
			return false
		}
		return a.RedactedThinking.Data == b.RedactedThinking.Data
	case a.ToolUse != nil || b.ToolUse != nil:
		if a.ToolUse == nil || b.ToolUse == nil {
			return false
		}
		return a.ToolUse.ID == b.ToolUse.ID && a.ToolUse.Name == b.ToolUse.Name &&
			string(a.ToolUse.Arguments.Bytes()) == string(b.ToolUse.Arguments.Bytes()) &&
			a.ToolUse.Signature == b.ToolUse.Signature
	case a.ToolResult != nil || b.ToolResult != nil:
		if a.ToolResult == nil || b.ToolResult == nil {
			return false
		}
		if a.ToolResult.ToolCallID != b.ToolResult.ToolCallID || a.ToolResult.ToolName != b.ToolResult.ToolName ||
			len(a.ToolResult.Content) != len(b.ToolResult.Content) {
			return false
		}
		for i := range a.ToolResult.Content {
			ca, cb := a.ToolResult.Content[i], b.ToolResult.Content[i]
			if ca.Unknown != nil || cb.Unknown != nil {
				if ca.Unknown == nil || cb.Unknown == nil || ca.Unknown.Kind != cb.Unknown.Kind ||
					string(ca.Unknown.Payload.Bytes()) != string(cb.Unknown.Payload.Bytes()) {
					return false
				}
			} else if ca.CacheBreakpoint != nil || cb.CacheBreakpoint != nil {
				if ca.CacheBreakpoint == nil || cb.CacheBreakpoint == nil ||
					string(ca.CacheBreakpoint.Marker.Bytes()) != string(cb.CacheBreakpoint.Marker.Bytes()) {
					return false
				}
			} else if ca.Text != cb.Text {
				return false
			}
		}
		return true
	case a.CacheBreakpoint != nil || b.CacheBreakpoint != nil:
		if a.CacheBreakpoint == nil || b.CacheBreakpoint == nil {
			return false
		}
		return string(a.CacheBreakpoint.Marker.Bytes()) == string(b.CacheBreakpoint.Marker.Bytes())
	case a.Unknown != nil || b.Unknown != nil:
		if a.Unknown == nil || b.Unknown == nil {
			return false
		}
		return a.Unknown.Kind == b.Unknown.Kind &&
			string(a.Unknown.Payload.Bytes()) == string(b.Unknown.Payload.Bytes())
	case a.TrailingSignature != nil || b.TrailingSignature != nil:
		if a.TrailingSignature == nil || b.TrailingSignature == nil {
			return false
		}
		return a.TrailingSignature.Signature == b.TrailingSignature.Signature
	}
	return true
}

// TestFromPBTotality pins the fail-closed totality of fromPBMessage: nil
// blocks, typed-nil oneof arms, and nil messages are refusals, never panics,
// never silent empties.
func TestFromPBTotality(t *testing.T) {
	cases := map[string]*pb.Message{
		"nil message":       nil,
		"nil block element": {Role: "user", Blocks: []*pb.RequestBlock{nil}},
		"typed-nil text arm": {Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{}},
		}},
		"typed-nil tool use": {Role: "assistant", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_ToolUse{}},
		}},
		"malformed arguments": {Role: "assistant", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{
				Id: "c1", Name: "f", ArgumentsJson: []byte(`[1,2]`),
			}}},
		}},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := fromPBMessage(m); err == nil {
				t.Fatal("malformed PB was accepted")
			}
		})
	}
}
