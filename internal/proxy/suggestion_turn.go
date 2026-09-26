package proxy

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// userTurnSignature distinguishes a new human message from retries and
// tool-result continuations. Only a digest is persisted, never prompt text.
func userTurnSignature(chat *engine.ChatRequest) string {
	if chat == nil {
		return ""
	}
	count := 0
	latest := ""
	for _, message := range chat.Messages {
		if message.Role != engine.RoleUser {
			continue
		}
		var text []string
		toolResult := false
		for _, block := range message.Blocks {
			if block.ToolResult != nil {
				toolResult = true
			}
			if block.Text != nil {
				text = append(text, block.Text.Text)
			}
		}
		if toolResult || len(text) == 0 {
			continue
		}
		count++
		latest = strings.Join(text, "\x00")
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", count, latest))))
}
