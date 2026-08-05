package gemini

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// parseStreamSSE parses one or more SSE frames (one frame per line) and returns
// the emitted events in order.
func parseStreamSSE(t *testing.T, sse string) []engine.StreamEvent {
	t.Helper()
	return drain((&StreamAdapter{Wrapped: true}).ParseStream(strings.NewReader(sse)))
}

// TestStreamToolCallBlockIndexesParallelInOneChunk pins the ABI invariant
// (SignatureScopeToolCallBlockByIndex): block indexes must be unique within one
// streamed message, so parallel Gemini parts in a single chunk get distinct
// sequential indexes, and each block's Start/Delta/End events share that index.
func TestStreamToolCallBlockIndexesParallelInOneChunk(t *testing.T) {
	frame := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"list_dir","args":{"p":"/x"},"id":"c1"}},` +
		`{"functionCall":{"name":"read_file","args":{"f":"y"},"id":"c2"}}` +
		`]}}]}}`
	events := parseStreamSSE(t, frame)

	var got []int
	var names []string
	for _, ev := range events {
		switch {
		case ev.ToolCallStart != nil:
			got = append(got, ev.ToolCallStart.Index)
			names = append(names, ev.ToolCallStart.Name)
		case ev.ToolCallDelta != nil:
			got = append(got, ev.ToolCallDelta.Index)
		case ev.ToolCallEnd != nil:
			got = append(got, ev.ToolCallEnd.Index)
		case ev.Error != nil:
			t.Fatalf("stream error: %s", ev.Error.Message)
		}
	}
	want := []int{0, 0, 0, 1, 1, 1} // start, delta, end per block; sequential across parallel calls
	if !slices.Equal(got, want) {
		t.Fatalf("tool event indexes = %v, want %v — parallel Gemini parts need distinct sequential block indexes",
			got, want)
	}
	if len(names) != 2 || names[0] != "list_dir" || names[1] != "read_file" {
		t.Fatalf("tool call names = %v, want [list_dir read_file]", names)
	}
}

func TestStreamRejectsMultipleCandidatesInsteadOfDroppingThem(t *testing.T) {
	frame := `data: {"response":{"candidates":[` +
		`{"content":{"role":"model","parts":[{"text":"first"}]}},` +
		`{"content":{"role":"model","parts":[{"text":"second"}]}}]}}`
	events := parseStreamSSE(t, frame)
	if len(events) != 1 || events[0].Error == nil {
		t.Fatalf("events = %+v, want one terminal error", events)
	}
	if events[0].Error.Message != "gemini: multiple streamed candidates are unsupported" {
		t.Fatalf("error = %q", events[0].Error.Message)
	}
}

// TestStreamToolCallBlockIndexesSharedAcrossChunks pins the same invariant when
// parts arrive split over multiple SSE frames: the per-stream block counter
// must carry across chunks, and it counts EVERY block — text, thinking, and
// tool — so the leading text part consumes index 0 and the two tool calls take
// 1 and 2. It also pins that a trailing signature-only part still emits a
// standalone SignatureDelta (SignatureScopeTrailingStandalone).
func TestStreamToolCallBlockIndexesSharedAcrossChunks(t *testing.T) {
	frames := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"thinking out loud"},` +
		`{"functionCall":{"name":"list_dir","args":{"p":"/x"},"id":"c1"}}` +
		`]}}]}}` + "\n" +
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"read_file","args":{"f":"y"},"id":"c2"}}` +
		`]}}]}}` + "\n" +
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"thoughtSignature":"SIG_FINAL","text":""}` +
		`]},"finishReason":"STOP"}]}}`
	events := parseStreamSSE(t, frames)

	var got []int
	var sawText, sawSig bool
	for _, ev := range events {
		switch {
		case ev.BlockStart != nil:
			if ev.BlockStart.Kind != engine.BlockKindText {
				t.Errorf("leading text part opened a %v block, want text", ev.BlockStart.Kind)
			}
		case ev.ToolCallStart != nil:
			got = append(got, ev.ToolCallStart.Index)
		case ev.ToolCallDelta != nil:
			got = append(got, ev.ToolCallDelta.Index)
		case ev.ToolCallEnd != nil:
			got = append(got, ev.ToolCallEnd.Index)
		case ev.TextDelta != nil:
			sawText = true
		case ev.SignatureDelta != nil:
			if *ev.SignatureDelta != "SIG_FINAL" {
				t.Errorf("signature delta = %q, want SIG_FINAL", *ev.SignatureDelta)
			}
			sawSig = true
		case ev.Error != nil:
			t.Fatalf("stream error: %s", ev.Error.Message)
		}
	}
	// Every block counts: text@0, then tool@1 and tool@2.
	want := []int{1, 1, 1, 2, 2, 2}
	if !slices.Equal(got, want) {
		t.Fatalf("tool event indexes = %v, want %v — the shared counter counts text, thinking and tool blocks", got, want)
	}
	if !sawText {
		t.Error("text part lost")
	}
	if !sawSig {
		t.Error("signature-only final part did not emit a standalone SignatureDelta")
	}
}

