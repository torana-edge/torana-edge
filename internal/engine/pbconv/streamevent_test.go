package pbconv

import (
	"bytes"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// current ABI replaced three v1 events with the canonical content-block sequence. The
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
	stream := []*engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "c", Name: "n", Signature: "s"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"a":1}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		{FinishReason: "tool_use"},
	}
	// The whole stream walks ONE tracker: a stop resolves by the kind of the
	// block its start recorded, never by a stateless guess.
	tracker := &BlockKindTracker{}
	for i, in := range stream {
		got, err := tracker.FromPBStreamEvent(ToPBStreamEvent(in))
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
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

// Non-tool ContentBlockStart arms now have an engine counterpart: the IR
// carries explicit text/thinking/provider block events (BlockStart), so the
// inbound direction maps them instead of dropping them. This pins that the
// arm's kind and index survive, and that the mapping never fabricates delta
// content or a tool call.
func TestNonToolBlockStartsMapToBlockStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pb           *pb.StreamEvent
		kind         engine.BlockKind
		providerKind string
	}{
		{"text", &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
			},
		}}, engine.BlockKindText, ""},
		{"thinking", &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 1,
				Block: &pb.ContentBlockStart_Thinking{Thinking: &pb.ThinkingBlock{}},
			},
		}}, engine.BlockKindThinking, ""},
		{"provider", &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 2,
				Block: &pb.ContentBlockStart_Provider{Provider: &pb.ProviderBlock{Kind: "redacted"}},
			},
		}}, engine.BlockKindProvider, "redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&BlockKindTracker{}).FromPBStreamEvent(tc.pb)
			if err != nil {
				t.Fatalf("conversion: %v", err)
			}
			if got.ToolCallStart != nil {
				t.Fatal("a non-tool block start became a tool call")
			}
			if got.BlockStart == nil {
				t.Fatal("non-tool block start was dropped — the IR must keep the block topology")
			}
			if got.BlockStart.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", got.BlockStart.Kind, tc.kind)
			}
			if tc.kind == engine.BlockKindProvider && got.BlockStart.ProviderKind != tc.providerKind {
				t.Errorf("provider kind = %q, want %q — ProviderBlock.kind passes through verbatim", got.BlockStart.ProviderKind, tc.providerKind)
			}
			if got.TextDelta != nil || got.ThinkingDelta != nil {
				t.Fatal("a block start invented delta content")
			}
		})
	}
}

// A ContentBlockStart arm whose payload is nil must not produce a BlockStart:
// the arm names a kind it does not carry, so the host drops it rather than
// invent a block. (The SDK's Validate refuses such starts; this guards the
// decode path regardless.)
func TestNonToolBlockStartWithoutPayloadIsSafe(t *testing.T) {
	for _, tc := range []*pb.StreamEvent{
		{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_Text{Text: nil},
			},
		}},
		{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_Thinking{Thinking: nil},
			},
		}},
		{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_Provider{Provider: nil},
			},
		}},
	} {
		got, err := (&BlockKindTracker{}).FromPBStreamEvent(tc)
		if err != nil {
			t.Fatalf("conversion: %v", err)
		}
		if got.BlockStart != nil || got.ToolCallStart != nil {
			t.Fatalf("nil-payload block start produced an event: %+v", got)
		}
	}
}

// A tool-call block start carrying no ref must not panic or produce a
// half-built tool call. The host decodes bytes a guest controls, so the guest
// picks when this path runs.
func TestToolBlockStartWithoutRefIsSafe(t *testing.T) {
	got, err := (&BlockKindTracker{}).FromPBStreamEvent(&pb.StreamEvent{
		Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_ToolCall{ToolCall: nil},
			},
		},
	})
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	if got.ToolCallStart != nil {
		t.Fatalf("built a tool call from a nil ref: %+v", got.ToolCallStart)
	}
}

