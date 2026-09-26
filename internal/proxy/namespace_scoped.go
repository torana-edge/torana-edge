package proxy

import (
	"time"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

// Scope is exclusively host-observed. Never reuse an operator handler that
// accepts a caller-selected conversation or returns private consent material.
func (s *Server) executeScopedCoreOperation(call operationCall) (any, *mcpserver.DomainError, error) {
	if !call.Binding.Bound || call.Binding.ConversationID == "" {
		return nil, &mcpserver.DomainError{Code: "unbound_conversation", Message: "Retry after Torana observes this tool call.", Retryable: true}, nil
	}
	id := call.Binding.ConversationID
	switch call.Operation.ID {
	case "feed.recent":
		return s.feed.SnapshotForConversation(id), nil, nil
	case "session.usage":
		record, ok := s.conversations.Get(id)
		if !ok {
			return map[string]any{"available": false}, nil, nil
		}
		// This is an in-memory observation window, not a durable billing total.
		// Plugin egress without a verified conversation is intentionally excluded.
		return map[string]any{"available": true, "window": "since_observed", "since": record.FirstSeen, "requests": record.Turns, "tokens_in": record.TokensIn, "tokens_out": record.TokensOut, "cache_read_tokens": record.CacheReadTokens, "cache_write_tokens": record.CacheWriteTokens, "includes_unscoped_plugin_egress": false}, nil, nil
	case "changes.list":
		items, err := s.suggestions.ListChanges(id)
		if items == nil {
			items = []suggest.Change{}
		}
		return items, nil, err
	case "suggestions.list":
		turn, err := s.suggestions.ObserveUserTurn(id, "")
		if err != nil {
			return nil, nil, err
		}
		items, err := s.suggestions.List(id, "", turn)
		if err != nil {
			return nil, nil, err
		}
		// The model can describe pending work but cannot approve it. Bodies,
		// codes, diffs, action payloads and dedupe keys stay operator-only.
		type summary struct {
			ID        string    `json:"id"`
			Kind      string    `json:"kind"`
			Title     string    `json:"title"`
			Status    string    `json:"status"`
			CreatedAt time.Time `json:"created_at"`
		}
		out := make([]summary, 0, len(items))
		for _, item := range items {
			out = append(out, summary{item.ID, catalogText(item.Kind, 60), catalogText(item.Title, 60), item.Status, item.CreatedAt})
		}
		return out, nil, nil
	}
	return nil, &mcpserver.DomainError{Code: "unknown_operation", Message: "This scoped operation is unavailable."}, nil
}
