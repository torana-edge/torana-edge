package proxy

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func TestNamespaceRegistryRequiresExactLoadedDigest(t *testing.T) {
	d := &plugin.AgentDescriptor{SchemaVersion: 2, Namespace: &plugin.AgentNamespace{Title: "Routing", Summary: "Choose models", Alias: "router"}, Operations: []plugin.AgentOperation{{ID: "conversation.get", Risk: "read", ConversationBinding: "required"}}}
	b := plugin.PluginBundle{Manifest: plugin.PluginManifest{Name: "decision_router", Description: "Router"}, Digest: "new", Agent: d}
	for _, tc := range []struct {
		digest, status string
		live           bool
	}{
		{"new", "enabled", true}, {"old", "failed", false},
	} {
		r, err := buildNamespaceRegistry([]plugin.PluginBundle{b}, []plugin.LoadedPluginStatus{{Name: "decision_router", Digest: tc.digest, Agent: d}}, []string{"decision_router"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		entry, ok := r.resolve("decision_router", false)
		if !ok || entry.Status != tc.status {
			t.Fatalf("entry = %+v", entry)
		}
		for _, op := range entry.Operations {
			if op.ID == "conversation.get" && op.Callable != tc.live {
				t.Fatalf("guest callable = %t", op.Callable)
			}
		}
		if _, ok := r.resolve("router", false); ok {
			t.Fatal("MCP canonical lookup accepted alias")
		}
		if _, ok := r.resolve("router", true); !ok {
			t.Fatal("directive alias lookup failed")
		}
	}
}

func TestNamespaceRegistryListsUnavailablePluginsWithoutExecutingThem(t *testing.T) {
	for _, status := range []string{"disabled", "unapproved", "failed"} {
		var enabled []string
		var skipped []plugin.SkippedPlugin
		if status != "disabled" {
			enabled = []string{"logger"}
		}
		if status == "unapproved" {
			skipped = []plugin.SkippedPlugin{{Name: "logger"}}
		}
		r, err := buildNamespaceRegistry([]plugin.PluginBundle{{Manifest: plugin.PluginManifest{Name: "logger"}, Agent: &plugin.AgentDescriptor{Operations: []plugin.AgentOperation{{ID: "logs.read", Risk: "read"}}}}}, nil, enabled, skipped)
		if err != nil {
			t.Fatal(err)
		}
		entry, _ := r.resolve("logger", false)
		if entry.Status != status {
			t.Fatalf("status = %s", entry.Status)
		}
		for _, op := range entry.Operations {
			if op.Source == "plugin" && op.Callable {
				t.Fatal("unavailable guest operation callable")
			}
			if (op.ID == "_info" || op.ID == "_status" || (op.ID == "_enable" && status != "unapproved")) && !op.Callable {
				t.Fatalf("status operation %s unavailable", op.ID)
			}
			if op.ID == "_enable" && status == "unapproved" && op.Callable {
				t.Fatal("unapproved plugin can be enabled")
			}
			if op.ID == "_config.get" && op.ModelAccess != "never" {
				t.Fatal("configuration readable by model")
			}
		}
	}
}

func TestCoreNamespaceOmitsOperatorOnlySurfaces(t *testing.T) {
	r, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	core, _ := r.resolve("torana", false)
	seen := map[string]bool{}
	for _, op := range core.Operations {
		seen[op.ID] = true
		if (op.ID == "feed.recent" || op.ID == "suggestions.list") && (op.Callable || op.ModelAccess != "never") {
			t.Fatal("unscoped operator handler exposed")
		}
		if op.ID == "feed.recent" && op.ConversationBinding != "required" {
			t.Fatal("feed not conversation bound")
		}
	}
	for _, id := range []string{"config.get", "config.update", "system.stop", "conversations.list", "suggestions.accept", "suggestions.dismiss"} {
		if seen[id] {
			t.Fatalf("floor operation exposed: %s", id)
		}
	}
	for _, id := range []string{"system.status", "plugins.list", "stats.get", "feed.recent", "session.usage", "suggestions.list", "changes.list", "changes.undo"} {
		if !seen[id] {
			t.Fatalf("missing core operation %s", id)
		}
	}
}

func TestNamespaceAliasCollisionDoesNotBreakDiscovery(t *testing.T) {
	bundles := []plugin.PluginBundle{
		{Manifest: plugin.PluginManifest{Name: "decision_router"}, Agent: &plugin.AgentDescriptor{Namespace: &plugin.AgentNamespace{Alias: "logger"}}},
		{Manifest: plugin.PluginManifest{Name: "logger"}},
	}
	for _, installed := range [][]plugin.PluginBundle{bundles, {bundles[1], bundles[0]}} {
		r, err := buildNamespaceRegistry(installed, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		entry, ok := r.resolve("decision_router", false)
		if !ok || entry.AliasError == "" {
			t.Fatal("collision not reported")
		}
		logger, ok := r.resolve("logger", true)
		if !ok || logger.Name != "logger" {
			t.Fatal("alias hides canonical namespace")
		}
	}
}
