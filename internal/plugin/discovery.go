package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/wasm"
	"github.com/torana-edge/torana-edge/pkg/pb"
	"google.golang.org/protobuf/proto"
)

// ============================================================================
// Manifest
// ============================================================================

type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Hook struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

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

type PluginBundle struct {
	Manifest  PluginManifest
	WASMBytes []byte
}

func DiscoverPlugins(pluginsDir string) ([]PluginBundle, error) {
	if pluginsDir == "" {
		pluginsDir = "./plugins"
	}
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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
	warnIfStale(dir, wasmPath, manifest.Name)
	return &PluginBundle{Manifest: manifest, WASMBytes: wBytes}, nil
}

// warnIfStale logs a warning when plugin.wasm is older than any Go source
// in the plugin directory. Stale binaries silently running outdated logic
// caused a production incident — binaries are build artifacts (`make plugins`).
func warnIfStale(dir, wasmPath, name string) {
	wasmInfo, err := os.Stat(wasmPath)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(wasmInfo.ModTime()) {
			log.Printf("[plugin] %s: plugin.wasm is older than %s — rebuild with 'make plugins'", name, e.Name())
			return
		}
	}
}

// ============================================================================
// Pipeline
// ============================================================================

type PluginPipeline struct {
	plugins []*loadedPlugin
	runtime *wasm.Runtime
	wg      sync.WaitGroup // active requests using this pipeline
}

type loadedPlugin struct {
	manifest PluginManifest
	plugin   *wasm.Plugin
}

func NewPipeline(runtime *wasm.Runtime, config PluginConfig) (*PluginPipeline, error) {
	return reloadPipeline(runtime, config)
}

func reloadPipeline(runtime *wasm.Runtime, config PluginConfig) (*PluginPipeline, error) {
	bundles, err := DiscoverPlugins(config.Dir)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]PluginBundle)
	for _, b := range bundles {
		byName[b.Manifest.Name] = b
	}
	var loaded []*loadedPlugin
	order := config.Order
	if len(order) == 0 {
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
		var grants []string
		for _, p := range bundle.Manifest.Permissions {
			grants = append(grants, p.Name)
		}
		pl.SetGrants(grants)
		if raw, ok := config.Config[name]; ok && len(raw) > 0 {
			pl.SetConfig(string(raw))
		}
		// Validate that every declared hook is actually exported by the WASM module.
		if err := pl.ValidateHooks(context.Background(), hookNames(bundle.Manifest.Hooks)); err != nil {
			log.Printf("[plugin] %s: hook validation failed: %v — skipping", name, err)
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

func (pp *PluginPipeline) Acquire() { pp.wg.Add(1) }
func (pp *PluginPipeline) Release() { pp.wg.Done() }

// Len returns the number of successfully loaded plugins.
func (pp *PluginPipeline) Len() int { return len(pp.plugins) }

// EndRequest drops all request-scoped plugin state for a finished request.
func (pp *PluginPipeline) EndRequest(reqID uint64) { pp.runtime.EndRequest(reqID) }

// HasGrant reports whether any loaded plugin declares the given permission.
func (pp *PluginPipeline) HasGrant(perm string) bool {
	for _, lp := range pp.plugins {
		for _, p := range lp.manifest.Permissions {
			if p.Name == perm {
				return true
			}
		}
	}
	return false
}

// DrainAndClose waits for active requests then closes the runtime.
func (pp *PluginPipeline) DrainAndClose() {
	pp.wg.Wait()
	if err := pp.runtime.Close(); err != nil {
		log.Printf("[plugin] close old runtime: %v", err)
	}
}

// RunOnChatRequest calls every plugin that implements run_before_request.
func (pp *PluginPipeline) RunBeforeRequest(ctx context.Context, reqID uint64, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	pp.Acquire()
	defer pp.Release()

	pbReq := pbconv.ToPBChatRequest(chat)
	reqBytes, err := proto.Marshal(pbReq)
	if err != nil {
		return chat, err
	}

	resultBytes := reqBytes
	modified := false
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_before_request") {
			continue
		}
		var outBytes []byte
		if err := lp.plugin.CallRequest(ctx, "run_before_request", reqID, resultBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_before_request: %v", lp.manifest.Name, err)
			continue
		}
		if len(outBytes) > 0 {
			resultBytes = outBytes
			modified = true
		}
	}

	if !modified {
		// No plugin produced output — skip the pb round-trip entirely.
		return chat, nil
	}
	var resReq pb.ChatRequest
	if err := proto.Unmarshal(resultBytes, &resReq); err != nil {
		return chat, err
	}
	return pbconv.FromPBChatRequest(&resReq), nil
}

