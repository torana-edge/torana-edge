package pbconv

import (
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
