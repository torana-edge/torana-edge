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
	"github.com/torana-edge/torana-edge/sdk/pb"
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

type ConfigField struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"`              // "string" | "number" | "boolean" | "enum"
	Label   string   `json:"label"`
	Default any      `json:"default,omitempty"`
	Options []string `json:"options,omitempty"` // enum only
	Help    string   `json:"help,omitempty"`
}

type ConfigSchema struct {
	Fields []ConfigField `json:"fields"`
}

// ============================================================================
// Discovery
// ============================================================================

type PluginBundle struct {
	Manifest  PluginManifest
	WASMBytes []byte
	Schema    *ConfigSchema
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
	schemaPath := filepath.Join(dir, "schema.json")
	var schema *ConfigSchema
	if sBytes, err := os.ReadFile(schemaPath); err == nil {
		var s ConfigSchema
		if err := json.Unmarshal(sBytes, &s); err != nil {
			return nil, fmt.Errorf("parse schema: %w", err)
		}
		schema = &s
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	warnIfStale(dir, wasmPath, manifest.Name)
	return &PluginBundle{Manifest: manifest, WASMBytes: wBytes, Schema: schema}, nil
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

	mu        sync.Mutex
	active    int
	draining  bool
	drained   chan struct{}
	closed    chan struct{}
	drainOnce sync.Once
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
	// Enforce ordering constraint: route-capable plugins (env.route_request)
	// must precede compaction economic-gate plugins (env.host_call.torana_evaluate_compaction).
	var seenCompactionGate bool
	for _, name := range order {
		bundle, ok := byName[name]
		if !ok {
			continue
		}
		var hasRoute, hasCompactionGate bool
		for _, p := range bundle.Manifest.Permissions {
			if p.Name == "env.route_request" {
				hasRoute = true
			}
			if p.Name == "env.host_call.torana_evaluate_compaction" {
				hasCompactionGate = true
			}
		}
		if hasRoute && seenCompactionGate {
			return nil, fmt.Errorf("ordering constraint violation: route-capable plugin %q (grant env.route_request) must precede compaction economic-gate plugins (grant env.host_call.torana_evaluate_compaction)", name)
		}
		if hasCompactionGate {
			seenCompactionGate = true
		}
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
		// SECURITY BOUNDARY SECURITY NOTE:
		// Grants are read directly from the plugin's own self-declared manifest
		// (bundle.Manifest.Permissions). This is a policy declaration for runtime auditability
		// and developer convenience, NOT a secure security boundary against untrusted plugin
		// artifacts. Since any plugin can request any permission in its manifest, the system
		// does not enforce third-party validation or sandbox boundaries beyond wazero's basic
		// WASM environment. Administrative review of plugins before deployment is required.
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
		// run_after_response mutations are applied on the non-streaming JSON
		// path but are OBSERVATIONAL on streaming responses (the stream is
		// already written when the hook fires) — see docs/PLUGIN_IMPLEMENTATION_
		// GUIDE.md §5. Warn once at load so a plugin that expects to rewrite
		// streamed responses isn't silently a no-op.
		if hasHook(bundle.Manifest, "run_after_response") {
			log.Printf("[plugin] %s: run_after_response mutations are observational on streaming responses (metrics/audit OK; response rewrites are dropped mid-stream)", name)
		}
	}
	return &PluginPipeline{
		plugins: loaded,
		runtime: runtime,
		drained: make(chan struct{}),
		closed:  make(chan struct{}),
	}, nil
}

// TryAcquire pins a pipeline for a new HTTP request. Once draining begins it
// rejects new work, preventing a request that observed the old atomic pointer
// from acquiring a runtime after that runtime has been closed.
func (pp *PluginPipeline) TryAcquire() bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.draining {
		return false
	}
	pp.active++
	return true
}

// Acquire/Release protect individual hook calls. A request that already owns
// a pipeline may make nested hook calls while it is draining, so Acquire is
// deliberately not gated by draining; new request admission uses TryAcquire.
func (pp *PluginPipeline) Acquire() {
	pp.mu.Lock()
	pp.active++
	pp.mu.Unlock()
}

func (pp *PluginPipeline) Release() {
	pp.mu.Lock()
	pp.active--
	if pp.active == 0 && pp.draining {
		close(pp.drained)
	}
	pp.mu.Unlock()
}

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