// rawStreamParts re-parses serialized SSE output and returns every part of the
// first candidate's content as RAW MAPS — the wire shape asserted by the
// round-trip tests, including key presence (an explicit "text":"" is distinct
// from an absent text member, which the lossy geminiPart struct cannot see).
func rawStreamParts(t *testing.T, output string) []map[string]any {
	t.Helper()
	var parts []map[string]any
	for _, block := range strings.Split(output, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		data, ok := strings.CutPrefix(block, "data:")
		if !ok {
			t.Fatalf("frame missing data: prefix: %q", block)
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &frame); err != nil {
			t.Fatalf("frame not valid JSON: %v (%q)", err, block)
		}
		if resp, ok := frame["response"].(map[string]any); ok {
			frame = resp
		}
		cands, _ := frame["candidates"].([]any)
		if len(cands) == 0 {
			continue
		}
		content, _ := cands[0].(map[string]any)["content"].(map[string]any)
		if content == nil {
			continue
		}
		ps, _ := content["parts"].([]any)
		for _, p := range ps {
			parts = append(parts, p.(map[string]any))
		}
	}
	return parts
}

func serializeEvents(t *testing.T, events ...engine.StreamEvent) string {
	t.Helper()
	ch := make(chan engine.StreamEvent, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	var buf strings.Builder
	if err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, ch); err != nil {
		t.Fatalf("SerializeStream: %v", err)
	}
	return buf.String()
}

// TestStreamCurrentTextSignatureRoundTrip: a wire part {text:"A",
// thoughtSignature:"S"} opens a text block whose signature rides INSIDE the
// open block (BlockStart → TextDelta → SignatureDelta → BlockStop); the
// serializer must re-emit ONE part with the signature beside the text — not a
// split part and not a standalone signature part.
func TestStreamCurrentTextSignatureRoundTrip(t *testing.T) {
	wire := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"A","thoughtSignature":"S"}` +
		`]}}]}}`
	events := parseStreamSSE(t, wire)

	// Reader: the signature stays inside the open block.
	want := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{TextDelta: strPtr("A")},
		{SignatureDelta: strPtr("S")},
		{BlockStop: &engine.BlockStop{Index: 0}},
	}
	assertEventSequence(t, events, want)

	parts := rawStreamParts(t, serializeEvents(t, events...))
	if len(parts) != 1 {
		t.Fatalf("serialized %d parts, want exactly 1 (signature must not split or trail): %v", len(parts), parts)
	}
	if parts[0]["text"] != "A" || parts[0]["thoughtSignature"] != "S" {
		t.Errorf("part = %v, want {text:A thoughtSignature:S}", parts[0])
	}
}

// TestStreamCurrentThinkingSignatureRoundTrip: the same topology for a
// thought part — one {thought:true, text:"T", thoughtSignature:"S"} part.
func TestStreamCurrentThinkingSignatureRoundTrip(t *testing.T) {
	wire := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"thought":true,"text":"T","thoughtSignature":"S"}` +
		`]}}]}}`
	events := parseStreamSSE(t, wire)

	want := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindThinking}},
		{ThinkingDelta: strPtr("T")},
		{SignatureDelta: strPtr("S")},
		{BlockStop: &engine.BlockStop{Index: 0}},
	}
	assertEventSequence(t, events, want)

	parts := rawStreamParts(t, serializeEvents(t, events...))
	if len(parts) != 1 {
		t.Fatalf("serialized %d parts, want exactly 1: %v", len(parts), parts)
	}
	if parts[0]["text"] != "T" || parts[0]["thought"] != true || parts[0]["thoughtSignature"] != "S" {
		t.Errorf("part = %v, want {thought:true text:T thoughtSignature:S}", parts[0])
	}
}

