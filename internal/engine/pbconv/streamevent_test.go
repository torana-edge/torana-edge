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

// Non-tool ContentBlockStart arms now have an engine counterpart: the IR
// carries explicit text/thinking/provider block events (BlockStart), so the
// inbound direction maps them instead of dropping them. This pins that the
// arm's kind and index survive, and that the mapping never fabricates delta
// content or a tool call.
func TestNonToolBlockStartsMapToBlockStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		pb   *pb.StreamEvent
		kind engine.BlockKind
	}{
		{"text", &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 0,
				Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
			},
		}}, engine.BlockKindText},
		{"thinking", &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 1,
				Block: &pb.ContentBlockStart_Thinking{Thinking: &pb.ThinkingBlock{}},
			},
		}}, engine.BlockKindThinking},
		{"provider", &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: 2,
				Block: &pb.ContentBlockStart_Provider{Provider: &pb.ProviderBlock{Kind: "redacted"}},
			},
		}}, engine.BlockKindProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FromPBStreamEvent(tc.pb)
			if got.ToolCallStart != nil {
				t.Fatal("a non-tool block start became a tool call")
			}
			if got.BlockStart == nil {
				t.Fatal("non-tool block start was dropped — the IR must keep the block topology")
			}
			if got.BlockStart.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", got.BlockStart.Kind, tc.kind)
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
		got := FromPBStreamEvent(tc)
		if got.BlockStart != nil || got.ToolCallStart != nil {
			t.Fatalf("nil-payload block start produced an event: %+v", got)
		}
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

// convertWithKinds walks a pb stream through BlockKindTracker so a stop
// resolves by the kind of the block it closes — the stream-aware conversion
// the host uses for plugin-passed streams.
func convertWithKinds(t *testing.T, events []*pb.StreamEvent) []engine.StreamEvent {
	t.Helper()
	tracker := &BlockKindTracker{}
	out := make([]engine.StreamEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, *tracker.FromPBStreamEvent(ev))
	}
	return out
}

// The v2 topology must survive engine → pb → engine exactly: a text/thinking
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
		if stops[i].Index != i {
			t.Errorf("stop %d index = %d, want %d", i, stops[i].Index, i)
		}
	}

	// Convert back: arms and indexes must come out identical.
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
			// The arm (text/thinking/provider) must survive; the provider
			// kind string is not representable in the engine IR, so it is
			// normalized to the canonical default on the way back out.
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
				} else if p.Provider.Kind != "provider" {
					t.Errorf("event %d: provider kind = %q, want canonical %q", i, p.Provider.Kind, "provider")
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

// A stop whose index was never recorded as an open block resolves to BlockStop
// (defensive): the stop closes non-tool content rather than inventing a tool
// block. Tool stops only resolve to ToolCallEnd when a tool start recorded the
// index.
func TestStopWithNoRecordedOpenBlockIsBlockStop(t *testing.T) {
	tracker := &BlockKindTracker{}
	got := tracker.FromPBStreamEvent(&pb.StreamEvent{
		Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 4}},
	})
	if got.BlockStop == nil || got.BlockStop.Index != 4 {
		t.Fatalf("unrecorded stop = %+v, want BlockStop(4)", got)
	}
	if got.ToolCallEnd != nil {
		t.Fatalf("unrecorded stop invented a ToolCallEnd: %+v", got.ToolCallEnd)
	}
}

// The stateless per-event conversion keeps the pre-block-IR behavior: a lone
// ContentBlockStop maps to ToolCallEnd. Streams that must preserve non-tool
// block boundaries use BlockKindTracker.
func TestStatelessStopMapsToToolCallEnd(t *testing.T) {
	got := FromPBStreamEvent(&pb.StreamEvent{
		Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 0}},
	})
	if got.ToolCallEnd == nil || got.ToolCallEnd.Index != 0 {
		t.Fatalf("stateless stop = %+v, want ToolCallEnd(0)", got)
	}
	if got.BlockStop != nil {
		t.Fatalf("stateless stop invented a BlockStop: %+v", got.BlockStop)
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
