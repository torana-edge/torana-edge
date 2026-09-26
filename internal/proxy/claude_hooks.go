package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/metrics"
)

const claudeHooksPath = "/_torana/hooks/claude-code/"

func inferHarnessSwitch(source string, claudeAdapter bool) bool {
	return source != "claude-code-session" || !claudeAdapter
}

// No callback can authorize a Torana mutation, block stopping, inject model
// context or force a switch. The optional pre-switch warning only informs.
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
	case "pre-model-switch":
		if !cfg.Suggestions.ClaudeCode.PreModelSwitch {
			http.NotFound(w, r)
			return
		}
		event = "PreModelSwitch"
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
		Session             string   `json:"session_id"`
		Event               string   `json:"hook_event_name"`
		Model               string   `json:"to_model"`
		Source              string   `json:"source"`
		ContextTokens       int64    `json:"context_tokens"`
		CacheWarm           bool     `json:"prompt_cache_warm"`
		EstimatedCacheWrite *float64 `json:"estimated_cache_write_usd"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	var trailing any
	if decoder.Decode(&input) != nil || decoder.Decode(&trailing) != io.EOF || input.Event != event || strings.TrimSpace(input.Session) == "" || len(input.Session) > 1024 {
		http.Error(w, "invalid hook event", http.StatusBadRequest)
		return
	}
	if event == "PreModelSwitch" {
		if input.Model == "" || len(input.Model) > 256 || input.ContextTokens < 0 || input.ContextTokens > 1<<53 || input.EstimatedCacheWrite != nil && (math.IsNaN(*input.EstimatedCacheWrite) || math.IsInf(*input.EstimatedCacheWrite, 0) || *input.EstimatedCacheWrite < 0) {
			http.Error(w, "invalid switch estimate", http.StatusBadRequest)
			return
		}
		switch input.Source {
		case "command", "picker", "sdk":
		default:
			http.Error(w, "invalid switch source", http.StatusBadRequest)
			return
		}
		// After authentication, no suggestion storage, transcript, model call or
		// provider pricing lookup is needed. Claude supplies its own estimate.
		if !input.CacheWarm || input.ContextTokens == 0 {
			writeAgentJSON(w, http.StatusOK, map[string]any{})
			return
		}
		message := fmt.Sprintf("Torana: switching models may rebuild the warm prompt cache for about %d context tokens.", input.ContextTokens)
		if input.EstimatedCacheWrite != nil {
			message += fmt.Sprintf(" Claude estimates a cache-write cost of $%.4f; actual cost may differ.", *input.EstimatedCacheWrite)
		}
		// Do not return permissionDecision: allow would bypass Claude's normal
		// confirmation, while ask refuses switches on noninteractive surfaces.
		writeAgentJSON(w, http.StatusOK, map[string]string{"systemMessage": message})
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
		items, err := s.suggestions.ObserveAdapterSwitch(conversation, input.Model, input.Source, turn)
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
	item, err := s.suggestions.ClaimHookAnnouncement(conversation, turn)
	if err != nil {
		http.Error(w, "hook state unavailable", http.StatusServiceUnavailable)
		return
	}
	if item.ID != "" {
		message := "Torana has a suggestion for this conversation. Review it in Torana's UI or CLI."
		if item.Kind == "model_switch" {
			message += " You can also use /model to switch models in Claude Code."
		}
		writeAgentJSON(w, http.StatusOK, map[string]string{"systemMessage": message})
		return
	}
	writeAgentJSON(w, http.StatusOK, map[string]any{})
}