// TestStreamTrailingStandaloneSignatureRoundTrip: the signature-only part
// stays trailing and standalone — its own explicit empty-text part — never
// merged into the preceding text.
func TestStreamTrailingStandaloneSignatureRoundTrip(t *testing.T) {
	wire := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"A"},{"text":"","thoughtSignature":"S"}` +
		`]}}]}}`
	events := parseStreamSSE(t, wire)

	want := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{TextDelta: strPtr("A")},
		{BlockStop: &engine.BlockStop{Index: 0}},
		{SignatureDelta: strPtr("S")}, // no block events around it
	}
	assertEventSequence(t, events, want)

	parts := rawStreamParts(t, serializeEvents(t, events...))
	if len(parts) != 2 {
		t.Fatalf("serialized %d parts, want exactly 2: %v", len(parts), parts)
	}
	if parts[0]["text"] != "A" {
		t.Errorf("part 0 = %v, want {text:A}", parts[0])
	}
	if _, hasSig := parts[0]["thoughtSignature"]; hasSig {
		t.Errorf("part 0 must not carry the trailing signature: %v", parts[0])
	}
	if tv, ok := parts[1]["text"]; !ok || tv != "" {
		t.Errorf("part 1 must keep the EXPLICIT empty text arm: %v", parts[1])
	}
	if parts[1]["thoughtSignature"] != "S" {
		t.Errorf("part 1 = %v, want thoughtSignature S", parts[1])
	}
}

// TestStreamEmptySignatureClearInsideBlock: an empty SignatureDelta inside an
// open block is a CLEAR marker — it overwrites any earlier signature, and the
// flushed part carries NO thoughtSignature (an empty marker must never become
// a signature on the wire).
func TestStreamEmptySignatureClearInsideBlock(t *testing.T) {
	parts := rawStreamParts(t, serializeEvents(t,
		engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		engine.StreamEvent{TextDelta: strPtr("A")},
		engine.StreamEvent{SignatureDelta: strPtr("S")},
		engine.StreamEvent{SignatureDelta: strPtr("")},
		engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}},
	))
	if len(parts) != 1 {
		t.Fatalf("serialized %d parts, want exactly 1: %v", len(parts), parts)
	}
	if parts[0]["text"] != "A" {
		t.Errorf("part = %v, want text A", parts[0])
	}
	if _, hasSig := parts[0]["thoughtSignature"]; hasSig {
		t.Errorf("empty clear marker leaked onto the wire as a signature: %v", parts[0])
	}
}

// TestStreamEmptyStandaloneSignatureEmitsNoPart pins the empty-clear-marker
// decision for the trailing case: a standalone SignatureDelta("") is a clear
// marker with nothing to clear — the serializer emits NO part for it, rather
// than a signature-only part carrying an empty token.
func TestStreamEmptyStandaloneSignatureEmitsNoPart(t *testing.T) {
	parts := rawStreamParts(t, serializeEvents(t,
		engine.StreamEvent{SignatureDelta: strPtr("")},
		engine.StreamEvent{FinishReason: "stop"},
	))
	if len(parts) != 0 {
		t.Fatalf("empty standalone signature emitted %d parts, want 0: %v", len(parts), parts)
	}
}

// TestStreamEmptySignedThinkingRoundTrip pins the reviewer's reproduction: an
// empty thinking block carrying a signature (BlockStart(thinking) →
// SignatureDelta → BlockStop) must serialize to {"thought":true,"text":"",
// "thoughtSignature":"S"} and PARSE BACK to the SAME events — it must not
// collapse to a bare SignatureDelta (the pre-fix value-based detection dropped
// the empty-text thought part and left the signature unbound).
func TestStreamEmptySignedThinkingRoundTrip(t *testing.T) {
	sig := "S"
	want := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindThinking}},
		{SignatureDelta: &sig},
		{BlockStop: &engine.BlockStop{Index: 0}},
	}

	parts := rawStreamParts(t, serializeEvents(t, want...))
	if len(parts) != 1 {
		t.Fatalf("serialized %d parts, want exactly 1: %v", len(parts), parts)
	}
	if parts[0]["text"] != "" || parts[0]["thought"] != true || parts[0]["thoughtSignature"] != "S" {
		t.Errorf("part = %v, want {thought:true text:\"\" thoughtSignature:S}", parts[0])
	}

	// Serialize → parse must reproduce the SAME events: an empty signed
	// thinking block, NOT a bare SignatureDelta.
	wire := serializeEvents(t, want...)
	events := parseStreamSSE(t, wire)
	assertEventSequence(t, events, want)
}

// TestStreamEmptyUnsignedBlocksRoundTrip: empty unsigned text and thinking
// blocks (zero-delta blocks) each open AND close a block, consuming an index,
// and survive serialize → parse unchanged — an empty part is a block, not
// nothing.
func TestStreamEmptyUnsignedBlocksRoundTrip(t *testing.T) {
	want := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{BlockStop: &engine.BlockStop{Index: 0}},
		{BlockStart: &engine.BlockStart{Index: 1, Kind: engine.BlockKindThinking}},
		{BlockStop: &engine.BlockStop{Index: 1}},
	}

	parts := rawStreamParts(t, serializeEvents(t, want...))
	if len(parts) != 2 {
		t.Fatalf("serialized %d parts, want 2: %v", len(parts), parts)
	}
	if parts[0]["text"] != "" {
		t.Errorf("part 0 = %v, want explicit empty text arm", parts[0])
	}
	if parts[1]["text"] != "" || parts[1]["thought"] != true {
		t.Errorf("part 1 = %v, want {thought:true text:\"\"}", parts[1])
	}

	events := parseStreamSSE(t, serializeEvents(t, want...))
	assertEventSequence(t, events, want)
}

// TestStreamEmptyBlockConsumesIndexBeforeToolCall: an empty {"text":""} part
// still opens and closes a text block, so the tool call that follows must take
// the NEXT index. The pre-fix value-based detection dropped the empty part
// (consuming no index) and shifted the tool call to index 0.
func TestStreamEmptyBlockConsumesIndexBeforeToolCall(t *testing.T) {
	wire := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":""},` +
		`{"functionCall":{"name":"list_dir","args":{"p":"/x"},"id":"c1"}}` +
		`]}}]}}`
	events := parseStreamSSE(t, wire)

	want := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{BlockStop: &engine.BlockStop{Index: 0}},
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "c1", Name: "list_dir"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"p":"/x"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
	}
	assertEventSequence(t, events, want)
}

