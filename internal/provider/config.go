// Package provider handles upstream provider configuration and URL routing.
// Providers are configured via JSON and keyed by name. Each provider declares
// its upstream URL and wire format.
package provider

import (
	"encoding/json"
	"fmt"
	"os"
)

// Provider describes an upstream LLM API endpoint.
type Provider struct {
	URL      string   `json:"url"`    // upstream base URL
	Format   string   `json:"format"` // wire format: "openai", "anthropic", "bedrock", "vertex"
	Fallback []string `json:"fallback,omitempty"` // provider names to try on 429/5xx
}

// Config is the top-level Torana configuration.
type Config struct {
	Port      int                 `json:"port"`
	Providers map[string]Provider `json:"providers"`
	Plugins   PluginsConfig       `json:"plugins,omitempty"`
	Offload   OffloadConfig        `json:"offload,omitempty"` // deprecated — use plugins.config.compactor
}

// PluginsConfig controls WASM plugin loading and execution.
type PluginsConfig struct {
	Dir    string                     `json:"dir"`    // plugins directory, default "./plugins"
	Order  []string                   `json:"order"`  // execution order by plugin name
	Config map[string]json.RawMessage `json:"config"` // per-plugin config blobs
}

// OffloadConfig controls tool-result compaction via a cheaper model.
type OffloadConfig struct {
	// Enabled toggles model offloading. Defaults to false.
	Enabled bool `json:"enabled,omitempty"`
	// Model is the cheaper model used for compaction (e.g. "deepseek-v4-flash").
	Model string `json:"model,omitempty"`
	// Provider is which configured provider to use for offload API calls.
	// Defaults to "deepseek" if unset.
	Provider string `json:"provider,omitempty"`
}

// DefaultConfig returns the built-in configuration for common providers.
// Users override or extend this with a config.json file.
func DefaultConfig() Config {
	return Config{
		Port: 8080,
		Providers: map[string]Provider{
			"deepseek": {
				URL:    "https://api.deepseek.com/beta",
				Format: "openai",
			},
			"deepseek-anthropic": {
				URL:    "https://api.deepseek.com/anthropic",
				Format: "anthropic",
			},
			"openai": {
				URL:    "https://api.openai.com",
				Format: "openai",
			},
			"anthropic": {
				URL:    "https://api.anthropic.com",
				Format: "anthropic",
			},
		},
	}
}

// Load reads a JSON config file and merges it over the defaults.
// If the file doesn't exist, the defaults are returned as-is.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // no user config, use defaults
		}
		return cfg, fmt.Errorf("reading config %q: %w", path, err)
	}

	var user Config
	if err := json.Unmarshal(raw, &user); err != nil {
		return cfg, fmt.Errorf("parsing config %q: %w", path, err)
	}

	// Merge: user port overrides default, provider map merges, offload merges.
	if user.Port != 0 {
		cfg.Port = user.Port
	}
	for name, p := range user.Providers {
		cfg.Providers[name] = p
	}
	if user.Offload.Enabled {
		cfg.Offload.Enabled = true
	}
	if user.Offload.Model != "" {
		cfg.Offload.Model = user.Offload.Model
	}
	if user.Offload.Provider != "" {
		cfg.Offload.Provider = user.Offload.Provider
	}
	if user.Plugins.Dir != "" {
		cfg.Plugins = user.Plugins
	}

	return cfg, nil
}
