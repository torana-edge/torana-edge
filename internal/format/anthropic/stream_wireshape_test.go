package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestSerializeStreamWireShape pins the SSE envelope Anthropic SDK clients
// require. Regression: the serializer emitted bare `data:` frames with no
// message_start — Claude Code treated every stream as malformed, aborted it,
// and silently retried the request NON-streaming; the retried JSON response
// then bypassed the plugin pipeline entirely (compressed body), leaking
// plugin-injected tool-call fields to the harness.
func TestSerializeStreamWireShape(t *testing.T) {
	text := "hi"
	events := make(chan engine.StreamEvent, 8)
	events <- engine.StreamEvent{TextDelta: &text}
	events <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "t1", Name: "read"}}
	events <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"a":1}`}}
	events <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}}
	events <- engine.StreamEvent{Usage: &engine.StreamUsage{InputTokens: 7, OutputTokens: 3}}
	events <- engine.StreamEvent{FinishReason: "tool_calls"}
	close(events)

	var buf bytes.Buffer
	if err := (&StreamAdapter{}).SerializeStream(&buf, events); err != nil {
		t.Fatalf("SerializeStream: %v", err)
	}
	out := buf.String()

	// Every frame is an `event:` line followed by a `data:` line whose type
	// matches — strict SDK decoders dispatch on the event line.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var frames []struct {
		event string
		data  map[string]any
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") {
			t.Fatalf("frame does not open with an event line: %q", line)
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "data: ") {
			t.Fatalf("event line %q not followed by a data line", line)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[i+1], "data: ")), &data); err != nil {
			t.Fatalf("invalid data JSON after %q: %v", line, err)
		}
		ev := strings.TrimPrefix(line, "event: ")
		if data["type"] != ev {
			t.Fatalf("event line %q disagrees with data type %q", ev, data["type"])
		}
		frames = append(frames, struct {
			event string
			data  map[string]any
		}{ev, data})
		i++
	}

	// The stream must open with message_start (the SDK message accumulator
	// is seeded by it), then ping, and close with message_stop.
	if len(frames) < 3 || frames[0].event != "message_start" || frames[1].event != "ping" {
		t.Fatalf("stream must open with message_start, ping — got %v", frameEvents(frames))
	}
	if frames[len(frames)-1].event != "message_stop" {
		t.Fatalf("stream must close with message_stop — got %v", frameEvents(frames))
	}
	msg, _ := frames[0].data["message"].(map[string]any)
	if msg == nil || msg["role"] != "assistant" || msg["id"] == "" || msg["usage"] == nil {
		t.Fatalf("message_start missing envelope fields: %v", frames[0].data)
	}

	// tool_use content_block_start carries an input object (SDK accumulators
	// initialize partial-JSON assembly from it).
	foundTool := false
	for _, f := range frames {
		cb, _ := f.data["content_block"].(map[string]any)
		if f.event == "content_block_start" && cb != nil && cb["type"] == "tool_use" {
			foundTool = true
			if _, ok := cb["input"]; !ok {
				t.Fatalf("tool_use content_block_start missing input: %v", f.data)
			}
		}
	}
	if !foundTool {
		t.Fatal("no tool_use content_block_start emitted")
	}
}

func frameEvents(frames []struct {
	event string
	data  map[string]any
}) []string {
	var out []string
	for _, f := range frames {
		out = append(out, f.event)
	}
	return out
}