// convertWithKinds walks a pb stream through BlockKindTracker so a stop
// resolves by the kind of the block it closes — the stream-aware conversion
// the host uses for plugin-passed streams.
func convertWithKinds(t *testing.T, events []*pb.StreamEvent) []engine.StreamEvent {
	t.Helper()
	tracker := &BlockKindTracker{}
	out := make([]engine.StreamEvent, 0, len(events))
	for _, ev := range events {
		converted, err := tracker.FromPBStreamEvent(ev)
		if err != nil {
			t.Fatalf("conversion: %v", err)
		}
		out = append(out, *converted)
	}
	return out
}

// The current ABI topology must survive engine → pb → engine exactly: a text/thinking
// block's start AND stop keep their kind and index, and a tool block's stop
// stays ToolCallEnd (never BlockStop). This is the lossless round trip the
// host needs for plugin-passed streams.
func TestBlockTopologyRoundTripsEngineToPBToEngine(t *testing.T) {
	text := "A"
	sig := "S"
	stream := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{TextDelta: &text},
		{SignatureDelta: &sig},
		{BlockStop: &engine.BlockStop{Index: 0}},
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "c1", Name: "read_file"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		{BlockStart: &engine.BlockStart{Index: 2, Kind: engine.BlockKindThinking}},
		{ThinkingDelta: &text},
		{BlockStop: &engine.BlockStop{Index: 2}},
	}

	var pbEvents []*pb.StreamEvent
	for i := range stream {
		pbEvents = append(pbEvents, ToPBStreamEvent(&stream[i]))
	}
	got := convertWithKinds(t, pbEvents)

	if len(got) != len(stream) {
		t.Fatalf("event count changed through the round trip: got %d, want %d", len(got), len(stream))
	}
	for i := range stream {
		switch {
		case stream[i].BlockStart != nil:
			if got[i].BlockStart == nil || *got[i].BlockStart != *stream[i].BlockStart {
				t.Errorf("event %d BlockStart round trip: got %+v, want %+v", i, got[i].BlockStart, stream[i].BlockStart)
			}
		case stream[i].BlockStop != nil:
			if got[i].BlockStop == nil || *got[i].BlockStop != *stream[i].BlockStop {
				t.Errorf("event %d BlockStop round trip: got %+v, want %+v (a non-tool stop must not become ToolCallEnd)", i, got[i].BlockStop, stream[i].BlockStop)
			}
		case stream[i].ToolCallStart != nil:
			if got[i].ToolCallStart == nil || *got[i].ToolCallStart != *stream[i].ToolCallStart {
				t.Errorf("event %d ToolCallStart round trip: got %+v, want %+v", i, got[i].ToolCallStart, stream[i].ToolCallStart)
			}
		case stream[i].ToolCallDelta != nil:
			if got[i].ToolCallDelta == nil || *got[i].ToolCallDelta != *stream[i].ToolCallDelta {
				t.Errorf("event %d ToolCallDelta round trip: got %+v, want %+v", i, got[i].ToolCallDelta, stream[i].ToolCallDelta)
			}
		case stream[i].ToolCallEnd != nil:
			if got[i].ToolCallEnd == nil || *got[i].ToolCallEnd != *stream[i].ToolCallEnd {
				t.Errorf("event %d ToolCallEnd round trip: got %+v, want %+v (a tool stop must not become BlockStop)", i, got[i].ToolCallEnd, stream[i].ToolCallEnd)
			}
		case stream[i].TextDelta != nil:
			if got[i].TextDelta == nil || *got[i].TextDelta != *stream[i].TextDelta {
				t.Errorf("event %d TextDelta round trip: got %+v, want %+v", i, got[i].TextDelta, stream[i].TextDelta)
			}
		case stream[i].ThinkingDelta != nil:
			if got[i].ThinkingDelta == nil || *got[i].ThinkingDelta != *stream[i].ThinkingDelta {
				t.Errorf("event %d ThinkingDelta round trip: got %+v, want %+v", i, got[i].ThinkingDelta, stream[i].ThinkingDelta)
			}
		case stream[i].SignatureDelta != nil:
			if got[i].SignatureDelta == nil || *got[i].SignatureDelta != *stream[i].SignatureDelta {
				t.Errorf("event %d SignatureDelta round trip: got %+v, want %+v", i, got[i].SignatureDelta, stream[i].SignatureDelta)
			}
		}
	}
}

