package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/metrics"
)

// ============================================================================
// Plugin — WASM module with instance pooling and permission enforcement
// ============================================================================

// poolSize is the number of concurrent WASM instances per plugin kept warm in the pool.
// Requests exceeding this size will create instances on-the-fly and close them afterwards.
const poolSize = 100

type Plugin struct {
	name    string
	grants  map[string]bool
	config  string // per-plugin config JSON (plugins.config.<name>); "" if none
	runtime wazero.Runtime

	// compiled is the module compiled ONCE at load. Pool instances are
	// created from it via InstantiateModule, which skips the expensive
	// decode+codegen that InstantiateWithConfig(bytes) redoes on every call.
	compiled wazero.CompiledModule

	// Instance pool for concurrent request handling.
	pool   chan *pluginInstance
	poolMu sync.Mutex

	instanceCount uint64
}

type pluginInstance struct {
	mod api.Module
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) SetGrants(g []string) {
	p.grants = make(map[string]bool)
	for _, x := range g {
		p.grants[x] = true
	}
}

// SetConfig stores the plugin's config JSON blob (plugins.config.<name>),
// returned to the plugin via the env.plugin_config host call.
func (p *Plugin) SetConfig(cfg string) { p.config = cfg }

func (p *Plugin) hasGrant(perm string) bool {
	if p.grants == nil {
		return false // fail-closed: no grants = no permissions
	}
	return p.grants[perm]
}

// HasGrant reports whether this plugin holds the named permission grant.
// It is the exported complement of hasGrant, used by callers outside the
// wasm package (e.g. plugin.PluginPipeline.RunOnHTTPRequest).
func (p *Plugin) HasGrant(perm string) bool { return p.hasGrant(perm) }


// ValidateHooks checks that every named hook from the manifest is actually
// exported by the WASM module. Returns an error listing all missing hooks.
func (p *Plugin) ValidateHooks(ctx context.Context, hooks []string) error {
	inst, err := p.newInstance(ctx)
	if err != nil {
		return fmt.Errorf("wasm: %s: create validation instance: %w", p.name, err)
	}
	defer inst.mod.Close(ctx)
	var missing []string
	for _, h := range hooks {
		if fn := inst.mod.ExportedFunction(h); fn == nil {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("wasm: %s: hooks not exported by module: %v", p.name, missing)
	}
	return nil
}

// acquire returns a plugin instance from the pool.
func (p *Plugin) acquire(ctx context.Context) (*pluginInstance, error) {
	select {
	case inst := <-p.pool:
		if inst != nil {
			return inst, nil
		}
	default:
	}
	// Pool empty — create a new instance.
	return p.newInstance(ctx)
}

// release returns an instance to the pool.
func (p *Plugin) release(inst *pluginInstance) {
	select {
	case p.pool <- inst:
	default:
		// Pool full — close the extra instance.
		inst.mod.Close(context.Background())
	}
}

func (p *Plugin) newInstance(ctx context.Context) (*pluginInstance, error) {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()

	// Wazero requires unique names for instances
	p.instanceCount++
	instanceName := fmt.Sprintf("%s-%d", p.name, p.instanceCount)

	// Instantiate from the already-compiled module. This avoids recompiling
	// the ~8 MB Go/WASI module on every pool instance (and on every instance
	// created on-the-fly when the pool is exhausted under load).
	mod, err := p.runtime.InstantiateModule(ctx, p.compiled,
		wazero.NewModuleConfig().WithName(instanceName).
			WithSysWalltime().WithSysNanotime().
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		return nil, err
	}
	init := mod.ExportedFunction("_initialize")
	if init != nil {
		init.Call(ctx)
	}
	return &pluginInstance{mod: mod}, nil
}

// CallRequest passes byte payload to the WASM hook and returns the result.
// Uses instance pooling for concurrent request handling.
func (p *Plugin) CallRequest(ctx context.Context, hook string, reqID uint64, inBytes []byte, output *[]byte) error {
	// Carry the request ID into host functions (wazero propagates the
	// fn.Call context) so meta state is scoped per request.
	ctx = context.WithValue(ctx, reqIDKey{}, reqID)

	// Acquire an instance from the pool.
	inst, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(inst)

	mod := inst.mod

	fn := mod.ExportedFunction(hook)
	if fn == nil {
		return nil
	}

	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		return fmt.Errorf("wasm: %s missing alloc", p.name)
	}
	deallocFn := mod.ExportedFunction("dealloc")

	// Allocate in WASM linear memory.
	r, err := allocFn.Call(ctx, uint64(len(inBytes)))
	if err != nil {
		return err
	}
	inPtr := uint32(r[0])
	mod.Memory().Write(inPtr, inBytes)

	// Call hook.
	ret, err := fn.Call(ctx, reqID, uint64(inPtr), uint64(len(inBytes)))
	if deallocFn != nil {
		deallocFn.Call(ctx, uint64(inPtr), uint64(len(inBytes)))
	}
	if err != nil {
		return err
	}

	// Read result.
	if len(ret) > 0 && ret[0] != 0 {
		v := ret[0]
		outPtr := uint32(v >> 32)
		outLen := uint32(v & 0xFFFFFFFF)
		if outPtr != 0 && outLen > 0 {
			b, ok := mod.Memory().Read(outPtr, outLen)
			if ok {
				res := make([]byte, len(b))
				copy(res, b)
				*output = res
				if deallocFn != nil {
					deallocFn.Call(ctx, uint64(outPtr), uint64(outLen))
				}
				return nil
			}
		}
	}
	return nil
}

