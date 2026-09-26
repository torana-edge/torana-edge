package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// Undo uses a persistent keyed revision, unlike an operator's process-local
// ETag. Restart must not invalidate an already-applied change's undo guard.
func (s *Server) operationRevision(cfg provider.Config) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	mac, err := s.secrets.MAC("operation-config-revision", string(raw))
	return hex.EncodeToString(mac), err
}

func candidatePluginConfiguration(current provider.PluginsConfig, name, operation string, input json.RawMessage) (provider.PluginsConfig, error) {
	candidate := current
	candidate.Order = append([]string(nil), current.Order...)
	candidate.Config = make(map[string]json.RawMessage, len(current.Config))
	for key, value := range current.Config {
		candidate.Config[key] = append(json.RawMessage(nil), value...)
	}
	switch operation {
	case "_config.set":
		candidate.Config[name] = append(json.RawMessage(nil), input...)
	case "_enable":
		for _, existing := range candidate.Order {
			if existing == name {
				return candidate, nil
			}
		}
		candidate.Order = append(candidate.Order, name)
	case "_disable":
		kept := candidate.Order[:0]
		for _, existing := range candidate.Order {
			if existing != name {
				kept = append(kept, existing)
			}
		}
		candidate.Order = kept
	default:
		return provider.PluginsConfig{}, errors.New("not a standard plugin mutation")
	}
	return candidate, nil
}

// applyPluginConfigurationLocked validates the complete candidate pipeline
// before persistence or publication. The caller holds controlPlaneMutationMu.
func (s *Server) applyPluginConfigurationLocked(ctx context.Context, candidate provider.Config, target ...string) error {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	credentials, err := s.prepareCredentialRegistry(candidate.Credentials)
	if err != nil {
		return err
	}
	runtime := s.newRuntime()
	pipeline, err := plugin.NewPipeline(runtime, s.pipelinePluginConfig(candidate.Plugins))
	if err != nil {
		runtime.Close()
		return err
	}
	var previous []plugin.SkippedPlugin
	if raw := s.pluginPipeline.Load(); raw != nil {
		previous = raw.(*plugin.PluginPipeline).Skipped()
	}
	name := ""
	if len(target) > 0 {
		name = target[0]
	}
	if introducesSkippedPlugins(previous, pipeline.Skipped(), name) {
		pipeline.DrainAndClose()
		return errors.New("candidate plugin pipeline contains unavailable plugins")
	}
	if err := ctx.Err(); err != nil {
		pipeline.DrainAndClose()
		return err
	}
	if err := s.persistProviders(candidate); err != nil {
		pipeline.DrainAndClose()
		return err
	}
	old := s.pluginPipeline.Swap(pipeline)
	s.applyProviders(candidate, credentials)
	if old != nil {
		s.retireAsyncLocked(old.(*plugin.PluginPipeline).DrainAndClose)
	}
	return nil
}

