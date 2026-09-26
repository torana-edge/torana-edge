package proxy

import (
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// latestUserHasOtherContent is conservative: a directive-only local reply is
// allowed only when the final human item contains no other text or blocks.
// Unknown/multimodal content is never silently discarded.
func latestUserHasOtherContent(chat *engine.ChatRequest) bool {
	if chat == nil || len(chat.Messages) == 0 {
		return true
	}
	last := chat.Messages[len(chat.Messages)-1]
	if last.Role != engine.RoleUser {
		return true
	}
	for _, block := range last.Blocks {
		if block.Text == nil || strings.TrimSpace(block.Text.Text) != "" {
			return true
		}
	}
	return false
}
