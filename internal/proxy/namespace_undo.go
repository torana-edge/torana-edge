package proxy

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

type pluginUndoSnapshot struct {
	Namespace string                 `json:"namespace"`
	Digest    string                 `json:"digest"`
	Plugins   provider.PluginsConfig `json:"plugins"`
}

// undoConfirmedPluginChange is for an explicitly user-supplied undo code, not
// model input. Mounting CLI/directive handlers must preserve that distinction.
func (s *Server) undoConfirmedPluginChange(ctx context.Context, conversation, code string, protected []string) (mcpserver.Result, error) {
	if err := ctx.Err(); err != nil {
		return mcpserver.Result{}, err
	}
	if s.suggestions == nil || s.secrets == nil {
		return operationError("not_configured", "Change history is unavailable."), nil
	}
	s.controlPlaneMutationMu.Lock()
	defer s.controlPlaneMutationMu.Unlock()
	current := s.GetConfig().Providers
	revision, err := s.operationRevision(current)
	if err != nil {
		return mcpserver.Result{}, err
	}
	id, sealed, err := s.suggestions.ClaimUndo(conversation, code, revision)
	if errors.Is(err, suggest.ErrConflict) {
		return operationError("conflict", "Configuration changed after this operation; review the current configuration instead of undoing it."), nil
	}
	if errors.Is(err, suggest.ErrNotFound) {
		return operationError("not_found", "No undoable change matches this code in the current conversation."), nil
	}
	if err != nil {
		return mcpserver.Result{}, err
	}
	finish := func(status string, result mcpserver.Result, internal error) (mcpserver.Result, error) {
		if err := s.suggestions.FinishUndo(conversation, id, status); err != nil {
			if status == "undone" {
				return mcpserver.Result{OK: true, Status: "undone_history_incomplete", Summary: "The change was undone, but its history update failed. Do not retry; check Torana's current configuration and change history."}, nil
			}
			return mcpserver.Result{}, err
		}
		return result, internal
	}
	plaintext, err := s.secrets.Decrypt(sealed)
	if err != nil {
		return finish("failed", mcpserver.Result{}, err)
	}
	var snapshot pluginUndoSnapshot
	if err := json.Unmarshal([]byte(plaintext), &snapshot); err != nil {
		return finish("failed", mcpserver.Result{}, err)
	}
	registry, err := s.currentNamespaceRegistryLocked()
	if err != nil {
		return finish("failed", mcpserver.Result{}, err)
	}
	entry, exists := registry.resolve(snapshot.Namespace, false)
	if !exists || entry.Digest != snapshot.Digest {
		return finish("conflict", operationError("stale_digest", "The installed plugin changed; review its current configuration instead of undoing it."), nil)
	}
	policy, err := newNamespaceAccessPolicy(registry, protected, nil)
	if err != nil {
		return finish("failed", mcpserver.Result{}, err)
	}
	if policy.protected[entry.Name] {
		return finish("conflict", operationError("access_denied", "This plugin is now protected; manage it in Torana's UI or CLI."), nil)
	}
	candidate := current
	candidate.Plugins = snapshot.Plugins
	// This flag is a test-only construction option and is never serialized.
	candidate.Plugins.AllowUnapproved = current.Plugins.AllowUnapproved
	if err := s.applyPluginConfigurationLocked(ctx, candidate, entry.Name); err != nil {
		return finish("failed", operationError("plugin_failed", "The previous configuration could not be restored; configuration is unchanged."), nil)
	}
	return finish("undone", mcpserver.Result{OK: true, Status: "undone", Summary: "The confirmed change was undone."}, nil)
}