// RunAfterResponse calls every plugin that implements run_after_response.
func (pp *PluginPipeline) RunAfterResponse(ctx context.Context, reqID uint64, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	pp.Acquire()
	defer pp.Release()

	pbReq := pbconv.ToPBChatRequest(chat)
	reqBytes, err := proto.Marshal(pbReq)
	if err != nil {
		return chat, err
	}

	resultBytes := reqBytes
	modified := false
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_after_response") {
			continue
		}
		var outBytes []byte
		if err := lp.plugin.CallRequest(ctx, "run_after_response", reqID, resultBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_after_response: %v", lp.manifest.Name, err)
			continue
		}
		if len(outBytes) > 0 {
			resultBytes = outBytes
			modified = true
		}
	}

	if !modified {
		// No plugin produced output — skip the pb round-trip entirely.
		return chat, nil
	}
	var resReq pb.ChatRequest
	if err := proto.Unmarshal(resultBytes, &resReq); err != nil {
		return chat, err
	}
	return pbconv.FromPBChatRequest(&resReq), nil
}

// RunOnStreamChunk calls every plugin that implements run_on_stream_chunk.
//
// Each plugin sees every event produced by the previous plugin in the chain
// and returns a StreamEventResult per event: a zero-length return passes the
// event through unchanged; handled=true splices in its events (empty =
// suppress, one = replace, many = fan-out). The final event set replaces the
// input chunk in the stream — possibly empty.
func (pp *PluginPipeline) RunOnStreamChunk(ctx context.Context, reqID uint64, chunk *engine.StreamEvent) ([]engine.StreamEvent, error) {
	pp.Acquire()
	defer pp.Release()

	current := []*pb.StreamEvent{pbconv.ToPBStreamEvent(chunk)}

	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_on_stream_chunk") {
			continue
		}
		next := make([]*pb.StreamEvent, 0, len(current))
		for _, ev := range current {
			evBytes, err := proto.Marshal(ev)
			if err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk marshal: %v", lp.manifest.Name, err)
				next = append(next, ev)
				continue
			}
			var outBytes []byte
			if err := lp.plugin.CallRequest(ctx, "run_on_stream_chunk", reqID, evBytes, &outBytes); err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: %v", lp.manifest.Name, err)
				next = append(next, ev)
				continue
			}
			if len(outBytes) == 0 {
				// Passthrough: plugin did not handle this event.
				next = append(next, ev)
				continue
			}
			var res pb.StreamEventResult
			if err := proto.Unmarshal(outBytes, &res); err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk unmarshal: %v", lp.manifest.Name, err)
				next = append(next, ev)
				continue
			}
			if !res.Handled {
				next = append(next, ev)
				continue
			}
			next = append(next, res.Events...)
		}
		current = next
	}

	out := make([]engine.StreamEvent, 0, len(current))
	for _, ev := range current {
		out = append(out, *pbconv.FromPBStreamEvent(ev))
	}
	return out, nil
}

// ============================================================================
// Plugin config
// ============================================================================

type PluginConfig struct {
	Dir    string                     `json:"dir"`
	Order  []string                   `json:"order"`
	Config map[string]json.RawMessage `json:"config"`
}

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

// ============================================================================
// Hot-Reload (fsnotify)
// ============================================================================

// WatchPlugins starts a file watcher on the plugins directory. When a
// .wasm or plugin.json file changes (or is removed), it calls reloadFn with
// a freshly built pipeline. The reloadFn should atomically swap the active
// pipeline.
//
// configFn is consulted at reload time so config hot-reloads (plugin order,
// per-plugin config) take effect without restarting the watcher. runtimeFn
// builds each reload's runtime — the caller wires host callbacks (offload,
// savings) there; a bare runtime would silently lose them.
func WatchPlugins(ctx context.Context, dir string, configFn func() PluginConfig, runtimeFn func() *wasm.Runtime, reloadFn func(pipeline *PluginPipeline)) error {
	if dir == "" {
		dir = "./plugins"
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}

	// Watch the plugins directory and all subdirectories recursively.
	addRecursive := func(root string) {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				w.Add(path)
			}
			return nil
		})
	}
	addRecursive(dir)

	go func() {
		defer w.Close()

		// Debounce: batch rapid changes.
		var debounceTimer *time.Timer
		const debounce = 500 * time.Millisecond

		for {
			select {
			case <-ctx.Done():
				return

			case event, ok := <-w.Events:
				if !ok {
					return
				}
				// Handle newly created directories for recursive watching.
				if event.Op&fsnotify.Create == fsnotify.Create {
					if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
						addRecursive(event.Name)
						continue
					}
				}

				// Only reload on .wasm or plugin.json changes.
				name := filepath.Base(event.Name)
				if name != "plugin.wasm" && name != "plugin.json" {
					continue
				}
				// Remove/Rename included: deleting a plugin must unload it.
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}

				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(debounce, func() {
					log.Printf("[plugin] file change detected: %s — reloading", event.Name)
					newRT := runtimeFn()
					pp, err := reloadPipeline(newRT, configFn())
					if err != nil {
						log.Printf("[plugin] reload failed: %v", err)
						newRT.Close()
						return
					}
					log.Printf("[plugin] hot-reload complete: %d plugins", len(pp.plugins))
					reloadFn(pp)
				})

			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[plugin] fsnotify error: %v", err)
			}
		}
	}()

	return nil
}