// DrainAndClose rejects future request admission, waits for active work, then
// closes the runtime exactly once. It does not use WaitGroup because Add racing
// with Wait at a zero count can close the runtime before a request is pinned.
func (pp *PluginPipeline) DrainAndClose() {
	pp.drainOnce.Do(func() {
		pp.mu.Lock()
		pp.draining = true
		if pp.active == 0 {
			close(pp.drained)
		}
		pp.mu.Unlock()
		go func() {
			<-pp.drained
			if err := pp.runtime.Close(); err != nil {
				log.Printf("[plugin] close old runtime: %v", err)
			}
			close(pp.closed)
		}()
	})
	<-pp.closed
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

// ErrServeHTTPForbidden is returned by RunOnHTTPRequest when the named plugin
// exists and declares the run_on_http_request hook but does NOT hold the
// env.serve_http permission. The proxy route handler maps this to 403.
var ErrServeHTTPForbidden = fmt.Errorf("plugin does not hold env.serve_http permission")

// RunOnHTTPRequest dispatches an HTTP request to a single named plugin's
// run_on_http_request hook. It is used by the /_torana/plugin/<name>/* proxy
// route so plugins can serve their own HTTP UI/API namespace.
//
// Return values:
//
//	(nil, nil)                   — plugin not found, or does not declare
//	                               run_on_http_request; caller should 404.
//	(nil, ErrServeHTTPForbidden) — plugin exists and has the hook but lacks
//	                               the env.serve_http grant; caller → 403.
//	(*HttpResponse, nil)         — plugin returned a response; caller writes it.
//	(nil, other error)           — internal dispatch error; caller → 503.
//
// httpReq is built directly from net/http — it does not cross the engine IR.
func (pp *PluginPipeline) RunOnHTTPRequest(ctx context.Context, reqID uint64, pluginName string, httpReq *pb.HttpRequest) (*pb.HttpResponse, error) {
	pp.Acquire()
	defer pp.Release()

	// Find the named plugin.
	var target *loadedPlugin
	for _, lp := range pp.plugins {
		if lp.manifest.Name == pluginName {
			target = lp
			break
		}
	}
	if target == nil {
		return nil, nil // not found
	}

	// Plugin must declare the hook.
	if !hasHook(target.manifest, "run_on_http_request") {
		return nil, nil // not serving HTTP
	}

	// Enforce env.serve_http capability.
	if !target.plugin.HasGrant("env.serve_http") {
		return nil, ErrServeHTTPForbidden
	}

	inBytes, err := proto.Marshal(httpReq)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: marshal http request: %w", pluginName, err)
	}

	var outBytes []byte
	if err := target.plugin.CallRequest(ctx, "run_on_http_request", reqID, inBytes, &outBytes); err != nil {
		return nil, fmt.Errorf("plugin %s: run_on_http_request: %w", pluginName, err)
	}

	// Zero-length return → plugin did not handle the request.
	if len(outBytes) == 0 {
		return nil, nil
	}

	var resp pb.HttpResponse
	if err := proto.Unmarshal(outBytes, &resp); err != nil {
		return nil, fmt.Errorf("plugin %s: unmarshal http response: %w", pluginName, err)
	}

	// Explicit handled flag required — see proto comment.
	if !resp.Handled {
		return nil, nil
	}

	return &resp, nil
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
func WatchPlugins(ctx context.Context, dir string, configFn func() PluginConfig, runtimeFn func() *wasm.Runtime, reloadFn func(pipeline *PluginPipeline), done func()) error {
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
		if done != nil {
			defer done()
		}

		// Debounce in this goroutine rather than time.AfterFunc. This serializes
		// reloads: an older, slow reload can never overwrite a newer one.
		var debounceTimer *time.Timer
		var debounceC <-chan time.Time
		const debounce = 500 * time.Millisecond

		for {
			select {
			case <-ctx.Done():
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				return

			case <-debounceC:
				debounceTimer = nil
				debounceC = nil
				if ctx.Err() != nil {
					return
				}
				newRT := runtimeFn()
				pp, err := reloadPipeline(newRT, configFn())
				if err != nil {
					log.Printf("[plugin] reload failed: %v", err)
					newRT.Close()
					continue
				}
				if ctx.Err() != nil {
					newRT.Close()
					return
				}
				log.Printf("[plugin] hot-reload complete: %d plugins", len(pp.plugins))
				reloadFn(pp)

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

				// Only reload on .wasm, plugin.json, or schema.json changes.
				name := filepath.Base(event.Name)
				if name != "plugin.wasm" && name != "plugin.json" && name != "schema.json" {
					continue
				}
				// Remove/Rename included: deleting a plugin must unload it.
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}

				if debounceTimer == nil {
					debounceTimer = time.NewTimer(debounce)
					debounceC = debounceTimer.C
				} else {
					if !debounceTimer.Stop() {
						select {
						case <-debounceTimer.C:
						default:
						}
					}
					debounceTimer.Reset(debounce)
				}

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
