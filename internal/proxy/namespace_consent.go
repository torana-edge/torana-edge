package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

const operationConsentTTL = 10 * time.Minute

// This record is encrypted before persistence. Before/after values belong only
// in the operator's confirmation view, never a model response or plugin header.
type sealedOperationIntent struct {
	Namespace string            `json:"namespace"`
	Operation string            `json:"operation"`
	Digest    string            `json:"digest"`
	Revision  string            `json:"revision"`
	Input     json.RawMessage   `json:"input,omitempty"`
	Before    json.RawMessage   `json:"before,omitempty"`
	After     json.RawMessage   `json:"after,omitempty"`
	Binding   plugin.MCPBinding `json:"binding"`
}

func (s *Server) proposeNamespaceOperation(ctx context.Context, call operationCall) (mcpserver.Result, error) {
	if err := ctx.Err(); err != nil {
		return mcpserver.Result{}, err
	}
	if !call.Binding.Bound {
		result := operationError("unbound_conversation", "Torana needs to observe this call before requesting confirmation.")
		result.Error.Retryable = true
		return result, nil
	}
	if s.suggestions == nil || s.secrets == nil {
		return operationError("not_configured", "User confirmation is unavailable."), nil
	}
	s.controlPlaneMutationMu.Lock()
	defer s.controlPlaneMutationMu.Unlock()
	registry, err := s.currentNamespaceRegistryLocked()
	if err != nil {
		return mcpserver.Result{}, err
	}
	entry, exists := registry.resolve(call.Entry.Name, false)
	if !exists || entry.Digest != call.Entry.Digest {
		return operationError("stale_digest", "The plugin changed; describe it and try again."), nil
	}
	var operation *namespaceOperation
	for i := range entry.Operations {
		if entry.Operations[i].ID == call.Operation.ID {
			operation = &entry.Operations[i]
			break
		}
	}
	if operation == nil || !operation.Callable || operation.Risk != "write" {
		return operationError("namespace_unavailable", "This operation is not currently available."), nil
	}
	cfg := s.GetConfig().Providers
	intent := sealedOperationIntent{Namespace: entry.Name, Operation: operation.ID, Digest: entry.Digest, Revision: s.configRevision(cfg), Input: append(json.RawMessage(nil), call.Input...), Binding: call.Binding}
	if operation.Source == "standard" {
		switch operation.ID {
		case "_config.set":
			if len(entry.ConfigSchema) == 0 || plugin.ValidateConfigAgainstSchema(&plugin.ConfigSchema{Raw: append(json.RawMessage(nil), entry.ConfigSchema...)}, call.Input) != nil {
				return operationError("invalid_input", "Configuration does not match the plugin schema."), nil
			}
			intent.Before = append(json.RawMessage(nil), cfg.Plugins.Config[entry.Name]...)
			if len(intent.Before) == 0 {
				intent.Before = json.RawMessage(`{}`)
			}
			intent.After = append(json.RawMessage(nil), call.Input...)
		case "_enable", "_disable":
			enabled := false
			for _, name := range cfg.Plugins.Order {
				if name == entry.Name {
					enabled = true
					break
				}
			}
			intent.Before, _ = json.Marshal(map[string]bool{"enabled": enabled})
			intent.After, _ = json.Marshal(map[string]bool{"enabled": operation.ID == "_enable"})
		default:
			return operationError("unknown_operation", "This operation has no confirmation handler."), nil
		}
	}
	body, err := operationConsentSummary(entry, *operation, intent)
	if err != nil {
		return operationError("invalid_input", "Review this larger change in Torana's UI or CLI."), nil
	}
	plaintext, err := json.Marshal(intent)
	if err != nil {
		return mcpserver.Result{}, err
	}
	mac, err := s.secrets.MAC("operation-consent-intent", string(plaintext))
	if err != nil {
		return mcpserver.Result{}, err
	}
	sealed, err := s.secrets.Encrypt(string(plaintext))
	if err != nil {
		return mcpserver.Result{}, err
	}
	turn, err := s.suggestions.ObserveUserTurn(call.Binding.ConversationID, "")
	if err != nil {
		return mcpserver.Result{}, err
	}
	// The model gets neither the code, internal ID, input, config diff nor the
	// guest-authored title. Those stay in the trusted operator confirmation UI.
	// CallID is intentional consent provenance: a different tool call gets a
	// fresh code even if it proposes the same configuration change.
	_, err = s.suggestions.CreateOperation(call.Binding.ConversationID, turn, suggest.OperationProposal{
		IntentKey: entry.Name + "." + operation.ID, IntentDigest: hex.EncodeToString(mac), Title: "Confirm " + entry.Name + " change", Body: body,
		SealedIntent: sealed, ExpiresAt: time.Now().Add(operationConsentTTL),
	})
	if err != nil {
		return mcpserver.Result{}, err
	}
	return mcpserver.Result{OK: true, Status: "pending_confirmation", Summary: "Review and confirm this operation in Torana.", ExpiresInSeconds: int(operationConsentTTL.Seconds()), Conversation: &mcpserver.ConversationBinding{Binding: "bound"}}, nil
}