// The inbound direction must preserve the wire's block arms: a pb stream with
// ContentBlockStart{text|thinking|provider} and matching ContentBlockStops
// converts to engine events whose BlockStarts carry the right kinds and whose
// stops resolve to BlockStop, and converting back reproduces the same arms
// and indexes.
func TestBlockTopologyRoundTripsPBToEngineToPB(t *testing.T) {
	pbStream := []*pb.StreamEvent{
		{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
			Index: 0, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
		}}},
		{Event: &pb.StreamEvent_TextDelta{TextDelta: "A"}},
		{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 0}}},
		{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
			Index: 1, Block: &pb.ContentBlockStart_Thinking{Thinking: &pb.ThinkingBlock{}},
		}}},
		{Event: &pb.StreamEvent_ThinkingDelta{ThinkingDelta: "T"}},
		{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 1}}},
		{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
			Index: 2, Block: &pb.ContentBlockStart_Provider{Provider: &pb.ProviderBlock{Kind: "redacted"}},
		}}},
		{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 2}}},
	}

	eng := convertWithKinds(t, pbStream)
	wantKinds := []engine.BlockKind{engine.BlockKindText, engine.BlockKindThinking, engine.BlockKindProvider}
	var starts []*engine.BlockStart
	var stops []*engine.BlockStop
	for _, ev := range eng {
		if ev.BlockStart != nil {
			starts = append(starts, ev.BlockStart)
		}
		if ev.BlockStop != nil {
			stops = append(stops, ev.BlockStop)
		}
		if ev.ToolCallStart != nil || ev.ToolCallEnd != nil {
			t.Fatalf("non-tool block converted to a tool event: %+v", ev)
		}
	}
	if len(starts) != 3 || len(stops) != 3 {
		t.Fatalf("got %d starts / %d stops, want 3/3", len(starts), len(stops))
	}
	for i, want := range wantKinds {
		if starts[i].Kind != want || starts[i].Index != i {
			t.Errorf("start %d = kind %v index %d, want kind %v index %d", i, starts[i].Kind, starts[i].Index, want, i)
		}
		if want == engine.BlockKindProvider && starts[i].ProviderKind != "redacted" {
			t.Errorf("start %d: provider kind = %q, want %q — ProviderBlock.kind must survive verbatim", i, starts[i].ProviderKind, "redacted")
		}
		if stops[i].Index != i {
			t.Errorf("stop %d index = %d, want %d", i, stops[i].Index, i)
		}
	}

	// Convert back: arms, indexes, and the provider kind string must come
	// out identical — the ABI passes ProviderBlock.kind VERBATIM.
	var back []*pb.StreamEvent
	for i := range eng {
		back = append(back, ToPBStreamEvent(&eng[i]))
	}
	for i, orig := range pbStream {
		switch ob := orig.Event.(type) {
		case *pb.StreamEvent_ContentBlockStart:
			backCBS, ok := back[i].Event.(*pb.StreamEvent_ContentBlockStart)
			if !ok {
				t.Fatalf("event %d: start did not survive: %T", i, back[i].Event)
			}
			if backCBS.ContentBlockStart.Index != ob.ContentBlockStart.Index {
				t.Errorf("event %d: start index = %d, want %d", i, backCBS.ContentBlockStart.Index, ob.ContentBlockStart.Index)
			}
			switch ob.ContentBlockStart.Block.(type) {
			case *pb.ContentBlockStart_Text:
				if _, ok := backCBS.ContentBlockStart.Block.(*pb.ContentBlockStart_Text); !ok {
					t.Errorf("event %d: text arm became %T", i, backCBS.ContentBlockStart.Block)
				}
			case *pb.ContentBlockStart_Thinking:
				if _, ok := backCBS.ContentBlockStart.Block.(*pb.ContentBlockStart_Thinking); !ok {
					t.Errorf("event %d: thinking arm became %T", i, backCBS.ContentBlockStart.Block)
				}
			case *pb.ContentBlockStart_Provider:
				p, ok := backCBS.ContentBlockStart.Block.(*pb.ContentBlockStart_Provider)
				if !ok {
					t.Errorf("event %d: provider arm became %T", i, backCBS.ContentBlockStart.Block)
				} else if p.Provider.Kind != "redacted" {
					t.Errorf("event %d: provider kind = %q, want verbatim %q", i, p.Provider.Kind, "redacted")
				}
			}
		case *pb.StreamEvent_ContentBlockStop:
			backCBE, ok := back[i].Event.(*pb.StreamEvent_ContentBlockStop)
			if !ok {
				t.Fatalf("event %d: stop did not survive: %T", i, back[i].Event)
			}
			if backCBE.ContentBlockStop.Index != ob.ContentBlockStop.Index {
				t.Errorf("event %d: stop index = %d, want %d", i, backCBE.ContentBlockStop.Index, ob.ContentBlockStop.Index)
			}
		case *pb.StreamEvent_TextDelta:
			td, ok := back[i].Event.(*pb.StreamEvent_TextDelta)
			if !ok || td.TextDelta != ob.TextDelta {
				t.Errorf("event %d: text delta lost: %v", i, back[i].Event)
			}
		case *pb.StreamEvent_ThinkingDelta:
			td, ok := back[i].Event.(*pb.StreamEvent_ThinkingDelta)
			if !ok || td.ThinkingDelta != ob.ThinkingDelta {
				t.Errorf("event %d: thinking delta lost: %v", i, back[i].Event)
			}
		}
	}
}

