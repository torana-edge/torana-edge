// Package plugin defines the WASM plugin API for Torana Edge.
// It provides plugin discovery, manifest parsing, and a pluggable
// pipeline that replaces the Go-native engine.Pipeline.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// ============================================================================
// Manifest
// ============================================================================

// Permission describes a host function a plugin requires.
type Permission struct {
	Name        string `json:"name"`        // e.g. "env.http_request"
	Description string `json:"description"` // human-readable reason
}

// Hook declares a hook point the plugin implements.
type Hook struct {
	Name     string `json:"name"`     // e.g. "on_chat_request", "on_stream_event"
	Priority int    `json:"priority"` // lower = runs first (default 0)
}

// PluginManifest describes a plugin's metadata (from plugin.json).
type PluginManifest struct {
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Hooks       []Hook       `json:"hooks"`
	Permissions []Permission `json:"permissions"`
}

// ============================================================================
// Discovery
// ============================================================================

// PluginBundle groups a parsed manifest with its WASM bytes.
type PluginBundle struct {
	Manifest  PluginManifest
	WASMBytes []byte
}

// DiscoverPlugins scans a directory for plugin bundles.
// Each bundle is a directory containing plugin.json and plugin.wasm.
func DiscoverPlugins(pluginsDir string) ([]PluginBundle, error) {
	if pluginsDir == "" {
		pluginsDir = "./plugins"
	}

	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // empty is fine
		}
		return nil, err
	}

	var bundles []PluginBundle
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pluginDir := filepath.Join(pluginsDir, e.Name())
		bundle, err := loadBundle(pluginDir)
		if err != nil {
			log.Printf("[plugin] skipping %s: %v", e.Name(), err)
			continue
		}
		bundles = append(bundles, *bundle)
	}
	return bundles, nil
}

func loadBundle(dir string) (*PluginBundle, error) {
	manifestPath := filepath.Join(dir, "plugin.json")
	mBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifest PluginManifest
	if err := json.Unmarshal(mBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	wasmPath := filepath.Join(dir, "plugin.wasm")
	wBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm: %w", err)
	}

	return &PluginBundle{Manifest: manifest, WASMBytes: wBytes}, nil
}

// ============================================================================
// Pipeline — replaces engine.Pipeline
// ============================================================================

// PluginPipeline executes WASM plugins in configured order.
// It replaces the Go-native engine.Pipeline.
type PluginPipeline struct {
	plugins []*loadedPlugin
	runtime *wasm.Runtime
}

type loadedPlugin struct {
	manifest PluginManifest
	plugin   *wasm.Plugin
}

// NewPipeline loads and orders plugins from the runtime and config.
func NewPipeline(runtime *wasm.Runtime, config PluginConfig) (*PluginPipeline, error) {
	bundles, err := DiscoverPlugins(config.Dir)
	if err != nil {
		return nil, err
	}

	// Index bundles by name.
	byName := make(map[string]PluginBundle)
	for _, b := range bundles {
		byName[b.Manifest.Name] = b
	}

	// Load plugins in configured order.
	var loaded []*loadedPlugin
	order := config.Order
	if len(order) == 0 {
		// Default order: all discovered plugins by manifest name.
		for _, b := range bundles {
			order = append(order, b.Manifest.Name)
		}
		sort.Strings(order)
	}

	for _, name := range order {
		bundle, ok := byName[name]
		if !ok {
			log.Printf("[plugin] %s not found in plugins dir, skipping", name)
			continue
		}

		pl, err := runtime.LoadPlugin(name, bundle.WASMBytes)
		if err != nil {
			log.Printf("[plugin] %s: %v — skipping", name, err)
			continue
		}

		loaded = append(loaded, &loadedPlugin{
			manifest: bundle.Manifest,
			plugin:   pl,
		})
		log.Printf("[plugin] %s ready — hooks: %v", name, hookNames(bundle.Manifest.Hooks))
	}

	return &PluginPipeline{plugins: loaded, runtime: runtime}, nil
}

// Close releases all loaded plugins.
func (pp *PluginPipeline) Close() error {
	return pp.runtime.Close()
}

// ============================================================================
// Hook execution
// ============================================================================

// RunOnChatRequest calls every plugin that implements on_chat_request.
func (pp *PluginPipeline) RunOnChatRequest(ctx context.Context, chatJSON []byte) ([]byte, error) {
	var result struct{ ChatJSON []byte }
	result.ChatJSON = chatJSON

	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "on_chat_request") {
			continue
		}
		var out struct{ ChatJSON []byte }
		if err := lp.plugin.CallRequest(ctx, "on_chat_request", map[string]any{
			"chat": string(result.ChatJSON),
		}, &out); err != nil {
			log.Printf("[plugin] %s on_chat_request: %v", lp.manifest.Name, err)
			continue
		}
		if len(out.ChatJSON) > 0 {
			result.ChatJSON = out.ChatJSON
		}
	}

	return result.ChatJSON, nil
}

// ============================================================================
// Plugin config (from config.json)
// ============================================================================

// PluginConfig represents the plugins section of config.json.
type PluginConfig struct {
	Dir    string                     `json:"dir"`    // e.g. "./plugins"
	Order  []string                   `json:"order"`  // execution order
	Config map[string]json.RawMessage `json:"config"` // per-plugin config
}

// hasHook checks if a plugin implements a given hook.
func hasHook(m PluginManifest, hook string) bool {
	for _, h := range m.Hooks {
		if h.Name == hook {
			return true
		}
	}
	return false
}

func hookNames(hooks []Hook) []string {
	var names []string
	for _, h := range hooks {
		names = append(names, h.Name)
	}
	return names
}
