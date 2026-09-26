package proxy

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func TestNamespacePolicyExhaustiveStandardProtectionAndOverrides(t *testing.T) {
	for _, name := range []string{"pii", "pii_guard", "auth", "logger"} {
		bundle := plugin.PluginBundle{Manifest: plugin.PluginManifest{Name: name}, Digest: "digest"}
		r, err := buildNamespaceRegistry([]plugin.PluginBundle{bundle}, []plugin.LoadedPluginStatus{{Name: name, Digest: "digest"}}, []string{name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		entry, _ := r.resolve(name, false)
		for _, override := range []string{"read", "confirm", "never"} {
			p, err := newNamespaceAccessPolicy(r, nil, map[string]string{name: override})
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range entry.Operations {
				got := p.ModelReachable(name, op.ID)
				floor := op.ID == "_config.get" || (name != "logger" && op.Risk != "read")
				want := op.ModelAccess
				if accessRank(override) > accessRank(want) {
					want = override
				}
				if floor {
					want = "never"
				}
				if got != want {
					t.Fatalf("%s.%s override=%s access=%s want=%s", name, op.ID, override, got, want)
				}
				if floor && p.DirectiveAllowed(name, op.ID).Allowed {
					t.Fatalf("floor reachable as directive: %s.%s", name, op.ID)
				}
			}
		}
	}
}

func TestNamespacePolicyCoreFloorCannotBeReached(t *testing.T) {
	r, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, map[string]string{"torana": "read"})
	if err != nil {
		t.Fatal(err)
	}
	for _, builtIn := range builtInAgentOperations() {
		allowed := false
		switch builtIn.ID {
		case "torana.system.status", "torana.plugins.list", "torana.stats.get", "torana.feed.list", "torana.suggestions.list":
			allowed = true
		}
		if allowed {
			continue
		}
		operation := builtIn.ID[len("torana."):]
		if p.ModelReachable("torana", operation) != "never" || p.DirectiveAllowed("torana", operation).Allowed {
			t.Fatalf("operator-only core operation reachable: %s", builtIn.ID)
		}
	}
}

func TestDirectiveUserDirectIndependentOfModelAccess(t *testing.T) {
	op := plugin.AgentOperation{ID: "route.pin", Risk: "write", Directive: &plugin.AgentDirective{Command: "pin", UserDirect: true}}
	d := &plugin.AgentDescriptor{Namespace: &plugin.AgentNamespace{Alias: "router"}, Operations: []plugin.AgentOperation{op}}
	b := plugin.PluginBundle{Manifest: plugin.PluginManifest{Name: "decision_router"}, Digest: "digest", Agent: d}
	r, err := buildNamespaceRegistry([]plugin.PluginBundle{b}, []plugin.LoadedPluginStatus{{Name: b.Manifest.Name, Digest: b.Digest, Agent: d}}, []string{b.Manifest.Name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		override         string
		allowed, confirm bool
	}{{"", true, false}, {"confirm", true, false}, {"never", true, false}} {
		overrides := map[string]string{}
		if tc.override != "" {
			overrides[b.Manifest.Name+".route.pin"] = tc.override
		}
		p, err := newNamespaceAccessPolicy(r, nil, overrides)
		if err != nil {
			t.Fatal(err)
		}
		got := p.DirectiveAllowed("router", "route.pin")
		if got.Allowed != tc.allowed || got.Confirm != tc.confirm {
			t.Fatalf("override=%s policy=%+v", tc.override, got)
		}
		if p.ModelReachable("router", "route.pin") != "never" {
			t.Fatal("model accepted alias instead of canonical namespace")
		}
	}
}

func TestNamespacePolicyUnscopedCoreHandlersStayUnavailable(t *testing.T) {
	r, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, map[string]string{"torana": "read"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"feed.recent", "suggestions.list", "session.usage", "changes.list", "changes.undo"} {
		if p.ModelReachable("torana", id) != "never" || p.DirectiveAllowed("torana", id).Allowed {
			t.Fatalf("unscoped operation reachable: %s", id)
		}
	}
}