// ============================================================================
// Runtime
// ============================================================================

// cacheTTL bounds the cross-request cache (intents, compacted results).
// Entries are keyed by tool_call_id; 15 minutes comfortably covers a
// harness resending tool results across conversation turns.
const cacheTTL = 15 * time.Minute

// reqIDKey carries the request ID through wazero's fn.Call context into
// host functions, scoping plugin meta state to a single request.
type reqIDKey struct{}

type Runtime struct {
	ctx     context.Context
	runtime wazero.Runtime
	plugins map[string]*Plugin
	mu      sync.RWMutex
	metaMu  sync.RWMutex
	// meta holds request-scoped, plugin-private state: reqID → namespaced
	// key → value. Buckets are dropped via EndRequest when a request ends.
	meta map[uint64]map[string]string
	// cache is the cross-request TTL store shared between plugins
	// (e.g. compactor writes intents, keyword_compactor reads them).
	cache cache.Store
	// ownsCache marks a runtime-private store (NewRuntime) that Close must
	// release; shared stores (NewRuntimeWithCache) outlive the runtime.
	ownsCache bool

	// OffloadFunc handles torana_offload_completion host calls.
	// Set by the server during initialization.
	OffloadFunc func(ctx context.Context, payloadJSON string) (string, error)

	// SavingsFunc handles torana_record_savings host calls (compaction
	// byte savings reported by plugins), attributed to the calling plugin.
	// Set by the server.
	SavingsFunc func(plugin string, originalBytes, finalBytes int64)

	// OriginalRequestFunc returns the pristine pre-pipeline request as pb
	// bytes for env.original_request (empty when unavailable). Set by the
	// server; grant-gated at dispatch.
	OriginalRequestFunc func(ctx context.Context) []byte

	// OriginalResponseFunc returns the raw upstream response body for
	// env.original_response (empty when unavailable — e.g. streaming
	// responses, which are never buffered). Set by the server.
	OriginalResponseFunc func(ctx context.Context) []byte
}

// wasmCompilationCache is shared by every Runtime in the process. wazero's
// optimizing compiler turns each ~8 MB Go/WASI plugin into machine code once;
// later runtimes — notably the fresh runtime built on every plugin
// hot-reload — reuse that cached artifact for unchanged modules instead of
// paying the full (and, under -race, very slow) compilation again.
var wasmCompilationCache wazero.CompilationCache

func init() {
	if dir := os.Getenv("TORANA_CI_CACHE"); dir != "" {
		if c, err := wazero.NewCompilationCacheWithDir(dir); err == nil {
			wasmCompilationCache = c
			return
		} else {
			log.Printf("[wasm] compilation cache unavailable at %q; using memory only: %v", dir, err)
		}
	}
	wasmCompilationCache = wazero.NewCompilationCache()
}

func NewRuntime(ctx context.Context) *Runtime {
	r := newRuntime(ctx, cache.NewLocalCache(cacheTTL), true)
	return r
}

// NewRuntimeWithCache builds a Runtime on a caller-owned cache store. The
// store is shared across runtime instances — plugin cache state survives
// hot-reload swaps (each reload builds a fresh runtime) and, with a Redis
// store, restarts and multiple proxy instances. Close does NOT close a
// shared store; its owner does.
func NewRuntimeWithCache(ctx context.Context, store cache.Store) *Runtime {
	return newRuntime(ctx, store, false)
}

func newRuntime(ctx context.Context, store cache.Store, ownsCache bool) *Runtime {
	r := &Runtime{
		ctx: ctx,
		runtime: wazero.NewRuntimeWithConfig(ctx,
			wazero.NewRuntimeConfig().WithCompilationCache(wasmCompilationCache)),
		plugins:   make(map[string]*Plugin),
		meta:      make(map[uint64]map[string]string),
		cache:     store,
		ownsCache: ownsCache,
	}
	wasi_snapshot_preview1.MustInstantiate(r.ctx, r.runtime)
	r.installHostFunctions()
	return r
}

