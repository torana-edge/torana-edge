package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

const suggestionsAPIPath = "/_torana/api/v1/agent/suggestions"

func validSuggestionConversation(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value)
}

func (s *Server) handleAgentSuggestions(w http.ResponseWriter, r *http.Request) {
	if !s.GetConfig().Providers.Suggestions.Enabled {
		writeAgentError(w, http.StatusNotFound, "not_configured", "suggestions are disabled")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, suggestionsAPIPath)
	if path == "" || path == "/" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "suggestions list supports GET")
			return
		}
		conversation := r.URL.Query().Get("conversation_id")
		if !validSuggestionConversation(conversation) {
			writeAgentError(w, http.StatusBadRequest, "invalid_request", "conversation_id is required")
			return
		}
		turn, err := s.suggestions.ObserveUserTurn(conversation, "")
		if err != nil {
			writeAgentError(w, http.StatusServiceUnavailable, "state_unavailable", "suggestion state is unavailable")
			return
		}
		items, err := s.suggestions.List(conversation, "", turn)
		if err != nil {
			writeAgentError(w, http.StatusServiceUnavailable, "state_unavailable", "suggestion state is unavailable")
			return
		}
		if items == nil {
			items = []suggest.Suggestion{}
		}
		writeAgentJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation, "suggestions": items})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || (parts[1] != "accept" && parts[1] != "dismiss") {
		writeAgentError(w, http.StatusNotFound, "not_found", "unknown suggestion operation")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "suggestion actions support POST")
		return
	}
	var input struct {
		ConversationID string `json:"conversation_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !validSuggestionConversation(input.ConversationID) {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "conversation_id is required")
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "unexpected data after suggestion action")
		return
	}
	turn, err := s.suggestions.ObserveUserTurn(input.ConversationID, "")
	if err != nil {
		writeAgentError(w, http.StatusServiceUnavailable, "state_unavailable", "suggestion state is unavailable")
		return
	}
	status := "accepted"
	if parts[1] == "dismiss" {
		status = "dismissed"
	}
	item, err := s.suggestions.ResolveID(input.ConversationID, parts[0], status, "agent_api", turn)
	if errors.Is(err, suggest.ErrNotFound) {
		writeAgentError(w, http.StatusNotFound, "not_found", "pending suggestion not found in this conversation")
		return
	}
	if err != nil {
		writeAgentError(w, http.StatusServiceUnavailable, "state_unavailable", "suggestion state is unavailable")
		return
	}
	metrics.RecordSuggestion(r.Context(), item.Kind, item.Status, item.Via)
	writeAgentJSON(w, http.StatusOK, item)
}
