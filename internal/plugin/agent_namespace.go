package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func decodeAgentDescriptor(raw []byte, descriptor *AgentDescriptor) error {
	if err := json.Unmarshal(raw, descriptor); err != nil {
		return err
	}
	if descriptor.SchemaVersion != 2 {
		return nil
	}
	var fields struct {
		Namespace  map[string]json.RawMessage   `json:"namespace"`
		Operations []map[string]json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, present := fields.Namespace["alias"]; present {
		return fmt.Errorf("agent descriptor: namespace aliases were removed; use the canonical plugin name")
	}
	for _, operation := range fields.Operations {
		for _, removed := range []string{"directive", "user_direct"} {
			if _, present := operation[removed]; present {
				return fmt.Errorf("agent descriptor: %s was removed; expose operations through MCP", removed)
			}
		}
	}
	return nil
}

type AgentNamespace struct {
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Categories []string `json:"categories,omitempty"`
}

func ReservedNamespace(name string) bool {
	if strings.HasPrefix(name, "_") {
		return true
	}
	switch strings.ToLower(name) {
	case "torana", "host", "mcp", "system", "plugin", "plugins":
		return true
	}
	return false
}

func agentText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit && strings.IndexFunc(value, unicode.IsControl) < 0
}

// EffectiveModelAccess derives the least permissive allowed default from risk.
// Runtime policy may tighten it further, but must never loosen this declaration.
func (op AgentOperation) EffectiveModelAccess() string {
	if op.ModelAccess != "" {
		return op.ModelAccess
	}
	switch op.Risk {
	case "read":
		return "read"
	case "write":
		return "confirm"
	default:
		return "never"
	}
}

func validateAgentV2(d AgentDescriptor, manifest PluginManifest) error {
	if d.SchemaVersion == 1 {
		if d.Namespace != nil {
			return fmt.Errorf("agent descriptor: namespace requires schema_version 2")
		}
		for _, op := range d.Operations {
			if op.ModelAccess != "" || op.ConversationBinding != "" || len(op.Examples) > 0 || op.Deprecated || op.ReplacedBy != "" {
				return fmt.Errorf("agent descriptor: operation %q uses v2 fields with schema_version 1", op.ID)
			}
		}
		return nil
	}
	if d.Namespace == nil || !agentText(d.Namespace.Title, 60) || !agentText(d.Namespace.Summary, 300) {
		return fmt.Errorf("agent descriptor: v2 namespace needs bounded title and summary")
	}
	if ReservedNamespace(manifest.Name) {
		return fmt.Errorf("agent descriptor: reserved plugin namespace %q", manifest.Name)
	}
	if len(d.Namespace.Categories) > 16 {
		return fmt.Errorf("agent descriptor: too many namespace categories")
	}
	seen := map[string]bool{}
	for _, category := range d.Namespace.Categories {
		if !agentOperationIDPattern.MatchString(category) || seen[category] {
			return fmt.Errorf("agent descriptor: invalid or duplicate category %q", category)
		}
		seen[category] = true
	}
	operationIDs := map[string]bool{}
	for _, op := range d.Operations {
		operationIDs[op.ID] = true
	}
	for _, op := range d.Operations {
		access := op.EffectiveModelAccess()
		if access != "read" && access != "confirm" && access != "never" {
			return fmt.Errorf("agent descriptor: operation %q has invalid model_access", op.ID)
		}
		if (op.Risk == "write" && access == "read") || (op.Risk == "destructive" && access != "never") {
			return fmt.Errorf("agent descriptor: operation %q loosens model access beyond risk", op.ID)
		}
		switch op.ConversationBinding {
		case "", "none", "preferred", "required":
		default:
			return fmt.Errorf("agent descriptor: operation %q has invalid conversation_binding", op.ID)
		}
		if len(op.Examples) > 16 {
			return fmt.Errorf("agent descriptor: operation %q has too many examples", op.ID)
		}
		for _, example := range op.Examples {
			if !agentText(example, 300) {
				return fmt.Errorf("agent descriptor: operation %q has invalid example", op.ID)
			}
		}
		if op.ReplacedBy != "" && (!op.Deprecated || !agentOperationIDPattern.MatchString(op.ReplacedBy) || op.ReplacedBy == op.ID || !operationIDs[op.ReplacedBy]) {
			return fmt.Errorf("agent descriptor: operation %q has invalid replacement", op.ID)
		}

	}
	return nil
}
