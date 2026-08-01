package pbconv

import (
	"bytes"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// v2 replaced three v1 events with the canonical content-block sequence. The
// mapping is pinned here because it is not a rename: a tool call now opens a
// content block, and the block index is what binds its deltas and its signature
// to it. Getting it wrong produces a stream that decodes cleanly and means
// something else.
func TestToolCallMapsOntoTheContentBlockSequence(t *testing.T) {
	start := ToPBStreamEvent(&engine.StreamEvent{
		ToolCallStart: &engine.ToolCallStart{
			Index: 2, ID: "call_1", Name: "read_file", Signature: "sig",
		},
	})
	cbs, ok := start.Event.(*pb.StreamEvent_ContentBlockStart)
	if !ok {
		t.Fatalf("ToolCallStart did not become a ContentBlockStart: %T", start.Event)
	}
	if cbs.ContentBlockStart.Index != 2 {
		t.Fatalf("block index = %d, want 2 — deltas and signatures bind by index",
			cbs.ContentBlockStart.Index)
	}
	tc, ok := cbs.ContentBlockStart.Block.(*pb.ContentBlockStart_ToolCall)
	if !ok {
		t.Fatalf("block is not a tool call: %T", cbs.ContentBlockStart.Block)
	}
	if tc.ToolCall.Id != "call_1" || tc.ToolCall.Name != "read_file" || tc.ToolCall.Signature != "sig" {
		t.Fatalf("tool call ref lost fields: %+v", tc.ToolCall)
	}

	stop := ToPBStreamEvent(&engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 2}})
	cbe, ok := stop.Event.(*pb.StreamEvent_ContentBlockStop)
	if !ok {
		t.Fatalf("ToolCallEnd did not become a ContentBlockStop: %T", stop.Event)
	}
	if cbe.ContentBlockStop.Index != 2 {
		t.Fatalf("stop index = %d, want 2 — a stop naming another block is invalid",
			cbe.ContentBlockStop.Index)
	}

	finish := ToPBStreamEvent(&engine.StreamEvent{FinishReason: "stop"})
	ms, ok := finish.Event.(*pb.StreamEvent_MessageStop)
	if !ok {
		t.Fatalf("FinishReason did not become a MessageStop: %T", finish.Event)
	}
	if ms.MessageStop.FinishReason != "stop" {
		t.Fatalf("finish reason lost: %q", ms.MessageStop.FinishReason)
	}
}

// Parallel tool calls must arrive at the wire with their distinct sequential
// block indexes intact: the index is what binds each delta and signature to its
// block (SignatureScopeToolCallBlockByIndex), so collapsing them would make
// deltas of one call bind to another.
func TestToPBStreamEventPreservesDistinctBlockIndexes(t *testing.T) {
	for _, idx := range []int{0, 1} {
		start := ToPBStreamEvent(&engine.StreamEvent{
			ToolCallStart: &engine.ToolCallStart{Index: idx, ID: "c", Name: "n"},
		})
		cbs, ok := start.Event.(*pb.StreamEvent_ContentBlockStart)
		if !ok {
			t.Fatalf("ToolCallStart(%d) did not become a ContentBlockStart: %T", idx, start.Event)
		}
		if cbs.ContentBlockStart.Index != int32(idx) {
			t.Errorf("block index = %d, want %d", cbs.ContentBlockStart.Index, idx)
		}
	}
}

func TestToolCallSequenceRoundTrips(t *testing.T) {
	for _, in := range []*engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "c", Name: "n", Signature: "s"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"a":1}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		{FinishReason: "tool_use"},
	} {
		got := FromPBStreamEvent(ToPBStreamEvent(in))
		switch {
		case in.ToolCallStart != nil:
			if got.ToolCallStart == nil || *got.ToolCallStart != *in.ToolCallStart {
				t.Errorf("ToolCallStart round trip: got %+v, want %+v", got.ToolCallStart, in.ToolCallStart)
			}
		case in.ToolCallDelta != nil:
			if got.ToolCallDelta == nil || *got.ToolCallDelta != *in.ToolCallDelta {
				t.Errorf("ToolCallDelta round trip: got %+v, want %+v", got.ToolCallDelta, in.ToolCallDelta)
			}
		case in.ToolCallEnd != nil:
			if got.ToolCallEnd == nil || *got.ToolCallEnd != *in.ToolCallEnd {
				t.Errorf("ToolCallEnd round trip: got %+v, want %+v", got.ToolCallEnd, in.ToolCallEnd)
			}
		default:
			if got.FinishReason != in.FinishReason {
				t.Errorf("FinishReason round trip: got %q, want %q", got.FinishReason, in.FinishReason)
			}
		}
	}
}

// The inbound direction is lossy on purpose, and it must stay deliberate.
//
// The engine IR has no open-event for text or thinking — those arrive as bare
// deltas — so a text/thinking/provider ContentBlockStart has no counterpart.
// Dropping it is right; INVENTING one would fabricate content the provider
// never sent. This pins the drop so it is a decision rather than an oversight,
// and flags that the IR needs extending if the host ever has to round-trip a
// plugin's own text block.
func TestNonToolBlockStartsAreDropped(t *testing.T) {
	got := FromPBStreamEvent(&pb.StreamEvent{
		Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
			},
		},
	})
	if got.ToolCallStart != nil {
		t.Fatal("a text block start became a tool call")
	}
	if got.TextDelta != nil || got.ThinkingDelta != nil {
		t.Fatal("a text block start invented delta content")
	}
}

