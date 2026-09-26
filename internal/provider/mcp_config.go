package provider

import (
	"fmt"
	"regexp"
	"strings"
)

// MCP is opt-in. Access entries may tighten model access; they cannot relax
// descriptor policy or the protected-operation floor. Tokens are stored through
// the host secret store, never in this model-readable configuration section.
type MCPConfig struct {
	Enabled bool              `json:"enabled,omitempty"`
	Access  map[string]string `json:"access,omitempty"`
}

type AssistantConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

type HarnessConfig struct {
	SetupFromDirective *bool `json:"setup_from_directive,omitempty"`
}

func (h HarnessConfig) DirectiveSetupEnabled() bool {
	return h.SetupFromDirective == nil || *h.SetupFromDirective
}

func (p PluginsConfig) ProtectedNamespaces() []string {
	if p.Protected == nil {
		return []string{"pii", "pii_guard", "auth"}
	}
	return append([]string{}, p.Protected...)
}

var mcpAccessKeyPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,255}$`)
var protectedNamespacePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,127}$`)

func (c Config) validateMCPConfiguration() error {
	for key, access := range c.MCP.Access {
		if !mcpAccessKeyPattern.MatchString(key) || access != "read" && access != "confirm" && access != "never" {
			return fmt.Errorf("mcp.access requires namespace or namespace.operation keys with read, confirm, or never values")
		}
	}
	seen := map[string]bool{}
	for _, name := range c.Plugins.Protected {
		if !protectedNamespacePattern.MatchString(name) || seen[name] {
			return fmt.Errorf("plugins.protected requires distinct plugin namespace names")
		}
		seen[name] = true
	}
	if c.Assistant.Provider == "" && c.Assistant.Model == "" {
		return nil
	}
	if c.Assistant.Provider == "" || strings.TrimSpace(c.Assistant.Model) == "" || len(c.Assistant.Model) > 256 || strings.ContainsAny(c.Assistant.Model, "\r\n\x00") {
		return fmt.Errorf("assistant requires both provider and a bounded model name")
	}
	if _, exists := c.Providers[c.Assistant.Provider]; !exists {
		return fmt.Errorf("assistant.provider must name a configured provider")
	}
	return nil
}
