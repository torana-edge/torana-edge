package bedrock

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestToolResultMultiBlock: all text blocks of a tool result are preserved
// in Content (concatenated), and non-text blocks survive via ContentParts.
// Regression for private-nucleus #71/#73 (first-text-only was kept).
func TestToolResultMultiBlock(t *testing.T) {
	body := `{
		"messages": [{
			"role": "user",
			"content": [{
				"toolResult": {
					"toolUseId": "tu_1",
					"content": [
						{"text": "first chunk"},
						{"text": "second chunk"},
						{"json": {"score": 42}}
					]
				}
			}]
		}]
	}`

	var adapter Adapter
	chat, err := adapter.Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Bedrock has no tool role: the result rides its user message at its
	// exact wire position (no synthetic RoleTool split).
	var toolMsg *engine.Message
	for i := range chat.Messages {
		if len(toolResults(chat.Messages[i])) > 0 {
			toolMsg = &chat.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool-result message found in %+v", chat.Messages)
	}

	rs := toolResults(*toolMsg)
	if len(rs) != 1 {
		t.Fatalf("no tool result: %+v", chat.Messages)
	}
	var text string
	var parts []engine.ToolResultContentBlock
	for _, c := range rs[0].Content {
		if c.Text != "" {
			text += c.Text
		} else {
			parts = append(parts, c)
		}
	}
	if !strings.Contains(text, "first chunk") || !strings.Contains(text, "second chunk") {
		t.Errorf("text blocks lost: %q", text)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 non-text part preserved, got %d", len(parts))
	}
	if b := parts[0].Unknown.Payload.Bytes(); !strings.Contains(string(b), "42") {
		t.Errorf("json block lost: %s", b)
	}
}
