package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

type mcpPolicySnapshot struct {
	revision string
	pipeline *plugin.PluginPipeline
	expires  time.Time
	policy   *namespaceAccessPolicy
}

// Always registered so live settings can enable MCP without a restart. No
// request provisions a token; explicit operator setup remains a separate API.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.mcpMu.Lock()
	cfg := s.GetConfig()
	if s.mcpStopping || !cfg.Providers.MCP.Enabled {
		s.mcpMu.Unlock()
		http.NotFound(w, r)
		return
	}
	if s.mcpHandler == nil {
		handler, err := mcpserver.NewHandler(mcpserver.Options{Version: cfg.HostVersion, Token: func() string {
			if !s.GetConfig().Providers.MCP.Enabled {
				return ""
			}
			token, err := s.mcpTokens.Current()
			if err != nil {
				return ""
			}
			return token
		}, Dispatch: s.dispatchMCP})
		if err != nil {
			s.mcpMu.Unlock()
			http.Error(w, "MCP is unavailable", http.StatusServiceUnavailable)
			return
		}
		s.mcpHandler = handler
	}
	handler := s.mcpHandler
	s.mcpMu.Unlock()
	handler.ServeHTTP(w, r)
}

// Cancellation happens asynchronously: settings writes hold mutationMu, and
// an admitted MCP call may need that lock to finish. Shutdown joins retired
// generations before closing shared host resources.
func (s *Server) retireMCPHandler() {
	s.mcpMu.Lock()
	retained := s.mcpRetired[:0]
	for _, handler := range s.mcpRetired {
		select {
		case <-handler.Done():
		default:
			retained = append(retained, handler)
		}
	}
	clear(s.mcpRetired[len(retained):])
	s.mcpRetired = retained
	if handler := s.mcpHandler; handler != nil {
		s.mcpHandler = nil
		s.mcpRetired = append(s.mcpRetired, handler)
		go func() { _ = handler.Shutdown(context.Background()) }()
	}
	s.mcpMu.Unlock()
}

func (s *Server) shutdownMCP(ctx context.Context) error {
	s.mcpMu.Lock()
	s.mcpStopping = true
	handlers := append([]*mcpserver.Handler(nil), s.mcpRetired...)
	if s.mcpHandler != nil {
		handlers = append(handlers, s.mcpHandler)
	}
	s.mcpMu.Unlock()
	for _, handler := range handlers {
		if err := handler.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Cache discovery across calls, but never cache policy across a settings or
// pipeline change. A short refresh also detects newly installed disabled bundles.
func (s *Server) mcpPolicy() (*namespaceAccessPolicy, error) {
	s.controlPlaneMutationMu.Lock()
	defer s.controlPlaneMutationMu.Unlock()
	cfg := s.GetConfig().Providers
	revision := s.configRevision(cfg)
	var pipeline *plugin.PluginPipeline
	if raw := s.pluginPipeline.Load(); raw != nil {
		pipeline = raw.(*plugin.PluginPipeline)
	}
	now := time.Now()
	if snapshot := s.mcpCatalog; snapshot != nil && snapshot.revision == revision && snapshot.pipeline == pipeline && now.Before(snapshot.expires) {
		return snapshot.policy, nil
	}
	registry, err := s.currentNamespaceRegistryLocked()
	if err != nil {
		return nil, err
	}
	policy, err := newNamespaceAccessPolicy(registry, cfg.Plugins.ProtectedNamespaces(), cfg.MCP.Access)
	if err != nil {
		return nil, err
	}
	s.mcpCatalog = &mcpPolicySnapshot{revision: revision, pipeline: pipeline, expires: now.Add(time.Second), policy: policy}
	return policy, nil
}

func (s *Server) dispatchMCP(ctx context.Context, tool string, input json.RawMessage) (result mcpserver.Result, err error) {
	start := time.Now()
	binding := plugin.MCPBinding{}
	defer func() { s.appendMCPAudit(tool, binding.Bound, result, err, start) }()
	if err := ctx.Err(); err != nil {
		return mcpserver.Result{}, err
	}
	s.mcpMu.Lock()
	stopping := s.mcpStopping
	s.mcpMu.Unlock()
	if stopping {
		return operationError("not_configured", "Torana MCP is stopping."), nil
	}
	if !s.GetConfig().Providers.MCP.Enabled {
		return operationError("not_configured", "Torana MCP is disabled."), nil
	}
	switch tool {
	case "torana_namespaces", "torana_describe", "torana_search", "torana_invoke":
	default:
		return operationError("unknown_operation", "Choose a Torana tool from tools/list."), nil
	}
	releaseTool, allowed := s.mcpLimits.acquireLease("tool:" + tool)
	if !allowed {
		return mcpRateLimited(), nil
	}
	defer releaseTool()
	conversation := "unbound"
	if s.mcpCorrelation != nil {
		if evidence, ok := s.mcpCorrelation.Consume(tool, input, time.Now()); ok {
			binding = plugin.MCPBinding{Bound: true, ConversationID: evidence.ConversationID, CallID: evidence.CallID}
			conversation = evidence.ConversationID
		}
	}
	releaseConversation, allowed := s.mcpLimits.acquireLease("conversation:" + conversation)
	if !allowed {
		return mcpRateLimited(), nil
	}
	defer releaseConversation()
	policy, err := s.mcpPolicy()
	if err != nil {
		return mcpserver.Result{}, err
	}
	if tool != "torana_invoke" {
		return policy.catalogDispatch(tool, input), nil
	}
	dispatch := operationDispatch{policy: policy, execute: s.executeNamespaceOperation, propose: s.proposeNamespaceOperation}
	return dispatch.invoke(ctx, input, binding)
}

func mcpRateLimited() mcpserver.Result {
	result := operationError("rate_limited", "Too many Torana tool calls; wait and retry.")
	result.Error.Retryable = true
	result.Error.Details = &mcpserver.ErrorDetails{RetryAfterSeconds: 1}
	return result
}

// Audit only host-generated metadata. Never retain arguments, returned config,
// confirmation codes, tokens, or raw errors in this event.
func (s *Server) appendMCPAudit(tool string, bound bool, result mcpserver.Result, callErr error, start time.Time) {
	switch tool {
	case "torana_namespaces", "torana_describe", "torana_search", "torana_invoke":
	default:
		tool = "unknown"
	}
	code := ""
	if result.Error != nil {
		code = result.Error.Code
	}
	if callErr != nil {
		code = "internal_error"
	}
	s.auditMu.RLock()
	defer s.auditMu.RUnlock()
	if s.auditWriter == nil {
		return
	}
	err := s.auditWriter.Append(struct {
		SchemaVersion int       `json:"schema_version"`
		Type          string    `json:"type"`
		Timestamp     time.Time `json:"timestamp"`
		Tool          string    `json:"tool"`
		Bound         bool      `json:"bound"`
		OK            bool      `json:"ok"`
		ErrorCode     string    `json:"error_code,omitempty"`
		DurationMS    int64     `json:"duration_ms"`
	}{auditSchemaVersion, "mcp_call", start.UTC(), tool, bound, result.OK && callErr == nil, code, time.Since(start).Milliseconds()})
	if err != nil && s.stats != nil {
		s.stats.RecordAuditWriteFailure()
	}
}
