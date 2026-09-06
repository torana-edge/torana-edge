package format

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestRejectFreeformToolsCoversDefinitionsCallsAndResults(t *testing.T) {
	input := "x"
	rows := []struct {
		name string
		req  *engine.ChatRequest
		want string
	}{
		{"definition", &engine.ChatRequest{Tools: []engine.ToolDef{{Name: "exec", InvocationKind: engine.ToolInvocationFreeform}}}, "definition"},
		{"call", &engine.ChatRequest{Messages: []engine.Message{{Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{InputText: &input, InvocationKind: engine.ToolInvocationFreeform}}}}}}, "call"},
		{"result", &engine.ChatRequest{Messages: []engine.Message{{Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{InvocationKind: engine.ToolInvocationFreeform}}}}}}, "result"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := RejectFreeformTools(row.req, "provider")
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("error = %v, want %q", err, row.want)
			}
		})
	}
	if err := RejectFreeformTools(&engine.ChatRequest{}, "provider"); err != nil {
		t.Fatalf("ordinary request rejected: %v", err)
	}
}

func TestRejectFreeformStreamEventCoversStartAndPresenceSensitiveDelta(t *testing.T) {
	empty := ""
	for _, event := range []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{InvocationKind: engine.ToolInvocationFreeform}},
		{ToolCallDelta: &engine.ToolCallDelta{InputTextDelta: &empty}},
	} {
		if err := RejectFreeformStreamEvent(event, "provider"); err == nil {
			t.Fatalf("unsupported free-form event accepted: %+v", event)
		}
	}
	if err := RejectFreeformStreamEvent(engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{ArgumentsDelta: `{}`}}, "provider"); err != nil {
		t.Fatalf("function delta rejected: %v", err)
	}
}
