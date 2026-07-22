// Package provider handles upstream provider configuration and URL routing.
// Providers are configured via JSON and keyed by name. Each provider declares
// its upstream URL and wire format.
package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/torana-edge/torana-edge/internal/cache"
)

// Provider describes an upstream LLM API endpoint.
type Provider struct {
	URL      string   `json:"url"`                // upstream base URL
	Format   string   `json:"format"`             // wire format: "openai", "anthropic", "bedrock", "gemini", "gemini-codeassist"
	Fallback []string `json:"fallback,omitempty"` // provider names to try on 429/5xx
	// APIKeyEnv names an environment variable holding this provider's own
	// API key. Used when a plugin reroutes a request here
	// (env.route_request) — the caller's credential is never forwarded to a
	// rerouted provider. Empty means the provider needs no auth (e.g. a
	// local model server).
	APIKeyEnv string `json:"api_key_env,omitempty"`
	APIKeyEnc string `json:"api_key_enc,omitempty"`
}

// Config is the top-level Torana configuration.
type Config struct {
	Managed   bool                `json:"managed,omitempty"`
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
	// ControlPlane configures access control for the /_torana/* endpoints.
	ControlPlane ControlPlaneConfig `json:"control_plane,omitempty"`
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
	APIKeyEnc string `json:"api_key_enc,omitempty"`
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

// ControlPlaneConfig configures access control for the /_torana/* endpoints.
// Default (zero value) is loopback-only with no token. AllowRemote permits
// non-loopback callers; when Token is set, requests that provide it are allowed.
type ControlPlaneConfig struct {
	AllowRemote bool   `json:"allow_remote,omitempty"`
	Token       string `json:"token,omitempty"`
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
// If user.Managed is true, default-merge is skipped and user config is returned verbatim.
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

	if user.Managed {
		if user.Port == 0 {
			user.Port = 8080
		}
		if user.Providers == nil {
			user.Providers = make(map[string]Provider)
		}
		return user, nil
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
	if user.ControlPlane != (ControlPlaneConfig{}) {
		cfg.ControlPlane = user.ControlPlane
	}

	return cfg, nil
}

// Save writes cfg to path atomically with Managed set to true.
func Save(path string, cfg Config) error {
	cfg.Managed = true
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, ".config.json.tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp config file: %w", err)
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp config file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp config file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp config file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp config file: %w", err)
	}
	return nil
}

// ManagedStorePath returns the path to Torana's managed configuration file.
// It resolves to $TORANA_DATA_DIR/config.json if TORANA_DATA_DIR is set,
// otherwise os.UserConfigDir()/torana/config.json.
func ManagedStorePath() (string, error) {
	dataDir := os.Getenv("TORANA_DATA_DIR")
	if dataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("getting user config dir: %w", err)
		}
		dataDir = filepath.Join(dir, "torana")
	}
	return filepath.Join(dataDir, "config.json"), nil
}

// ResolveConfig resolves the active configuration for Torana.
// If storePath exists, it loads and returns the managed store (ignoring seedPath).
// If storePath does not exist, it loads seedPath (merging with defaults if needed),
// saves the result to storePath to materialize the store (setting Managed: true),
// and returns the config. The seed file is never modified.
func ResolveConfig(seedPath, storePath string) (Config, error) {
	if _, err := os.Stat(storePath); err == nil {
		return Load(storePath)
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("checking managed store %q: %w", storePath, err)
	}

	cfg, err := Load(seedPath)
	if err != nil {
		return cfg, fmt.Errorf("loading seed config %q: %w", seedPath, err)
	}

	if err := Save(storePath, cfg); err != nil {
		return cfg, fmt.Errorf("materializing managed store %q: %w", storePath, err)
	}

	// Save persists Managed:true; reflect that in the returned config so the
	// in-memory view the caller holds agrees with what is now on disk.
	cfg.Managed = true
	return cfg, nil
}