// TestStreamProviderBlockSerializeErrors: a provider BlockStart cannot be
// rendered on the Gemini wire (there is no provider part arm), so the
// serializer must error explicitly — never cast it to a text part.
func TestStreamProviderBlockSerializeErrors(t *testing.T) {
	ch := make(chan engine.StreamEvent, 1)
	ch <- engine.StreamEvent{BlockStart: &engine.BlockStart{
		Index: 0, Kind: engine.BlockKindProvider, ProviderKind: "redacted",
	}}
	close(ch)
	var buf strings.Builder
	err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, ch)
	if err == nil {
		t.Fatal("provider block did not error — it must never be cast to a text part")
	}
	want := `gemini: provider block kind "redacted" is not supported by this serializer`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if buf.Len() != 0 {
		t.Errorf("serializer wrote %q before erroring — nothing may be emitted for an unrenderable block", buf.String())
	}
}

func TestStreamPluginEmittedBlocksSerializeAsParts(t *testing.T) {
	parts := rawStreamParts(t, serializeEvents(t,
		engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		engine.StreamEvent{TextDelta: strPtr("hello ")},
		engine.StreamEvent{TextDelta: strPtr("world")},
		engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}},
		engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 1, Kind: engine.BlockKindThinking}},
		engine.StreamEvent{ThinkingDelta: strPtr("reasoning")},
		engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 1}},
	))
	if len(parts) != 2 {
		t.Fatalf("serialized %d parts, want 2 (one per block): %v", len(parts), parts)
	}
	if parts[0]["text"] != "hello world" {
		t.Errorf("part 0 = %v, want merged text 'hello world'", parts[0])
	}
	if _, hasSig := parts[0]["thoughtSignature"]; hasSig {
		t.Errorf("part 0 invented a signature: %v", parts[0])
	}
	if parts[1]["text"] != "reasoning" || parts[1]["thought"] != true {
		t.Errorf("part 1 = %v, want {thought:true text:reasoning}", parts[1])
	}
}