// applyConfirmedStandardOperation is a host-only execution seam. The caller
// has already resolved explicit user acceptance. Policy is reconstructed from
// the latest snapshot and operator settings, not a caller-held stale registry.
// Guest mutations and undo mounting follow in separate wiring changes.
func (s *Server) applyConfirmedStandardOperation(ctx context.Context, conversation, id string, protected []string) (mcpserver.Result, error) {
	if err := ctx.Err(); err != nil {
		return mcpserver.Result{}, err
	}
	if s.suggestions == nil || s.secrets == nil {
		return operationError("not_configured", "User confirmation is unavailable."), nil
	}
	s.controlPlaneMutationMu.Lock()
	defer s.controlPlaneMutationMu.Unlock()
	sealed, err := s.suggestions.AcceptedOperationIntent(conversation, id)
	if err != nil {
		return operationError("not_found", "Accepted operation is unavailable."), nil
	}
	plaintext, err := s.secrets.Decrypt(sealed)
	if err != nil {
		return mcpserver.Result{}, err
	}
	var intent sealedOperationIntent
	if err := json.Unmarshal([]byte(plaintext), &intent); err != nil {
		return mcpserver.Result{}, err
	}
	if intent.Binding.ConversationID != conversation || !intent.Binding.Bound {
		return operationError("unbound_conversation", "This operation belongs to another conversation."), nil
	}
	current := s.GetConfig().Providers
	if current.MCP.Consent == "operator_only" {
		items, lookupErr := s.suggestions.List(conversation, "torana", 0)
		if lookupErr != nil {
			return mcpserver.Result{}, lookupErr
		}
		for _, proposal := range items {
			if proposal.ID == id && proposal.Via == "mcp_elicitation" {
				return operationError("access_denied", "Confirm this operation in Torana's UI or CLI."), nil
			}
		}
	}
	// Operator acceptance must enforce the current protected floor, even when
	// a caller did not supply a policy snapshot. A stale override cannot grant
	// access removed since the proposal was created.
	protected = append(append([]string(nil), protected...), current.Plugins.ProtectedNamespaces()...)
	overrides := current.MCP.Access
	if intent.Revision != s.configRevision(current) {
		return operationError("conflict", "Configuration changed; request and review a fresh operation."), nil
	}
	registry, err := s.currentNamespaceRegistryLocked()
	if err != nil {
		return mcpserver.Result{}, err
	}
	policy, err := newNamespaceAccessPolicy(registry, protected, overrides)
	if err != nil {
		return mcpserver.Result{}, err
	}
	entry, operation, exists := policy.lookup(intent.Namespace, intent.Operation)
	if !exists || entry.Digest != intent.Digest {
		return operationError("stale_digest", "The plugin changed; request and review a fresh operation."), nil
	}
	allowed := policy.ModelReachable(entry.Name, operation.ID) == "confirm"
	if !allowed {
		return operationError("access_denied", "This operation is no longer available for confirmation."), nil
	}
	if operation.Source != "standard" {
		return operationError("confirmation_unavailable", "Confirming plugin-defined operations is unavailable here; use the plugin's CLI guide for this operation."), nil
	}
	if operation.ID == "_config.set" && (len(entry.ConfigSchema) == 0 || plugin.ValidateConfigAgainstSchema(&plugin.ConfigSchema{Raw: entry.ConfigSchema}, intent.Input) != nil) {
		return operationError("invalid_input", "Configuration does not match the plugin schema."), nil
	}
	plugins, err := candidatePluginConfiguration(current.Plugins, entry.Name, operation.ID, intent.Input)
	if err != nil {
		return operationError("unknown_operation", "This mutation has no execution handler."), nil
	}
	candidate := current
	candidate.Plugins = plugins
	postRevision, err := s.operationRevision(candidate)
	if err != nil {
		return mcpserver.Result{}, err
	}
	undo, err := json.Marshal(pluginUndoSnapshot{Namespace: entry.Name, Digest: entry.Digest, Plugins: current.Plugins})
	if err != nil {
		return mcpserver.Result{}, err
	}
	sealedUndo, err := s.secrets.Encrypt(string(undo))
	if err != nil {
		return mcpserver.Result{}, err
	}
	if _, err := s.suggestions.PrepareOperationChange(conversation, id, sealedUndo, postRevision); err != nil {
		return mcpserver.Result{}, err
	}
	if err := s.applyPluginConfigurationLocked(ctx, candidate, entry.Name); err != nil {
		if finishErr := s.suggestions.FinishOperation(conversation, id, "failed"); finishErr != nil {
			return mcpserver.Result{}, finishErr
		}
		return operationError("plugin_failed", "The change could not be applied; configuration is unchanged."), nil
	}
	if err := s.suggestions.FinishOperation(conversation, id, "applied"); err != nil {
		return appliedHistoryIncomplete(entry.Name, operation.ID), nil
	}
	return mcpserver.Result{OK: true, Status: "applied", Namespace: entry.Name, Operation: operation.ID, Summary: "The confirmed change was applied."}, nil
}

func introducesSkippedPlugins(previous, candidate []plugin.SkippedPlugin, target string) bool {
	type identity struct{ name, digest string }
	known := make(map[identity]bool, len(previous))
	for _, item := range previous {
		known[identity{item.Name, item.Digest}] = true
	}
	for _, item := range candidate {
		if item.Name == target || !known[identity{item.Name, item.Digest}] {
			return true
		}
	}
	return false
}

func appliedHistoryIncomplete(namespace, operation string) mcpserver.Result {
	return mcpserver.Result{OK: true, Status: "applied_history_incomplete", Namespace: namespace, Operation: operation, Summary: "The change was applied, but its history update failed. Do not retry the operation; check Torana's current configuration and change history."}
}
