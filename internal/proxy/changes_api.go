package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const changesAPIPath = "/_torana/api/v1/agent/changes"

// This operator endpoint is behind the same local-request/Host/Origin guards
// as settings mutations. It is not exposed as a model confirmation shortcut.
func (s *Server) handleOperatorChanges(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, changesAPIPath)
	if path == "" || path == "/" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "change history supports GET")
			return
		}
		conversation := r.URL.Query().Get("conversation_id")
		if !validSuggestionConversation(conversation) {
			writeAgentError(w, http.StatusBadRequest, "invalid_request", "conversation_id is required")
			return
		}
		items, err := s.suggestions.ListChanges(conversation)
		if err != nil {
			writeAgentError(w, http.StatusServiceUnavailable, "state_unavailable", "change history is unavailable")
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]any{"conversation_id": conversation, "changes": items})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "undo" {
		writeAgentError(w, http.StatusNotFound, "not_found", "unknown change operation")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "undo supports POST")
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
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "unexpected data after undo")
		return
	}
	result, err := s.undoConfirmedPluginChange(r.Context(), input.ConversationID, parts[0], nil)
	if err != nil {
		writeAgentError(w, http.StatusServiceUnavailable, "state_unavailable", "Undo could not be completed. Check current configuration and change history before taking further action.")
		return
	}
	writeAgentJSON(w, http.StatusOK, result)
}
