package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

// executeNamespaceOperation reuses digest-bound guest HTTP dispatch and its
// schema/header/response validation. It never calls the broad operator APIs
// for model-facing plugin listings or configuration: those include current
// plugin config and approvals that do not belong in a model's context.
func (s *Server) executeNamespaceOperation(ctx context.Context, call operationCall) (any, *mcpserver.DomainError, error) {
	if call.Operation.Source == "plugin" {
		guest := call.Operation.Guest
		if guest == nil {
			return nil, &mcpserver.DomainError{Code: "unknown_operation", Message: "Operation descriptor is unavailable."}, nil
		}
		if strings.ContainsAny(guest.Path, "{}?") {
			return nil, &mcpserver.DomainError{Code: "invalid_input", Message: "This operation needs route parameters; use Torana's CLI."}, nil
		}
		request, err := http.NewRequestWithContext(ctx, guest.Method, "http://localhost/_torana/api/v1/agent/plugins/"+url.PathEscape(call.Entry.Name)+guest.Path, bytes.NewReader(call.Input))
		if err != nil {
			return nil, nil, err
		}
		request.Header.Set("X-Torana-Plugin-Digest", call.Entry.Digest)
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "127.0.0.1:0"
		captured := newBufferedAgentResponse()
		s.handlePluginAgentOperation(captured, request)
		if captured.status < 200 || captured.status >= 300 {
			code := "plugin_failed"
			if captured.status == http.StatusPreconditionFailed {
				code = "stale_digest"
			}
			return nil, &mcpserver.DomainError{Code: code, Message: "The plugin could not complete this operation; describe it and try again."}, nil
		}
		return json.RawMessage(append([]byte(nil), captured.body.Bytes()...)), nil, nil
	}
	if call.Operation.Source == "standard" {
		switch call.Operation.ID {
		case "_info":
			return map[string]any{"name": call.Entry.Name, "title": catalogText(call.Entry.Title, 60), "summary": catalogText(call.Entry.Summary, 300), "version": call.Entry.Version, "digest": call.Entry.Digest, "status": call.Entry.Status, "categories": call.Entry.Categories}, nil, nil
		case "_status":
			return map[string]any{"name": call.Entry.Name, "status": call.Entry.Status}, nil, nil
		case "_config.schema":
			if len(call.Entry.ConfigSchema) == 0 {
				return map[string]any{"type": "object", "additionalProperties": false}, nil, nil
			}
			return call.Entry.ConfigSchema, nil, nil
		default:
			return nil, &mcpserver.DomainError{Code: "not_configured", Message: "This change needs the confirmation executor."}, nil
		}
	}
	if call.Operation.Source == "core" {
		switch call.Operation.ID {
		case "system.status":
			status := "running"
			if s.pluginReloadDegraded.Load() || s.pluginState != nil && s.pluginState.ReadOnly() {
				status = "degraded"
			}
			select {
			case <-s.stopRequested:
				status = "stopping"
			default:
			}
			return map[string]any{"service": "torana-edge", "version": s.GetConfig().HostVersion, "status": status, "uptime_seconds": int64(time.Since(s.startedAt).Seconds())}, nil, nil
		case "stats.get":
			return s.stats.Snapshot(), nil, nil
		case "plugins.list":
			// Get only public identity/status metadata; never the operator API's
			// config, credential declarations, approvals or permission grants.
			if call.Catalog == nil {
				return []catalogNamespace{}, nil, nil
			}
			return call.Catalog, nil, nil
		}
	}
	return nil, &mcpserver.DomainError{Code: "unknown_operation", Message: "This operation has no model-facing handler."}, nil
}

func (s *Server) currentNamespaceRegistry() (*namespaceRegistry, error) {
	s.controlPlaneMutationMu.Lock()
	defer s.controlPlaneMutationMu.Unlock()
	return s.currentNamespaceRegistryLocked()
}

// The caller holds controlPlaneMutationMu so registry and configuration
// revision belong to the same operator snapshot.
func (s *Server) currentNamespaceRegistryLocked() (*namespaceRegistry, error) {
	cfg := s.GetConfig().Providers.Plugins
	var bundles []plugin.PluginBundle
	var err error
	if cfg.Dir != "" {
		bundles, err = plugin.DiscoverPlugins(cfg.Dir)
		if err != nil {
			return nil, err
		}
	}
	var loaded []plugin.LoadedPluginStatus
	var skipped []plugin.SkippedPlugin
	if raw := s.pluginPipeline.Load(); raw != nil {
		pipeline := raw.(*plugin.PluginPipeline)
		if !pipeline.TryAcquire() {
			return nil, fmt.Errorf("plugin pipeline is reloading")
		}
		loaded, skipped = pipeline.LoadedPlugins(), pipeline.Skipped()
		pipeline.Release()
	}
	return buildNamespaceRegistry(bundles, loaded, cfg.Order, skipped)
}
