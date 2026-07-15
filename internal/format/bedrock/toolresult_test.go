package bedrock

import (
	"encoding/json"
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

	var toolMsg *engine.Message
	for i := range chat.Messages {
		if chat.Messages[i].Role == engine.RoleTool {
			toolMsg = &chat.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message found in %+v", chat.Messages)
	}

	if !strings.Contains(toolMsg.Content, "first chunk") || !strings.Contains(toolMsg.Content, "second chunk") {
		t.Errorf("text blocks lost: Content=%q", toolMsg.Content)
	}
	if len(toolMsg.ContentParts) != 1 {
		t.Fatalf("expected 1 non-text part preserved, got %d: %v", len(toolMsg.ContentParts), toolMsg.ContentParts)
	}
	b, _ := json.Marshal(toolMsg.ContentParts[0])
	if !strings.Contains(string(b), "42") {
		t.Errorf("json block lost: %s", b)
	}
}
