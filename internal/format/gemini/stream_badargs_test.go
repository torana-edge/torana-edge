package gemini

import (
	"bytes"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestSerializeBadArgsSurfacesError: unparseable accumulated tool args must
// produce an error, not a silent functionCall with empty args.
func TestSerializeBadArgsSurfacesError(t *testing.T) {
	events := make(chan engine.StreamEvent, 8)
	events <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "t", Name: "write"}}
	events <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"a":1}{"dup":true}`}}
	events <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}}
	close(events)

	var buf bytes.Buffer
	err := (&StreamAdapter{}).SerializeStream(&buf, events)
	if err == nil {
		t.Fatalf("expected error for invalid args, got nil; wire: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"args":{}`) {
		t.Fatalf("silent empty args emitted: %s", buf.String())
	}
}
