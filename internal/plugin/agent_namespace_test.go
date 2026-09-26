package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestV2DescriptorRejectsRemovedChatFields(t *testing.T) {
	for _, field := range []string{"directive", "user_direct", "alias"} {
		t.Run(field, func(t *testing.T) {
			d, m := validNamespaceDescriptor()
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			if field == "alias" {
				document["namespace"].(map[string]any)[field] = nil
			} else {
				document["operations"].([]any)[0].(map[string]any)[field] = nil
			}
			raw, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			var parsed AgentDescriptor
			if err := decodeAgentDescriptor(raw, &parsed); err == nil || !strings.Contains(err.Error(), "removed") {
				t.Fatalf("removed field accepted: %v", err)
			}
			clean, _ := json.Marshal(d)
			if err := decodeAgentDescriptor(clean, &parsed); err != nil {
				t.Fatal(err)
			}
			if err := validateAgentDescriptor(parsed, m); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func validNamespaceDescriptor() (AgentDescriptor, PluginManifest) {
	return AgentDescriptor{SchemaVersion: 2, Namespace: &AgentNamespace{Title: "Routing", Summary: "Choose a model for this conversation", Categories: []string{"routing"}}, Operations: []AgentOperation{{ID: "route.pin", Method: "POST", Path: "/pin", Description: "Pin a step", Risk: "write", Idempotent: true, ConversationBinding: "required", InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"string"},"turns":{"type":"integer"}},"required":["step"],"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object"}`)}}}, PluginManifest{Name: "decision_router", Hooks: []Hook{{Name: "run_on_http_request"}}, Permissions: []Permission{{Name: "env.serve_http"}}}
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
		{"missing_namespace", func(d *AgentDescriptor, _ *PluginManifest) { d.Namespace = nil }},
		{"v1_smuggling", func(d *AgentDescriptor, _ *PluginManifest) { d.SchemaVersion = 1 }},
		{"reserved_operation", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ID = "_disable" }},
		{"loosened_access", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ModelAccess = "read" }},
		{"unknown_binding", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ConversationBinding = "any" }},
		{"bad_replacement", func(d *AgentDescriptor, _ *PluginManifest) { d.Operations[0].ReplacedBy = "route.pin" }},
		{"missing_replacement", func(d *AgentDescriptor, _ *PluginManifest) {
			d.Operations[0].Deprecated = true
			d.Operations[0].ReplacedBy = "missing"
		}},
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

func TestNamespaceDescriptorCloneIsIndependent(t *testing.T) {
	d, _ := validNamespaceDescriptor()
	d.Operations[0].Examples = []string{"pin the model"}
	cloned := cloneAgentDescriptor(&d)
	cloned.Namespace.Categories[0] = "changed"
	cloned.Operations[0].Examples[0] = "changed"
	if d.Namespace.Categories[0] != "routing" || d.Operations[0].Examples[0] != "pin the model" {
		t.Fatal("clone shares mutable descriptor data")
	}
}
