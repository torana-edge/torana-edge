package anthropic

import (
	"bytes"
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
	if err := (&StreamAdapter{}).SerializeStream(&buf, nil, events); err != nil {
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