// A stop whose index names no currently open block is INVALID per the plugin ABI:
// the stop binds by index alone, and a stop that does not close the open block
// at its index is unknown topology. The conversion must error, never guess a
// kind (the pre-fix stateless path mapped every such stop to ToolCallEnd).
func TestStopWithNoOpenBlockErrors(t *testing.T) {
	tracker := &BlockKindTracker{}
	_, err := tracker.FromPBStreamEvent(&pb.StreamEvent{
		Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 4}},
	})
	if err == nil {
		t.Fatal("stop with no open block did not error — unknown topology must not be guessed")
	}
}

// A stop for an index that was already closed (stop-after-close) is the same
// invalid topology: the index no longer names an open block.
func TestStopAfterCloseErrors(t *testing.T) {
	tracker := &BlockKindTracker{}
	start := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 0, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
	}}}
	stop := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 0}}}
	if _, err := tracker.FromPBStreamEvent(start); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := tracker.FromPBStreamEvent(stop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := tracker.FromPBStreamEvent(stop); err == nil {
		t.Fatal("stop for an already-closed index did not error")
	}
}

// A second start at an index that already has an open block (duplicate/reused
// topology) is invalid: the current ABI wire binds deltas and stops by index, so two
// starts on one index would interleave two blocks' events.
func TestDuplicateStartErrors(t *testing.T) {
	tracker := &BlockKindTracker{}
	text := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 0, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
	}}}
	tool := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 0, Block: &pb.ContentBlockStart_ToolCall{ToolCall: &pb.ToolCallRef{Id: "c", Name: "n"}},
	}}}
	if _, err := tracker.FromPBStreamEvent(text); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := tracker.FromPBStreamEvent(tool); err == nil {
		t.Fatal("start at an index with an open block did not error — reused topology must not be guessed")
	}
}

// At most ONE content block may be open at a time: a second start at a
// DIFFERENT index while a block is open is invalid — the index-bound wire
// cannot interleave two blocks' events.
func TestSecondStartWhileOpenRejected(t *testing.T) {
	tracker := &BlockKindTracker{}
	text := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 0, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
	}}}
	thinking := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 1, Block: &pb.ContentBlockStart_Thinking{Thinking: &pb.ThinkingBlock{}},
	}}}
	if _, err := tracker.FromPBStreamEvent(text); err != nil {
		t.Fatalf("first start: %v", err)
	}
	_, err := tracker.FromPBStreamEvent(thinking)
	if err == nil {
		t.Fatal("second start at a different index while a block is open did not error")
	}
	want := "pbconv: content block start at index 1 while a text block at index 0 is still open"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// Indexes are unique across the ENTIRE streamed message: a start whose index