// TestStreamBareDeltasStillSerializePerDelta pins the compat path: deltas with
// no open block (legacy streams/tests that never emit block events) still
// serialize as per-delta parts.
func TestStreamBareDeltasStillSerializePerDelta(t *testing.T) {
	parts := rawStreamParts(t, serializeEvents(t,
		engine.StreamEvent{TextDelta: strPtr("a")},
		engine.StreamEvent{ThinkingDelta: strPtr("b")},
	))
	if len(parts) != 2 {
		t.Fatalf("serialized %d parts, want 2 per-delta parts: %v", len(parts), parts)
	}
	if parts[0]["text"] != "a" {
		t.Errorf("part 0 = %v, want {text:a}", parts[0])
	}
	if parts[1]["text"] != "b" || parts[1]["thought"] != true {
		t.Errorf("part 1 = %v, want {thought:true text:b}", parts[1])
	}
}

// serializeEventsErr is serializeEvents minus the Fatalf: it returns the
// serializer error (nil when the stream serializes cleanly).
func serializeEventsErr(t *testing.T, events ...engine.StreamEvent) error {
	t.Helper()
	ch := make(chan engine.StreamEvent, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return (&StreamAdapter{}).SerializeStream(context.Background(), io.Discard, ch)
}

// TestSerializeInterleavedConcurrentToolCalls pins the index-aware tool-call
// state: parallel tool blocks with interleaved deltas and NON-ASCENDING stops
// must serialize to TWO functionCall parts, each carrying its OWN
// id/name/arguments AND its OWN thoughtSignature. The pre-fix single
// serializeState overwrote the first start with the second, appended both
// deltas to one part, silently dropped the first end, and swapped signatures
// between calls.
func TestSerializeInterleavedConcurrentToolCalls(t *testing.T) {
	parts := rawStreamParts(t, serializeEvents(t,
		engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "c1", Name: "list_dir", Signature: "SIG_A"}},
		engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "c2", Name: "read_file", Signature: "SIG_B"}},
		engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"p":"/x"}`}},
		engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"f":"y"}`}},
		engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
	))
	if len(parts) != 2 {
		t.Fatalf("serialized %d parts, want 2 (one per tool call): %v", len(parts), parts)
	}
	// Ends emit in arrival order: End(1) renders call 2 first, End(0) call 1.
	for i, want := range []struct {
		id, name, sig string
		args          map[string]any
	}{
		{id: "c2", name: "read_file", sig: "SIG_B", args: map[string]any{"f": "y"}},
		{id: "c1", name: "list_dir", sig: "SIG_A", args: map[string]any{"p": "/x"}},
	} {
		fc, ok := parts[i]["functionCall"].(map[string]any)
		if !ok {
			t.Fatalf("part %d has no functionCall arm: %v", i, parts[i])
		}
		if fc["id"] != want.id || fc["name"] != want.name {
			t.Errorf("part %d = %v, want id %q name %q — each call must keep its own identity", i, fc, want.id, want.name)
		}
		// Each call keeps its OWN thoughtSignature on the emitted part: the
		// raw-map assertion pins signature ownership per call — a swapped or
		// shared token between concurrent starts is a corruption.
		if parts[i]["thoughtSignature"] != want.sig {
			t.Errorf("part %d thoughtSignature = %v, want %q — each call must keep its own token", i, parts[i]["thoughtSignature"], want.sig)
		}
		args, ok := fc["args"].(map[string]any)
		if !ok || len(args) != len(want.args) {
			t.Errorf("part %d args = %v, want %v — interleaved deltas must land on their own call", i, fc["args"], want.args)
			continue
		}
		for k, v := range want.args {
			if args[k] != v {
				t.Errorf("part %d args[%q] = %v, want %v", i, k, args[k], v)
			}
		}
	}
}

