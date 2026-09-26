package provider

import (
	"fmt"
	"regexp"
)

// MCP is opt-in. Access entries may tighten model access; they cannot relax
// descriptor policy or the protected-operation floor. Tokens are stored through
// the host secret store, never in operator configuration (which is never
// model-readable).
type MCPConfig struct {
	Enabled     bool              `json:"enabled,omitempty"`
	Access      map[string]string `json:"access,omitempty"`
	ServerNames []string          `json:"server_names,omitempty"`
}

func (c MCPConfig) ResponseServerNames() []string {
	if len(c.ServerNames) == 0 {
		return []string{"torana"}
	}
	return append([]string{}, c.ServerNames...)
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
	serverNames := map[string]bool{}
	for _, name := range c.MCP.ServerNames {
		if !protectedNamespacePattern.MatchString(name) || len(name) > 64 || serverNames[name] {
			return fmt.Errorf("mcp.server_names requires distinct server names of at most 64 characters")
		}
		serverNames[name] = true
	}
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
	return nil
}
