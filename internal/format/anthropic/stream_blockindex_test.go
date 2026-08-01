package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestSerializeToolBlockIndexes: when text precedes tool calls, the
// content_block_start/delta/stop of each tool call must share one block
// index, distinct from the text block and from other tool calls.
// Regression: start used a local counter while delta/stop used the event
// index, desyncing whenever text/thinking blocks preceded tool calls.
func TestSerializeToolBlockIndexes(t *testing.T) {
	text := "let me look"
	events := make(chan engine.StreamEvent, 16)
	events <- engine.StreamEvent{TextDelta: &text}
	events <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "t1", Name: "read"}}
	events <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"a":1}`}}
	events <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}}
	events <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "t2", Name: "write"}}
	events <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"b":2}`}}
	events <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 1}}
	close(events)

	var buf bytes.Buffer
	if err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, events); err != nil {
		t.Fatalf("SerializeStream: %v", err)
	}

	// Collect per-tool-block event indexes from the wire.
	type wireEvent struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content_block"`
		Delta *struct {
			Type string `json:"type"`
		} `json:"delta"`
	}

	toolStarts := map[string]int{} // tool id → block index
	blockEvents := map[int][]string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev wireEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_start" && ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use":
			toolStarts[ev.ContentBlock.ID] = ev.Index
			blockEvents[ev.Index] = append(blockEvents[ev.Index], "start")
		case ev.Type == "content_block_delta" && ev.Delta != nil && ev.Delta.Type == "input_json_delta":
			blockEvents[ev.Index] = append(blockEvents[ev.Index], "delta")
		case ev.Type == "content_block_stop":
			blockEvents[ev.Index] = append(blockEvents[ev.Index], "stop")
		}
	}

	if len(toolStarts) != 2 {
		t.Fatalf("expected 2 tool_use starts, got %v", toolStarts)
	}
	if toolStarts["t1"] == toolStarts["t2"] {
		t.Fatalf("tool calls share block index %d — must be distinct", toolStarts["t1"])
	}
	for id, idx := range toolStarts {
		got := fmt.Sprintf("%v", blockEvents[idx])
		// The text block's stop may share an index bucket check; require
		// the tool block to contain start→delta→stop in order.
		if !strings.Contains(got, "start delta stop") {
			t.Errorf("tool %s (block %d): events %v — want start, delta, stop on the same index", id, idx, blockEvents[idx])
		}
	}
}

// TestSerializeExplicitBlockEvents: plugin-emitted text/thinking block events
// render faithfully as per-kind content_block_start/delta/stop frames with
// consistent indexes — the pre-fix serializer silently dropped BlockStart/
// BlockStop (a plugin-emitted block would vanish), and casting a thinking
// block to a text frame would corrupt the topology the host verified.
func TestSerializeExplicitBlockEvents(t *testing.T) {
	text := "hi"
	thinking := "reason"
	events := make(chan engine.StreamEvent, 8)
	events <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}}
	events <- engine.StreamEvent{TextDelta: &text}
	events <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}}
	events <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 1, Kind: engine.BlockKindThinking}}
	events <- engine.StreamEvent{ThinkingDelta: &thinking}
	events <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 1}}
	events <- engine.StreamEvent{FinishReason: "stop"}
	close(events)

	var buf bytes.Buffer
	if err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, events); err != nil {
		t.Fatalf("SerializeStream: %v", err)
	}

	type frame struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content_block"`
	}
	var starts, stops []int
	var types []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
			t.Fatalf("bad frame: %v (%q)", err, line)
		}
		if f.ContentBlock != nil {
			starts = append(starts, f.Index)
			types = append(types, f.ContentBlock.Type)
		}
		if f.Type == "content_block_stop" {
			stops = append(stops, f.Index)
		}
	}
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 1 {
		t.Fatalf("block start indexes = %v, want [0 1]", starts)
	}
	if len(stops) != 2 || stops[0] != 0 || stops[1] != 1 {
		t.Fatalf("block stop indexes = %v, want [0 1]", stops)
	}
	if len(types) != 2 || types[0] != "text" || types[1] != "thinking" {
		t.Fatalf("block types = %v, want [text thinking]", types)
	}
}

// TestSerializeProviderBlockErrors: provider blocks have no Anthropic wire
// representation — the serializer must error explicitly rather than drop or
// cast them.
func TestSerializeProviderBlockErrors(t *testing.T) {
	events := make(chan engine.StreamEvent, 1)
	events <- engine.StreamEvent{BlockStart: &engine.BlockStart{
		Index: 0, Kind: engine.BlockKindProvider, ProviderKind: "redacted",
	}}
	close(events)

	var buf bytes.Buffer
	err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, events)
	if err == nil {
		t.Fatal("provider block did not error")
	}
	want := `anthropic: provider block kind "redacted" is not supported by this serializer`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