// TestSerializeToolCallStateErrors: index-bound tool-call state rejects
// malformed IR explicitly — a duplicate start, or a delta/end for an index
// that never started — instead of silently collapsing scopes (the pre-fix
// single-state serializer ignored all three).
func TestSerializeToolCallStateErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []engine.StreamEvent
		want   string
	}{
		{
			name: "duplicate start",
			events: []engine.StreamEvent{
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "a", Name: "a"}},
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "b", Name: "b"}},
			},
			want: "gemini: duplicate tool call start at index 0",
		},
		{
			name: "delta for unknown index",
			events: []engine.StreamEvent{
				{ToolCallDelta: &engine.ToolCallDelta{Index: 3, ArgumentsDelta: `{}`}},
			},
			want: "gemini: tool call delta for unknown index 3",
		},
		{
			name: "end for unknown index",
			events: []engine.StreamEvent{
				{ToolCallEnd: &engine.ToolCallEnd{Index: 2}},
			},
			want: "gemini: tool call end for unknown index 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := serializeEventsErr(t, tc.events...)
			if err == nil {
				t.Fatal("malformed tool-call stream did not error")
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// assertEventSequence compares engine event sequences field by field, failing
// on any mismatch.
func assertEventSequence(t *testing.T, got, want []engine.StreamEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		switch {
		case want[i].BlockStart != nil:
			if got[i].BlockStart == nil || *got[i].BlockStart != *want[i].BlockStart {
				t.Errorf("event %d: BlockStart = %+v, want %+v", i, got[i].BlockStart, want[i].BlockStart)
			}
		case want[i].BlockStop != nil:
			if got[i].BlockStop == nil || *got[i].BlockStop != *want[i].BlockStop {
				t.Errorf("event %d: BlockStop = %+v, want %+v", i, got[i].BlockStop, want[i].BlockStop)
			}
		case want[i].TextDelta != nil:
			if got[i].TextDelta == nil || *got[i].TextDelta != *want[i].TextDelta {
				t.Errorf("event %d: TextDelta = %v, want %v", i, got[i].TextDelta, want[i].TextDelta)
			}
		case want[i].ThinkingDelta != nil:
			if got[i].ThinkingDelta == nil || *got[i].ThinkingDelta != *want[i].ThinkingDelta {
				t.Errorf("event %d: ThinkingDelta = %v, want %v", i, got[i].ThinkingDelta, want[i].ThinkingDelta)
			}
		case want[i].SignatureDelta != nil:
			if got[i].SignatureDelta == nil || *got[i].SignatureDelta != *want[i].SignatureDelta {
				t.Errorf("event %d: SignatureDelta = %v, want %v", i, got[i].SignatureDelta, want[i].SignatureDelta)
			}
		case want[i].ToolCallStart != nil:
			if got[i].ToolCallStart == nil || *got[i].ToolCallStart != *want[i].ToolCallStart {
				t.Errorf("event %d: ToolCallStart = %+v, want %+v", i, got[i].ToolCallStart, want[i].ToolCallStart)
			}
		case want[i].ToolCallDelta != nil:
			if got[i].ToolCallDelta == nil || *got[i].ToolCallDelta != *want[i].ToolCallDelta {
				t.Errorf("event %d: ToolCallDelta = %+v, want %+v", i, got[i].ToolCallDelta, want[i].ToolCallDelta)
			}
		case want[i].ToolCallEnd != nil:
			if got[i].ToolCallEnd == nil || *got[i].ToolCallEnd != *want[i].ToolCallEnd {
				t.Errorf("event %d: ToolCallEnd = %+v, want %+v", i, got[i].ToolCallEnd, want[i].ToolCallEnd)
			}
		}
	}
}

func strPtr(s string) *string { return &s }
