package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/metrics"
)

const claudeHooksPath = "/_torana/hooks/claude-code/"

// Only Stop and PostModelSwitch are enabled here. No callback can authorize
// a Torana mutation, block stopping, inject model context or force a switch.
func (s *Server) handleClaudeHook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cfg := s.GetConfig().Providers
	if !cfg.MCP.Enabled || !cfg.Suggestions.ClaudeCode.Enabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	token, err := s.mcpTokens.Current()
	headers := r.Header.Values("Authorization")
	if err != nil || token == "" || len(headers) != 1 || subtle.ConstantTimeCompare([]byte(headers[0]), []byte("Bearer "+token)) != 1 {
		http.Error(w, "invalid hook token", http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, claudeHooksPath)
	event := ""
	switch path {
	case "stop":
		event = "Stop"
	case "post-model-switch":
		event = "PostModelSwitch"
	default:
		http.NotFound(w, r)
		return
	}
	release, allowed := s.mcpLimits.acquireLease("hook:claude-code:" + path)
	if !allowed {
		http.Error(w, "retry later", http.StatusTooManyRequests)
		return
	}
	defer release()
	var input struct {
		Session string `json:"session_id"`
		Event   string `json:"hook_event_name"`
		Model   string `json:"to_model"`
		Source  string `json:"source"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	var trailing any
	if decoder.Decode(&input) != nil || decoder.Decode(&trailing) != io.EOF || input.Event != event || strings.TrimSpace(input.Session) == "" || len(input.Session) > 1024 {
		http.Error(w, "invalid hook event", http.StatusBadRequest)
		return
	}
	conversation := engine.ExternalConversationID("claude-code-session", strings.TrimSpace(input.Session))
	turn, err := s.suggestions.ObserveUserTurn(conversation, "")
	if err != nil {
		http.Error(w, "hook state unavailable", http.StatusServiceUnavailable)
		return
	}
	if event == "PostModelSwitch" {
		if input.Model == "" || len(input.Model) > 256 {
			http.Error(w, "invalid target model", http.StatusBadRequest)
			return
		}
		switch input.Source {
		case "command", "picker", "sdk", "auto", "resume":
		default:
			http.Error(w, "invalid switch source", http.StatusBadRequest)
			return
		}
		items, err := s.suggestions.AcceptHarnessSwitchVia(conversation, input.Model, "adapter", turn)
		if err != nil {
			http.Error(w, "hook state unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, item := range items {
			metrics.RecordSuggestion(r.Context(), item.Kind, item.Status, item.Via)
		}
		writeAgentJSON(w, http.StatusOK, map[string]any{})
		return
	}
	items, err := s.suggestions.List(conversation, "", turn)
	if err != nil {
		http.Error(w, "hook state unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, item := range items {
		if item.Status == "pending" {
			message := "Torana has a suggestion for this conversation. Review it in Torana's UI or CLI."
			if item.Kind == "model_switch" {
				message += " You can also use /model to switch models in Claude Code."
			}
			writeAgentJSON(w, http.StatusOK, map[string]string{"systemMessage": message})
			return
		}
	}
	writeAgentJSON(w, http.StatusOK, map[string]any{})
}
