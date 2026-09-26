package proxy

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

// Provenance is host-owned. A guest can declare operations only in its own
// namespace; standard and core operations are constructed here, never parsed
// from guest metadata. Runtime access enforcement follows this registry.
type namespaceOperation struct {
	ID                  string                 `json:"id"`
	Description         string                 `json:"description"`
	Risk                string                 `json:"risk"`
	ModelAccess         string                 `json:"model_access"`
	ConversationBinding string                 `json:"conversation_binding"`
	Callable            bool                   `json:"callable"`
	Source              string                 `json:"-"`
	CoreID              string                 `json:"-"`
	Guest               *plugin.AgentOperation `json:"-"`
}

type namespaceEntry struct {
	Name         string               `json:"name"`
	Title        string               `json:"title"`
	Summary      string               `json:"summary"`
	Categories   []string             `json:"categories,omitempty"`
	Status       string               `json:"status"`
	Digest       string               `json:"digest,omitempty"`
	Version      string               `json:"version,omitempty"`
	ConfigSchema json.RawMessage      `json:"-"`
	Operations   []namespaceOperation `json:"operations"`
}

type namespaceRegistry struct {
	entries map[string]namespaceEntry
}

// Installed bundles provide status metadata. Guest callability is granted only
// from the exact digest in the pinned, approved pipeline snapshot.
func buildNamespaceRegistry(installed []plugin.PluginBundle, loaded []plugin.LoadedPluginStatus, enabled []string, skipped []plugin.SkippedPlugin) (*namespaceRegistry, error) {
	r := &namespaceRegistry{entries: map[string]namespaceEntry{}}
	core := namespaceEntry{Name: "torana", Title: "Torana", Summary: "Inspect and manage the local proxy and this conversation", Status: "enabled"}
	for _, op := range builtInAgentOperations() {
		id := ""
		binding := "none"
		switch op.ID {
		case "torana.system.status":
			id = "system.status"
		case "torana.plugins.list":
			id = "plugins.list"
		case "torana.stats.get":
			id = "stats.get"
		case "torana.feed.list":
			id = "feed.recent"
			binding = "required"
		case "torana.suggestions.list":
			id = "suggestions.list"
			binding = "required"
		default:
			continue // No config, shutdown, discovery or cross-session access.
		}
		access, callable := "read", true
		core.Operations = append(core.Operations, namespaceOperation{ID: id, Description: op.Description, Risk: "read", ModelAccess: access, ConversationBinding: binding, Callable: callable, Source: "core", CoreID: op.ID})
	}
	for _, op := range []namespaceOperation{
		{ID: "session.usage", Description: "Usage for this conversation; cache token counts are subsets of tokens_in", Risk: "read", ModelAccess: "read"},
		{ID: "changes.list", Description: "Changes for this conversation", Risk: "read", ModelAccess: "read"},
		{ID: "changes.undo", Description: "Undo a confirmed change", Risk: "write", ModelAccess: "confirm"},
	} {
		op.Source = "core"
		op.ConversationBinding = "required"
		op.Callable = op.Risk == "read" // Undo is available only to operators.
		core.Operations = append(core.Operations, op)
	}
	r.entries["torana"] = core
	active := map[string]plugin.LoadedPluginStatus{}
	for _, item := range loaded {
		active[item.Name] = item
	}
	wanted := map[string]bool{}
	for _, name := range enabled {
		wanted[name] = true
	}
	unapproved := map[string]bool{}
	for _, item := range skipped {
		unapproved[item.Name] = true
	}
	for _, bundle := range installed {
		name := bundle.Manifest.Name
		if _, exists := r.entries[name]; exists && !plugin.ReservedNamespace(name) {
			return nil, fmt.Errorf("duplicate installed namespace %q", name)
		}
		if plugin.ReservedNamespace(name) {
			continue
		}
		entry := namespaceEntry{Name: name, Title: name, Summary: bundle.Manifest.Description, Status: "disabled", Digest: bundle.Digest, Version: bundle.Manifest.Version}
		if bundle.Schema != nil {
			entry.ConfigSchema = append(json.RawMessage(nil), bundle.Schema.Raw...)
		}
		descriptor := bundle.Agent
		loadedBundle, live := active[name]
		if live && loadedBundle.Digest == bundle.Digest {
			entry.Status = "enabled"
			descriptor = loadedBundle.Agent
		} else if wanted[name] {
			entry.Status = "failed"
			if unapproved[name] {
				entry.Status = "unapproved"
			}
		}
		if descriptor != nil && descriptor.Namespace != nil {
			ns := descriptor.Namespace
			entry.Title, entry.Summary = ns.Title, ns.Summary
			entry.Categories = append([]string(nil), ns.Categories...)
		}
		for _, standard := range []struct{ id, description, risk, access string }{
			{"_info", "Plugin information", "read", "read"}, {"_status", "Plugin status", "read", "read"}, {"_config.schema", "Configuration schema", "read", "read"}, {"_config.get", "Current configuration", "read", "never"}, {"_config.set", "Update configuration", "write", "confirm"}, {"_enable", "Enable plugin", "write", "confirm"}, {"_disable", "Disable plugin", "write", "confirm"},
		} {
			anyStatus := standard.id == "_info" || standard.id == "_status" || standard.id == "_enable"
			callable := entry.Status == "enabled" || anyStatus
			if standard.id == "_enable" && entry.Status == "unapproved" {
				callable = false
			}
			entry.Operations = append(entry.Operations, namespaceOperation{ID: standard.id, Description: standard.description, Risk: standard.risk, ModelAccess: standard.access, ConversationBinding: "none", Callable: callable, Source: "standard"})
		}
		if descriptor != nil {
			for _, op := range descriptor.Operations {
				guestOperation := op
				binding := op.ConversationBinding
				if binding == "" {
					binding = "none"
				}
				entry.Operations = append(entry.Operations, namespaceOperation{ID: op.ID, Description: op.Description, Risk: op.Risk, ModelAccess: op.EffectiveModelAccess(), ConversationBinding: binding, Callable: entry.Status == "enabled", Source: "plugin", Guest: &guestOperation})
			}
		}
		r.entries[name] = entry
	}
	return r, nil
}

func (r *namespaceRegistry) list() []namespaceEntry {
	entries := make([]namespaceEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

func (r *namespaceRegistry) resolve(name string) (namespaceEntry, bool) {
	entry, ok := r.entries[name]
	return entry, ok
}
