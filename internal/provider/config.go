// Package provider handles upstream provider configuration and URL routing.
// Providers are configured via JSON and keyed by name. Each provider declares
// its upstream URL and wire format.
package provider

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/economics"
)

// Provider describes an upstream LLM API endpoint.
type Provider struct {
	URL                 string                     `json:"url"`                            // upstream base URL
	Format              string                     `json:"format"`                         // wire format: "openai", "anthropic", "bedrock", "gemini", "gemini-codeassist"
	Fallback            []string                   `json:"fallback,omitempty"`             // provider names to try on 429/5xx
	ResponsesCompaction *ResponsesCompactionConfig `json:"responses_compaction,omitempty"` // native OpenAI Responses context compaction; nil disables it
	// APIKeyEnv names an environment variable holding this provider's own
	// API key. Used when a plugin reroutes a request here
	// (env.route_request) — the caller's credential is never forwarded to a
	// rerouted provider. Empty means the provider needs no auth (e.g. a
	// local model server).
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Pricing is optional, operator-supplied pricing by exact model name.
	// "*" may be used as a provider default. Torana intentionally ships no
	// built-in rates because provider prices and cache semantics change.
	Pricing map[string]economics.ModelPricing `json:"pricing,omitempty"`
}

// PricingFor returns an explicitly configured exact-model rate, falling back
// to the provider's "*" entry. It never guesses or downloads pricing.
func (p Provider) PricingFor(model string) (economics.ModelPricing, bool) {
	if price, ok := p.Pricing[model]; ok {
		return price, true
	}
	price, ok := p.Pricing["*"]
	return price, ok
}

// Config is the top-level Torana configuration.
type Config struct {
	Port      int                 `json:"port"`
	Providers map[string]Provider `json:"providers"`
	Plugins   PluginsConfig       `json:"plugins,omitempty"`
	Limits    Limits              `json:"limits,omitempty"`
	Offload   OffloadConfig       `json:"offload,omitempty"`
	// Cache selects the cross-request plugin state backend: in-process
	// memory (default) or Redis for distributed / restart-safe deployments.
	Cache cache.Config `json:"cache,omitempty"`
	// MITM optionally terminates TLS for harnesses that can't be pointed at a
	// base URL (e.g. the Antigravity CLI), routing intercepted hosts into the
	// provider pipeline. Disabled unless configured.
	MITM MITMConfig `json:"mitm,omitempty"`
}

// MITMConfig configures the TLS-terminating ingress. When enabled, agy (or any
// client trusting the generated CA and pointed here via HTTPS_PROXY) has its
// requests to the mapped hosts decrypted and routed through the named provider;
// all other hosts and non-chat paths are tunneled/forwarded verbatim.
type MITMConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Listen is the CONNECT proxy address (e.g. "127.0.0.1:8099"). Keep it on
	// localhost — it decrypts caller traffic.
	Listen string `json:"listen,omitempty"`
	// CADir holds the generated CA cert/key and the SSL_CERT_FILE bundle. The
	// CA private key never leaves this dir and must not be committed.
	CADir string `json:"ca_dir,omitempty"`
	// Hosts maps an upstream hostname to the provider name that handles its
	// chat calls (e.g. "cloudcode-pa.googleapis.com" -> "antigravity").
	Hosts map[string]string `json:"hosts,omitempty"`
}

// OffloadConfig controls cheap-model tool result summarization
// (the torana_offload_completion host call used by the compactor plugin).
type OffloadConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Provider names the configured provider used for offload calls.
	// Must exist in Providers and use the "openai" format.
	Provider string `json:"provider,omitempty"`
	// Model is the cheap model requested for summarization.
	Model string `json:"model,omitempty"`
	// APIKeyEnv names an environment variable holding a dedicated offload
	// API key. When empty, the caller's request credential is reused.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// ResponsesCompactionConfig enables provider-native compaction for OpenAI
// Responses API requests. It does not apply to Chat Completions requests.
type ResponsesCompactionConfig struct {
	CompactThreshold int `json:"compact_threshold"`
}

// ValidateResponsesCompaction rejects configured-but-ineffective thresholds.
// A nil configuration is intentionally valid and means disabled.
func (p Provider) ValidateResponsesCompaction(name string) error {
	if p.ResponsesCompaction == nil {
		return nil
	}
	if p.Format != "openai" {
		return fmt.Errorf("provider %q responses_compaction requires format %q, has %q", name, "openai", p.Format)
	}
	if p.ResponsesCompaction.CompactThreshold <= 0 {
		return fmt.Errorf("provider %q responses_compaction.compact_threshold must be positive", name)
	}
	return nil
}

// Validate checks an enabled offload config against the provider map.
// A disabled config is always valid.
func (o OffloadConfig) Validate(providers map[string]Provider) error {
	if !o.Enabled {
		return nil
	}
	p, ok := providers[o.Provider]
	if !ok {
		return fmt.Errorf("offload.provider %q not found in providers", o.Provider)
	}
	if p.Format != "openai" {
		return fmt.Errorf("offload.provider %q must use the openai format, has %q", o.Provider, p.Format)
	}
	if o.Model == "" {
		return fmt.Errorf("offload.model must be set when offload is enabled")
	}
	return nil
}

// Limits defines the rate limit and concurrency caps.
type Limits struct {
	Concurrency int `json:"concurrency,omitempty"`
	RPM         int `json:"rpm,omitempty"`
}

// PluginsConfig controls WASM plugin loading and execution.
type PluginsConfig struct {
	Dir    string                     `json:"dir"`    // plugins directory, default "./plugins"
	Order  []string                   `json:"order"`  // execution order by plugin name
	Config map[string]json.RawMessage `json:"config"` // per-plugin config blobs
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

	// Merge: user values override defaults.
	if user.Port != 0 {
		cfg.Port = user.Port
	}
	for name, p := range user.Providers {
		cfg.Providers[name] = p
	}
	if user.Plugins.Dir != "" {
		cfg.Plugins = user.Plugins
	}
	if user.Limits.RPM != 0 || user.Limits.Concurrency != 0 {
		cfg.Limits = user.Limits
	}
	if user.Offload != (OffloadConfig{}) {
		cfg.Offload = user.Offload
	}
	if user.Cache != (cache.Config{}) {
		cfg.Cache = user.Cache
	}
	if user.MITM.Enabled {
		cfg.MITM = user.MITM
	}
	for name, p := range cfg.Providers {
		if err := p.ValidateResponsesCompaction(name); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}
