package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type AgentNamespace struct {
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Alias      string   `json:"alias,omitempty"`
	Categories []string `json:"categories,omitempty"`
}

type AgentDirective struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	UserDirect bool     `json:"user_direct,omitempty"`
}

func ReservedNamespace(name string) bool {
	if strings.HasPrefix(name, "_") {
		return true
	}
	switch strings.ToLower(name) {
	case "torana", "host", "mcp", "system", "plugin", "plugins", "accept", "dismiss", "undo", "status", "help", "ask", "setup":
		return true
	}
	return false
}

// ValidateNamespaceAliases prevents an alias from hiding another plugin's
// canonical namespace or alias. Call before loading any guest into the runtime.
func ValidateNamespaceAliases(bundles []PluginBundle) error {
	owners := map[string]string{}
	claim := func(name, owner string) error {
		name = strings.ToLower(name)
		if previous, ok := owners[name]; ok && previous != owner {
			return fmt.Errorf("namespace %q is claimed by plugins %q and %q", name, previous, owner)
		}
		owners[name] = owner
		return nil
	}
	for _, bundle := range bundles {
		name := bundle.Manifest.Name
		if !ReservedNamespace(name) {
			if err := claim(name, name); err != nil {
				return err
			}
		}
		if bundle.Agent != nil && bundle.Agent.Namespace != nil && bundle.Agent.Namespace.Alias != "" {
			if err := claim(bundle.Agent.Namespace.Alias, name); err != nil {
				return err
			}
		}
	}
	return nil
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
			if op.ModelAccess != "" || op.ConversationBinding != "" || op.Directive != nil || len(op.Examples) > 0 || op.Deprecated || op.ReplacedBy != "" {
				return fmt.Errorf("agent descriptor: operation %q uses v2 fields with schema_version 1", op.ID)
			}
		}
		return nil
	}
	if d.Namespace == nil || !agentText(d.Namespace.Title, 60) || !agentText(d.Namespace.Summary, 300) {
		return fmt.Errorf("agent descriptor: v2 namespace needs bounded title and summary")
	}
	alias := d.Namespace.Alias
	if alias != "" && (!agentOperationIDPattern.MatchString(alias) || ReservedNamespace(alias)) {
		return fmt.Errorf("agent descriptor: reserved or invalid namespace alias %q", alias)
	}
	if ReservedNamespace(manifest.Name) && alias == "" {
		return fmt.Errorf("agent descriptor: reserved plugin namespace %q requires an alias", manifest.Name)
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
	commands := map[string]bool{}
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
		if op.Directive == nil {
			continue
		}
		directive := op.Directive
		if !agentOperationIDPattern.MatchString(directive.Command) || ReservedNamespace(directive.Command) || commands[directive.Command] {
			return fmt.Errorf("agent descriptor: invalid or duplicate directive command %q", directive.Command)
		}
		commands[directive.Command] = true
		if directive.UserDirect && op.Risk != "write" {
			return fmt.Errorf("agent descriptor: user_direct requires an eligible write operation")
		}
		if err := validateDirectiveArgs(op); err != nil {
			return fmt.Errorf("agent descriptor: operation %q: %w", op.ID, err)
		}
	}
	return nil
}

func validateDirectiveArgs(op AgentOperation) error {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if len(op.InputSchema) > 0 {
		if err := json.Unmarshal(op.InputSchema, &schema); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	optionalSeen := false
	for _, arg := range op.Directive.Args {
		optional := strings.HasSuffix(arg, "?")
		name := strings.TrimSuffix(arg, "?")
		property, ok := schema.Properties[name]
		if !ok || seen[name] || (optionalSeen && !optional) {
			return fmt.Errorf("invalid directive argument %q", arg)
		}
		seen[name] = true
		optionalSeen = optionalSeen || optional
		var field struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(property, &field) != nil {
			return fmt.Errorf("invalid directive property %q", name)
		}
		switch field.Type {
		case "string", "number", "integer", "boolean":
		default:
			return fmt.Errorf("directive argument %q must be scalar", name)
		}
		for _, required := range schema.Required {
			if required == name && optional {
				return fmt.Errorf("required argument %q cannot be optional", name)
			}
		}
	}
	for _, required := range schema.Required {
		if !seen[required] {
			return fmt.Errorf("required property %q has no directive argument", required)
		}
	}
	return nil
}
