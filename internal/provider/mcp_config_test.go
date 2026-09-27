package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMCPConfigurationDefaultsAndExplicitProtection(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MCP.Enabled || len(cfg.Plugins.ProtectedNamespaces()) != 3 {
		t.Fatal("unexpected opt-in/default policy")
	}
	cfg.Plugins.Protected = []string{}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Config
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Plugins.Protected == nil || len(loaded.Plugins.ProtectedNamespaces()) != 0 {
		t.Fatal("explicit empty protection reverted to defaults")
	}
}

func TestMCPConsentPolicy(t *testing.T) {
	for _, mode := range []string{"", "elicitation", "operator_only"} {
		cfg := DefaultConfig()
		cfg.MCP.Consent = mode
		if err := cfg.validateMCPConfiguration(); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.MCP.Consent = "auto_approve"
	if cfg.validateMCPConfiguration() == nil {
		t.Fatal("invalid consent policy accepted")
	}
}

func TestUnmanagedLoadPreservesFeatureConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"suggestions":{"enabled":true},"mcp":{"enabled":true,"access":{"logger._disable":"never"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Suggestions.Enabled || !cfg.MCP.Enabled || cfg.MCP.Access["logger._disable"] != "never" {
		t.Fatalf("configuration discarded: %+v", cfg)
	}
}

func TestLoadRejectsRemovedChatControlSettings(t *testing.T) {
	for name, raw := range map[string]string{
		"directives": `{"directives":{"enabled":true}}`,
		"assistant":  `{"assistant":{"provider":"local"}}`,
		"harness":    `{"harness":{"setup_from_directive":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("removed chat-control setting accepted")
			}
		})
	}
}

func TestMCPConfigurationRejectsInvalidOperatorPolicy(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.MCP.Access = map[string]string{"logger": "allow"} },
		func(c *Config) { c.MCP.Access = map[string]string{"logger\n": "read"} },
		func(c *Config) { c.MCP.ServerNames = []string{"torana", "torana"} },
		func(c *Config) { c.MCP.ServerNames = []string{"bad name"} },
		func(c *Config) { c.Plugins.Protected = []string{"pii", "pii"} },
		func(c *Config) { c.Plugins.Protected = []string{"bad name"} },
	} {
		cfg := DefaultConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
