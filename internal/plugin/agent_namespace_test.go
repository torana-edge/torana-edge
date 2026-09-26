package plugin

import (
	"encoding/json"
	"testing"
)

func validNamespaceDescriptor() (AgentDescriptor, PluginManifest) {
	return AgentDescriptor{SchemaVersion: 2, Namespace: &AgentNamespace{Title: "Routing", Summary: "Choose a model for this conversation", Alias: "router", Categories: []string{"routing"}}, Operations: []AgentOperation{{ID: "route.pin", Method: "POST", Path: "/pin", Description: "Pin a step", Risk: "write", Idempotent: true, ConversationBinding: "required", InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"string"},"turns":{"type":"integer"}},"required":["step"],"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Directive: &AgentDirective{Command: "pin", Args: []string{"step", "turns?"}, UserDirect: true}}}}, PluginManifest{Name: "decision_router", Hooks: []Hook{{Name: "run_on_http_request"}}, Permissions: []Permission{{Name: "env.serve_http"}}}
}

func TestAgentNamespaceV2Validation(t *testing.T) {
	d, m := validNamespaceDescriptor()
	if err := validateAgentDescriptor(d, m); err != nil {
		t.Fatal(err)
	}
	if d.Operations[0].EffectiveModelAccess() != "confirm" {
		t.Fatal("write default does not require consent")
	}
	for _, tc := range []struct {
		name   string
		change func(*AgentDescriptor, *PluginManifest)
	}{
		{"reserved_alias", func(d *AgentDescriptor, _ *PluginManifest) { d.Namespace.Alias = "accept" }},
		{"missing_namespace", func(d *AgentDescriptor, _ *PluginManifest) { d.Namespace = nil }},
		{"v1_smuggling", func(d *AgentDescriptor, _ *PluginManifest) { d.SchemaVersion = 1 }},
		{"reserved_operation", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ID = "_disable" }},
		{"loosened_access", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ModelAccess = "read" }},
		{"unknown_binding", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ConversationBinding = "any" }},
		{"protected_direct", func(_ *AgentDescriptor, m *PluginManifest) { m.Name = "pii_guard" }},
		{"missing_property", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].Directive.Args = []string{"unknown"} }},
		{"optional_required", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].Directive.Args = []string{"step?"} }},
		{"required_after_optional", func(d *AgentDescriptor, _ *PluginManifest) {
			d.Operations[0].Directive.Args = []string{"turns?", "step"}
		}},
		{"duplicate_arg", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].Directive.Args = []string{"step", "step"} }},
		{"reserved_command", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].Directive.Command = "undo" }},
		{"bad_replacement", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ReplacedBy = "route.pin" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, m := validNamespaceDescriptor()
			tc.change(&d, &m)
			if err := validateAgentDescriptor(d, m); err == nil {
				t.Fatal("unsafe descriptor accepted")
			}
		})
	}
}

func TestNamespaceAliasesCannotHideCanonicalNames(t *testing.T) {
	d, m := validNamespaceDescriptor()
	for _, name := range []string{"router", "ROUTER"} {
		if err := ValidateNamespaceAliases([]PluginBundle{{Manifest: m, Agent: &d}, {Manifest: PluginManifest{Name: name}}}); err == nil {
			t.Fatal("alias collision accepted")
		}
	}
	if err := ValidateNamespaceAliases([]PluginBundle{{Manifest: m, Agent: &d}, {Manifest: PluginManifest{Name: "usage_logger"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceDescriptorCloneIsIndependent(t *testing.T) {
	d, _ := validNamespaceDescriptor()
	d.Operations[0].Examples = []string{"pin the model"}
	copy := cloneAgentDescriptor(&d)
	copy.Namespace.Categories[0] = "changed"
	copy.Operations[0].Directive.Args[0] = "changed"
	copy.Operations[0].Examples[0] = "changed"
	if d.Namespace.Categories[0] != "routing" || d.Operations[0].Directive.Args[0] != "step" || d.Operations[0].Examples[0] != "pin the model" {
		t.Fatal("clone shares mutable descriptor data")
	}
}