func (r *Runtime) Close() error {
	if r.ownsCache {
		r.cache.Close()
	}
	return r.runtime.Close(r.ctx)
}

// EndRequest drops all plugin meta state for a finished request.
func (r *Runtime) EndRequest(reqID uint64) {
	r.metaMu.Lock()
	delete(r.meta, reqID)
	r.metaMu.Unlock()
}

// metaGet reads a request-scoped meta value.
func (r *Runtime) metaGet(reqID uint64, key string) string {
	r.metaMu.RLock()
	defer r.metaMu.RUnlock()
	return r.meta[reqID][key]
}

// metaSet writes a request-scoped meta value; empty value deletes the key
// (plugins use this convention for cleanup).
func (r *Runtime) metaSet(reqID uint64, key, value string) {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	if value == "" {
		delete(r.meta[reqID], key)
		return
	}
	bucket, ok := r.meta[reqID]
	if !ok {
		bucket = make(map[string]string)
		r.meta[reqID] = bucket
	}
	bucket[key] = value
}

// reqIDFrom extracts the request ID host calls were invoked under.
// Calls outside a request (e.g. hook validation) land in bucket 0.
func reqIDFrom(ctx context.Context) uint64 {
	id, _ := ctx.Value(reqIDKey{}).(uint64)
	return id
}

func (r *Runtime) LoadPlugin(name string, wasmBytes []byte) (*Plugin, error) {
	// Compile once here; every pool instance is then instantiated cheaply
	// from p.compiled. With the shared compilation cache (see NewRuntime),
	// a runtime built on hot-reload reuses an unchanged module's machine
	// code instead of recompiling it.
	compiled, err := r.runtime.CompileModule(r.ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("wasm: %s: compile: %w", name, err)
	}

	p := &Plugin{
		name:     name,
		compiled: compiled,
		runtime:  r.runtime,
		pool:     make(chan *pluginInstance, poolSize),
	}

	// Pre-warm the pool with one instance.
	inst, err := p.newInstance(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("wasm: %s: %w", name, err)
	}
	p.pool <- inst

	r.mu.Lock()
	r.plugins[name] = p
	r.mu.Unlock()
	log.Printf("[wasm] loaded plugin %s (pool=%d)", name, poolSize)
	return p, nil
}

// pluginNameOf strips the "-<instance>" suffix wazero module names carry.
func pluginNameOf(mod api.Module) string {
	name := mod.Name()
	if idx := strings.LastIndex(name, "-"); idx != -1 {
		return name[:idx]
	}
	return name
}

// metaKey namespaces a plugin's meta key. Meta is plugin-private state
// (fragment buffers, tool-call tracking) — without namespacing, plugins
// sharing key conventions (tool:0, frag:<id>) clobber each other.
// The shared cross-plugin channel is the cache (env.cache_*), not meta.
func metaKey(plugin, key string) string { return plugin + "\x00" + key }

