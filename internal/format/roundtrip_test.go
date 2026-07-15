package format_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"

	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	_ "github.com/torana-edge/torana-edge/internal/format/vertex"
)

// TestSerializeSingleCompleteDelta validates the invariant the stream plugin
// pipeline relies on: buffering plugins suppress argument fragments and emit
// ONE complete ToolCallDelta before ToolCallEnd, and every provider format
// serializes that into a stream whose reparse yields exactly the complete
// argument JSON — no duplication, no data loss.
func TestSerializeSingleCompleteDelta(t *testing.T) {
	fullArgs := `{"env":{"A":"1"},"path":"main.go"}`
	wantArgs := map[string]any{
		"env":  map[string]any{"A": "1"},
		"path": "main.go",
	}

	for _, name := range []string{"openai", "anthropic", "bedrock", "vertex"} {
		t.Run(name, func(t *testing.T) {
			f := format.Lookup(name)
			if f == nil {
				t.Fatalf("format %s not registered", name)
			}

			events := make(chan engine.StreamEvent, 8)
			events <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_rt", Name: "write"}}
			events <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: fullArgs}}
			events <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}}
			events <- engine.StreamEvent{FinishReason: "tool_calls"}
			close(events)

			var buf bytes.Buffer
			if err := f.Stream.SerializeStream(&buf, events); err != nil {
				t.Fatalf("SerializeStream: %v", err)
			}

			// Reparse the serialized stream and reassemble tool call args
			// the way a client would.
			var assembled strings.Builder
			deltas := 0
			for ev := range f.Stream.ParseStream(bytes.NewReader(buf.Bytes())) {
				if ev.ToolCallDelta != nil {
					deltas++
					assembled.WriteString(ev.ToolCallDelta.ArgumentsDelta)
				}
				if ev.Error != nil {
					t.Fatalf("stream error event: %+v", ev.Error)
				}
			}
			if deltas == 0 {
				t.Fatalf("no ToolCallDelta events after reparse; wire:\n%s", buf.String())
			}

			var got map[string]any
			if err := json.Unmarshal([]byte(assembled.String()), &got); err != nil {
				t.Fatalf("assembled args not valid JSON: %v (%q)", err, assembled.String())
			}
			if !reflect.DeepEqual(got, wantArgs) {
				t.Fatalf("args mismatch: got %v want %v", got, wantArgs)
			}
		})
	}
}
