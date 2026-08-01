package gemini

import (
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

// TestStreamToolCallBlockIndexesSharedAcrossChunks pins the same invariant when
// parts arrive split over multiple SSE frames: the per-stream block counter
// must carry across chunks. It also pins that a trailing signature-only part
// still emits a standalone SignatureDelta (SignatureScopeTrailingStandalone).
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
	want := []int{0, 0, 0, 1, 1, 1} // the counter carries across chunks, text carries no index
	if !slices.Equal(got, want) {
		t.Fatalf("tool event indexes = %v, want %v — the block counter must be shared across chunks", got, want)
	}
	if !sawText {
		t.Error("text part lost")
	}
	if !sawSig {
		t.Error("signature-only final part did not emit a standalone SignatureDelta")
	}
}