// was already used is invalid EVEN AFTER its block closed. One response stream
// is one streamed message; the tracker is request-scoped and drops
// MessageStart, so it cannot infer a reset between "messages".
func TestIndexReuseAfterCloseRejected(t *testing.T) {
	tracker := &BlockKindTracker{}
	text := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 0, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
	}}}
	stop := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 0}}}
	tool := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 0, Block: &pb.ContentBlockStart_ToolCall{ToolCall: &pb.ToolCallRef{Id: "c", Name: "n"}},
	}}}
	for i, ev := range []*pb.StreamEvent{text, stop} {
		if _, err := tracker.FromPBStreamEvent(ev); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	_, err := tracker.FromPBStreamEvent(tool)
	if err == nil {
		t.Fatal("start at a closed index within one message did not error — indexes must stay unique per message")
	}
	want := "pbconv: content block index 0 reused within one streamed message"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// A stop must name the single open block: a stop at a different index while a
// block is open is invalid topology, never a guess.
func TestMismatchedStopRejected(t *testing.T) {
	tracker := &BlockKindTracker{}
	text := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 5, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
	}}}
	stop := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 3}}}
	if _, err := tracker.FromPBStreamEvent(text); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err := tracker.FromPBStreamEvent(stop)
	if err == nil {
		t.Fatal("stop at a different index than the open block did not error")
	}
	want := "pbconv: content block stop at index 3 does not match the open text block at index 5"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func toolBlockStart(idx int, id, name string) *pb.StreamEvent {
	return &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: int32(idx), Block: &pb.ContentBlockStart_ToolCall{ToolCall: &pb.ToolCallRef{Id: id, Name: name}},
	}}}
}

func toolBlockStop(idx int) *pb.StreamEvent {
	return &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: int32(idx)}}}
}

// The round-4 contract: MULTIPLE tool blocks may be open concurrently at
// unique indexes — parallel tool calls ride the native protocols. A second
// tool start while a tool block is open is legal (this was rejected by the
// round-3 single-open rule), and each stop resolves its own block by index,
// kind-matched, regardless of the order stops arrive in.
func TestConcurrentToolBlocksAccepted(t *testing.T) {
	tracker := &BlockKindTracker{}
	stream := []*pb.StreamEvent{
		toolBlockStart(0, "call_a", "get_weather"),
		toolBlockStart(1, "call_b", "get_time"),
		toolBlockStart(2, "call_c", "get_news"),
		// Stops in non-ascending order: each binds its own index.
		toolBlockStop(2),
		toolBlockStop(0),
		toolBlockStop(1),
	}
	for i, ev := range stream {
		got, err := tracker.FromPBStreamEvent(ev)
		if err != nil {
			t.Fatalf("event %d: parallel tool start/stop must be accepted, got error: %v", i, err)
		}
		switch arm := ev.Event.(type) {
		case *pb.StreamEvent_ContentBlockStart:
			if got.ToolCallStart == nil || got.ToolCallStart.Index != int(arm.ContentBlockStart.Index) {
				t.Fatalf("event %d: tool start lost its index: %+v", i, got)
			}
		case *pb.StreamEvent_ContentBlockStop:
			if got.ToolCallEnd == nil || got.ToolCallEnd.Index != int(arm.ContentBlockStop.Index) {
				t.Fatalf("event %d: tool stop did not lower to ToolCallEnd at its own index: %+v", i, got)
			}
		}
	}
	// Everything is closed: a fresh start is legal again.
	if _, err := tracker.FromPBStreamEvent(toolBlockStart(3, "call_d", "again")); err != nil {
		t.Fatalf("start after all parallel blocks closed: %v", err)
	}
}

