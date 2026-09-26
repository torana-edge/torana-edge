package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"time"
)

// AEAD-protected protocol state is echoed by MCP, not typed into chat.
type mcpConsentState struct {
	Kind         string `json:"kind"`
	Tool         string `json:"tool"`
	Hash         string `json:"hash"`
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Expires      int64  `json:"expires"`
}

func consentArgumentHash(raw json.RawMessage) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (s *Server) sealMCPConsent(ctx context.Context, name string, raw json.RawMessage, consent *mcpserver.Consent) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.GetConfig().Providers.MCP.Consent == "operator_only" {
		return "", fmt.Errorf("confirm this operation in Torana's UI or CLI")
	}
	state := mcpConsentState{Kind: "mcp-consent-v1", Tool: name, Hash: consentArgumentHash(raw), ID: consent.ID, Conversation: consent.Conversation, Expires: time.Now().Add(operationConsentTTL).Unix()}
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return s.secrets.Encrypt(string(encoded))
}

func (s *Server) resolveMCPConsent(ctx context.Context, name string, raw json.RawMessage, sealed, action string) (result mcpserver.Result, err error) {
	start := time.Now()
	bound := false
	defer func() { s.appendMCPAudit(name, bound, result, err, start) }()
	if err := ctx.Err(); err != nil {
		return mcpserver.Result{}, err
	}
	invalid := func() (mcpserver.Result, error) {
		return operationError("invalid_confirmation", "Confirmation expired or changed; review the pending change in Torana."), nil
	}
	if s.secrets == nil || s.suggestions == nil || !s.GetConfig().Providers.MCP.Enabled || s.GetConfig().Providers.MCP.Consent == "operator_only" {
		return invalid()
	}
	s.mcpMu.Lock()
	stopping := s.mcpStopping
	s.mcpMu.Unlock()
	if stopping {
		return operationError("not_configured", "Torana MCP is stopping."), nil
	}
	switch name {
	case "torana_invoke":
	default:
		return invalid()
	}
	releaseTool, allowed := s.mcpLimits.acquireLease("tool:" + name)
	if !allowed {
		return mcpRateLimited(), nil
	}
	defer releaseTool()
	plaintext, err := s.secrets.Decrypt(sealed)
	if err != nil {
		return invalid()
	}
	var state mcpConsentState
	if json.Unmarshal([]byte(plaintext), &state) != nil || state.Kind != "mcp-consent-v1" || state.Tool != name || state.Hash != consentArgumentHash(raw) || state.ID == "" || state.Conversation == "" || time.Now().Unix() >= state.Expires {
		return invalid()
	}
	bound = true
	releaseConversation, allowed := s.mcpLimits.acquireLease("conversation:" + state.Conversation)
	if !allowed {
		return mcpRateLimited(), nil
	}
	defer releaseConversation()
	if action == "cancel" {
		return mcpserver.Result{OK: true, Status: "pending_confirmation", Summary: "No change was applied. Review the pending change in Torana."}, nil
	}
	status := "accepted"
	if action == "decline" {
		status = "dismissed"
	} else if action != "accept" {
		return invalid()
	}
	item, err := s.suggestions.ResolveID(state.Conversation, state.ID, status, "mcp_elicitation", 0)
	if err != nil {
		return invalid()
	}
	metrics.RecordSuggestion(ctx, item.Kind, item.Status, item.Via)
	if status == "dismissed" {
		return mcpserver.Result{OK: true, Status: "dismissed", Summary: "The change was declined."}, nil
	}
	return s.applyConfirmedStandardOperation(ctx, state.Conversation, state.ID, nil)
}