func (r *Runtime) installHostFunctions() {
	env := r.runtime.NewHostModuleBuilder("env")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {
		key := metaKey(pluginNameOf(mod), readStr(mod, kPtr, kLen))
		return writeStr(ctx, mod, r.metaGet(reqIDFrom(ctx), key))
	}).Export("meta_get")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, level int32, ptr, length uint32) {
		msg := readStr(mod, ptr, length)
		if level == 0 {
			log.Printf("[plugin debug] %s", msg)
		} else {
			log.Printf("[plugin] %s", msg)
		}
	}).Export("log")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, message, fileName, lineNumber, columnNumber uint32) {
		log.Printf("[wasm] abort at line %d col %d", lineNumber, columnNumber)
	}).Export("abort")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, metricType int32, ptr, length uint32, value float64, labelsPtr, labelsLen uint32) {
		pluginName := pluginNameOf(mod)
		r.mu.RLock()
		p := r.plugins[pluginName]
		r.mu.RUnlock()
		if p == nil || !p.hasGrant("env.emit_metric") {
			log.Printf("[wasm] permission denied: %s tried env.emit_metric", mod.Name())
			return
		}
		name := readStr(mod, ptr, length)
		var labels map[string]string
		if labelsLen > 0 {
			if raw := readStr(mod, labelsPtr, labelsLen); raw != "" {
				_ = json.Unmarshal([]byte(raw), &labels)
			}
		}
		metrics.EmitPluginMetric(ctx, pluginName, name, int(metricType), value, labels)
	}).Export("emit_metric")

	// host_call — permission-enforced per-command.
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, cmdPtr, cmdLen, argsPtr, argsLen uint32) uint64 {
		cmd := readStr(mod, cmdPtr, cmdLen)
		args := readStr(mod, argsPtr, argsLen)

		// Enforce per-command permission: env.host_call.<command>
		pluginName := pluginNameOf(mod)

		r.mu.RLock()
		p := r.plugins[pluginName]
		r.mu.RUnlock()
		perm := "env.host_call"
		if cmd != "" {
			if strings.HasPrefix(cmd, "env.") {
				perm = cmd
			} else {
				perm = "env.host_call." + cmd
			}
		}
		if p == nil || !p.hasGrant(perm) {
			log.Printf("[wasm] permission denied: %s tried %s", mod.Name(), perm)
			return writeStr(ctx, mod, `{"status":"error","message":"permission denied"}`)
		}

		var res string
		switch cmd {
		case "env.meta_set":
			var kv struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			}
			if err := json.Unmarshal([]byte(args), &kv); err == nil {
				key := metaKey(pluginName, kv.Key)
				switch v := kv.Value.(type) {
				case string:
					r.metaSet(reqIDFrom(ctx), key, v)
				default:
					b, _ := json.Marshal(v)
					r.metaSet(reqIDFrom(ctx), key, string(b))
				}
				res = `{"status":"ok"}`
			} else {
				res = `{"status":"error","message":"invalid payload"}`
			}
		case "env.meta_get":
			res = r.metaGet(reqIDFrom(ctx), metaKey(pluginName, args))
		case "env.cache_set":
			var kv struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			}
			if err := json.Unmarshal([]byte(args), &kv); err == nil {
				switch v := kv.Value.(type) {
				case string:
					r.cache.Set(kv.Key, v)
				default:
					b, _ := json.Marshal(v)
					r.cache.Set(kv.Key, string(b))
				}
				res = `{"status":"ok"}`
			} else {
				res = `{"status":"error","message":"invalid payload"}`
			}
		case "env.cache_get":
			res, _ = r.cache.Get(args)
		case "env.plugin_config":
			// Return this plugin's config blob (plugins.config.<name>).
			res = p.config
			if res == "" {
				res = "{}"
			}
		case "env.original_request":
			// Pristine pre-pipeline request, pb-encoded. Empty = unavailable.
			if r.OriginalRequestFunc != nil {
				res = string(r.OriginalRequestFunc(ctx))
			}
		case "env.original_response":
			// Raw upstream response body (non-streaming only). Empty = unavailable.
			if r.OriginalResponseFunc != nil {
				res = string(r.OriginalResponseFunc(ctx))
			}
		case "torana_db_query":
			res = `{"status":"error","message":"database not configured — set plugins.config.compactor.dsn"}`
		case "torana_kms_decrypt":
			res = `{"status":"error","message":"KMS not configured — set TORANA_KMS_ENDPOINT"}`
		case "torana_record_savings":
			var pl struct {
				OriginalBytes int64 `json:"original_bytes"`
				FinalBytes    int64 `json:"final_bytes"`
			}
			if err := json.Unmarshal([]byte(args), &pl); err != nil || pl.OriginalBytes < 0 || pl.FinalBytes < 0 {
				res = `{"status":"error","message":"invalid payload"}`
			} else if r.SavingsFunc != nil {
				r.SavingsFunc(pluginName, pl.OriginalBytes, pl.FinalBytes)
				res = `{"status":"ok"}`
			} else {
				res = `{"status":"error","message":"savings tracking not configured"}`
			}
		case "torana_offload_completion":
			if r.OffloadFunc != nil {
				result, err := r.OffloadFunc(ctx, args)
				if err != nil {
					res = fmt.Sprintf(`{"status":"error","message":%q}`, err.Error())
				} else {
					res = fmt.Sprintf(`{"status":"ok","completion":%q}`, result)
				}
			} else {
				res = `{"status":"error","message":"offload not configured"}`
			}
		case "verify_virtual_key":
			res = `{"status":"error","message":"unimplemented: enterprise auth is available in torana-edge/private-nucleus"}`
		default:
			res = `{"status":"error","message":"unknown host call"}`
		}

		return writeStr(ctx, mod, res)
	}).Export("host_call")

	env.Instantiate(r.ctx)
}

func readStr(mod api.Module, ptr, length uint32) string {
	b, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(b)
}

// writeStr calls the WASM module's 'alloc' function to allocate space, then writes the string.
func writeStr(ctx context.Context, mod api.Module, s string) uint64 {
	b := []byte(s)
	if len(b) == 0 {
		return 0
	}

	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		log.Printf("[wasm] writeStr: missing alloc function in module %s", mod.Name())
		return 0
	}

	res, err := allocFn.Call(ctx, uint64(len(b)))
	if err != nil {
		log.Printf("[wasm] writeStr: alloc failed: %v", err)
		return 0
	}
	if len(res) == 0 {
		return 0
	}

	ptr := uint32(res[0])
	mod.Memory().Write(ptr, b)
	return uint64(ptr)<<32 | uint64(len(b))
}