// A stop naming an index with no open block is unknown topology even while
// OTHER tool blocks are open: the stop binds by index, so an index that is
// not open cannot be closed. The tracker must error, never guess.
func TestUnknownStopRejectedWhileToolsOpen(t *testing.T) {
	tracker := &BlockKindTracker{}
	if _, err := tracker.FromPBStreamEvent(toolBlockStart(0, "call_a", "a")); err != nil {
		t.Fatalf("start 0: %v", err)
	}
	if _, err := tracker.FromPBStreamEvent(toolBlockStart(1, "call_b", "b")); err != nil {
		t.Fatalf("start 1: %v", err)
	}
	_, err := tracker.FromPBStreamEvent(toolBlockStop(4))
	if err == nil {
		t.Fatal("stop for an index with no open tool block did not error")
	}
	want := "pbconv: content block stop at index 4 has no open block (unknown, mismatched, or already-closed topology)"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// A non-tool block is exclusive: it may not open while ANY tool block is
// open, no matter how many. The error names the count of open tool blocks.
func TestNonToolStartWhileToolOpenRejected(t *testing.T) {
	tracker := &BlockKindTracker{}
	if _, err := tracker.FromPBStreamEvent(toolBlockStart(0, "call_a", "a")); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if _, err := tracker.FromPBStreamEvent(toolBlockStart(1, "call_b", "b")); err != nil {
		t.Fatalf("tool start 1: %v", err)
	}
	for name, start := range map[string]*pb.StreamEvent{
		"text":     {Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{Index: 2, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}}}}},
		"thinking": {Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{Index: 2, Block: &pb.ContentBlockStart_Thinking{Thinking: &pb.ThinkingBlock{}}}}},
	} {
		_, err := tracker.FromPBStreamEvent(start)
		if err == nil {
			t.Fatalf("%s start while tool blocks are open did not error", name)
		}
		want := "pbconv: content block start at index 2 while 2 tool block(s) are still open"
		if err.Error() != want {
			t.Errorf("%s: error = %q, want %q", name, err.Error(), want)
		}
	}
}

// A tool block may not open while a non-tool block is open: the index-bound
// wire cannot interleave a non-tool block with anything else. The tool start
// gets the same single-open error a non-tool start would.
func TestToolStartWhileNonToolOpenRejected(t *testing.T) {
	tracker := &BlockKindTracker{}
	if _, err := tracker.FromPBStreamEvent(&pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 5, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
	}}}); err != nil {
		t.Fatalf("text start: %v", err)
	}
	_, err := tracker.FromPBStreamEvent(toolBlockStart(6, "call_a", "a"))
	if err == nil {
		t.Fatal("tool start while a text block is open did not error")
	}
	want := "pbconv: content block start at index 6 while a text block at index 5 is still open"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// ProviderBlock.kind is topology the host must not change: pin that
// {kind:"redacted"} PB → engine → PB is field-equivalent (index, arm, kind
// string all identical). The pre-fix test blessed "redacted"→"provider"; that
// normalization is the loss this replaces.
func TestProviderKindPassesThroughVerbatim(t *testing.T) {
	pbStart := &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{ContentBlockStart: &pb.ContentBlockStart{
		Index: 2, Block: &pb.ContentBlockStart_Provider{Provider: &pb.ProviderBlock{Kind: "redacted"}},
	}}}

	eng, err := (&BlockKindTracker{}).FromPBStreamEvent(pbStart)
	if err != nil {
		t.Fatalf("pb → engine: %v", err)
	}
	if eng.BlockStart == nil || eng.BlockStart.Kind != engine.BlockKindProvider || eng.BlockStart.ProviderKind != "redacted" {
		t.Fatalf("pb → engine lost the provider kind: %+v", eng.BlockStart)
	}

	back := ToPBStreamEvent(eng)
	backCBS, ok := back.Event.(*pb.StreamEvent_ContentBlockStart)
	if !ok {
		t.Fatalf("engine → pb became %T", back.Event)
	}
	p, ok := backCBS.ContentBlockStart.Block.(*pb.ContentBlockStart_Provider)
	if !ok {
		t.Fatalf("engine → pb provider arm became %T", backCBS.ContentBlockStart.Block)
	}
	if backCBS.ContentBlockStart.Index != 2 || p.Provider.Kind != "redacted" {
		t.Fatalf("round trip not field-equivalent: index %d kind %q, want index 2 kind %q",
			backCBS.ContentBlockStart.Index, p.Provider.Kind, "redacted")
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