// A tool-call block start carrying no ref must not panic or produce a
// half-built tool call. The host decodes bytes a guest controls, so the guest
// picks when this path runs.
func TestToolBlockStartWithoutRefIsSafe(t *testing.T) {
	got := FromPBStreamEvent(&pb.StreamEvent{
		Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_ToolCall{ToolCall: nil},
			},
		},
	})
	if got.ToolCallStart != nil {
		t.Fatalf("built a tool call from a nil ref: %+v", got.ToolCallStart)
	}
}

// The response conversion must not lose the assistant's reply. v1 had no
// response type at all, so this mapping is new surface rather than a port, and
// a field silently dropped here reproduces the v1 failure it exists to fix.
func TestChatResponseRoundTrips(t *testing.T) {
	content := "here you go"
	in := &engine.ChatResponse{
		Model:        "claude-opus-5",
		ID:           "msg_1",
		FinishReason: "tool_use",
		Message: &engine.ResponseMessage{
			Content: &content,
			ToolCalls: []engine.ResponseToolCall{{
				ID: "call_1", Name: "read_file",
				ArgumentsJSON: []byte(`{"path":"/a"}`),
				Signature:     "sig",
			}},
		},
		Usage:              &engine.StreamUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 4},
		UpstreamStatus:     200,
		DurationMS:         1234,
		ProviderExtensions: map[string]any{"x": "y"},
	}
	got := FromPBChatResponse(ToPBChatResponse(in))
	if got == nil {
		t.Fatal("round trip produced nil")
	}
	if got.Model != in.Model || got.ID != in.ID || got.FinishReason != in.FinishReason {
		t.Errorf("scalars lost: %+v", got)
	}
	if got.UpstreamStatus != 200 || got.DurationMS != 1234 {
		t.Errorf("upstream status/duration lost: %d %d", got.UpstreamStatus, got.DurationMS)
	}
	if got.Message == nil {
		t.Fatal("the assistant reply was dropped — this is the v1 bug")
	}
	if got.Message.Content == nil || *got.Message.Content != "here you go" {
		t.Errorf("message content lost: %+v", got.Message.Content)
	}
	if len(got.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls lost: %+v", got.Message.ToolCalls)
	}
	tc := got.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" || tc.Signature != "sig" {
		t.Errorf("tool call fields lost: %+v", tc)
	}
	if string(tc.ArgumentsJSON) != `{"path":"/a"}` {
		t.Errorf("tool call arguments lost: %q", tc.ArgumentsJSON)
	}
	if got.Usage == nil || got.Usage.InputTokens != 10 || got.Usage.CacheWriteTokens != 4 {
		t.Errorf("usage lost: %+v", got.Usage)
	}
	if got.ProviderExtensions["x"] != "y" {
		t.Errorf("provider extensions lost: %+v", got.ProviderExtensions)
	}
}

// The canonical conversion must never re-encode arguments: key order is part
// of the cacheable prompt prefix, and 9007199254740993 exceeds JavaScript's
// exact-integer range, so any float64 round-trip corrupts it. The bytes a
// plugin accepted must be the bytes the host compares and writes back.
func TestPBChatResponsePreservesRawArgumentsBytes(t *testing.T) {
	raw := []byte(`{"zzz":1,"aaa":9007199254740993}`)
	in := &engine.ChatResponse{
		Message: &engine.ResponseMessage{
			ToolCalls: []engine.ResponseToolCall{{
				ID: "call_9", Name: "t", ArgumentsJSON: raw, Signature: "sig-9",
			}},
		},
	}
	got := FromPBChatResponse(ToPBChatResponse(in))
	if got == nil || got.Message == nil || len(got.Message.ToolCalls) != 1 {
		t.Fatalf("round trip lost the tool call: %+v", got)
	}
	tc := got.Message.ToolCalls[0]
	if !bytes.Equal(tc.ArgumentsJSON, raw) {
		t.Errorf("arguments re-encoded: got %q want %q", tc.ArgumentsJSON, raw)
	}
	if tc.ID != "call_9" || tc.Signature != "sig-9" {
		t.Errorf("id/signature lost: %+v", tc)
	}
}

// Absence and present-empty are different facts: absent means the provider
// body has no writable text slot, present-empty means an empty text part. A
// plugin cannot change presence, so the conversion must preserve it exactly.
func TestPBChatResponseContentPresence(t *testing.T) {
	got := FromPBChatResponse(ToPBChatResponse(&engine.ChatResponse{
		Message: &engine.ResponseMessage{},
	}))
	if got.Message == nil || got.Message.Content != nil {
		t.Fatalf("absent content became present: %+v", got.Message)
	}

	empty := ""
	got = FromPBChatResponse(ToPBChatResponse(&engine.ChatResponse{
		Message: &engine.ResponseMessage{Content: &empty},
	}))
	if got.Message == nil || got.Message.Content == nil || *got.Message.Content != "" {
		t.Fatalf("present-empty content lost: %+v", got.Message)
	}
}

// An error response has no message. Plugins must not assume one is present,
// and the conversion must not fabricate an empty assistant turn.
func TestErrorResponseCarriesNoMessage(t *testing.T) {
	got := FromPBChatResponse(ToPBChatResponse(&engine.ChatResponse{
		Model: "m", UpstreamStatus: 503,
	}))
	if got.Message != nil {
		t.Fatalf("invented a message for an error response: %+v", got.Message)
	}
	if got.UpstreamStatus != 503 {
		t.Fatalf("upstream status lost: %d", got.UpstreamStatus)
	}
}
