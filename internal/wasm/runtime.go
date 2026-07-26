package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/metrics"
)

// ============================================================================
// Plugin — WASM module with instance pooling and permission enforcement
// ============================================================================

const (
	// defaultPoolSize is deliberately small. A Go/WASI plugin can consume several
	// MiB per instance, so the previous 100-slot pool made a single plugin able to
	// reserve far more memory than a personal proxy should spend.
	defaultPoolSize = 4
	// defaultCallTimeout bounds every untrusted guest call, including _initialize.
	defaultCallTimeout = 5 * time.Second
	// defaultMemoryLimitPages caps a plugin at 64 MiB of Wasm linear memory.
	// A page is 64 KiB. This is high enough for the current Go/WASI plugins while
	// preventing an absent module maximum from becoming wazero's 4 GiB default.
	defaultMemoryLimitPages uint32 = 1024
)

// RuntimeOptions bounds resources used by every plugin loaded in a Runtime.
// Zero values select conservative defaults, so existing callers retain working
// behavior without inheriting unbounded resource use.
type RuntimeOptions struct {
	// PoolSize is the maximum number of idle instances retained per plugin.
	PoolSize int
	// CallTimeout is the maximum duration of one guest function invocation.
	CallTimeout time.Duration
	// MemoryLimitPages is the maximum Wasm linear memory per instance (64 KiB/page).
	MemoryLimitPages uint32
}

func defaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{
		PoolSize:         defaultPoolSize,
		CallTimeout:      defaultCallTimeout,
		MemoryLimitPages: defaultMemoryLimitPages,
	}
}

func normalizeRuntimeOptions(options RuntimeOptions) RuntimeOptions {
	defaults := defaultRuntimeOptions()
	if options.PoolSize <= 0 {
		options.PoolSize = defaults.PoolSize
	}
	if options.CallTimeout <= 0 {
		options.CallTimeout = defaults.CallTimeout
	}
	if options.MemoryLimitPages == 0 {
		options.MemoryLimitPages = defaults.MemoryLimitPages
	}
	// Wazero panics for a value above the WebAssembly maximum. Clamp here so a
	// configuration mistake cannot crash the proxy at startup.
	if options.MemoryLimitPages > 65536 {
		options.MemoryLimitPages = 65536
	}
	return options
}

type Plugin struct {
	name    string
	grants  map[string]bool
	config  string // per-plugin config JSON (plugins.config.<name>); "" if none
	runtime wazero.Runtime

	// compiled is the module compiled ONCE at load. Pool instances are
	// created from it via InstantiateModule, which skips the expensive
	// decode+codegen that InstantiateWithConfig(bytes) redoes on every call.
	compiled wazero.CompiledModule

	// Instance pool for concurrent request handling. slots is a semaphore over
	// both pooled and newly-created instances, making PoolSize a hard bound on
	// simultaneous guest calls rather than only an idle retention target.
	pool    chan *pluginInstance
	slots   chan struct{}
	poolMu  sync.Mutex
	stateMu sync.RWMutex

	poolSize    int
	callTimeout time.Duration

	instanceCount uint64
}

type pluginInstance struct {
	mod        api.Module
	logEnabled bool
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) SetGrants(g []string) {
	p.stateMu.Lock()
	p.grants = make(map[string]bool)
	for _, x := range g {
		p.grants[x] = true
	}
	p.stateMu.Unlock()

	// Stdout/stderr are selected when a module is instantiated. Recycle idle
	// instances after grants change so an env.log grant is applied consistently.
	p.discardIdleInstances()
}

// SetConfig stores the plugin's config JSON blob (plugins.config.<name>),
// returned to the plugin via the env.plugin_config host call.
func (p *Plugin) SetConfig(cfg string) {
	p.stateMu.Lock()
	p.config = cfg
	p.stateMu.Unlock()
}

func (p *Plugin) pluginConfig() string {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.config
}

func (p *Plugin) hasGrant(perm string) bool {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if p.grants == nil {
		return false // fail-closed: no grants = no permissions
	}
	return p.grants[perm]
}

func (p *Plugin) discardIdleInstances() {
	for {
		select {
		case inst := <-p.pool:
			if inst != nil {
				_ = inst.mod.Close(context.Background())
			}
		default:
			return
		}
	}
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
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case inst := <-p.pool:
		if inst != nil {
			return inst, nil
		}
	default:
	}
	// Pool empty — create a new instance.
	inst, err := p.newInstance(ctx)
	if err != nil {
		<-p.slots
		return nil, err
	}
	return inst, nil
}

// release returns an instance to the pool.
func (p *Plugin) release(inst *pluginInstance) {
	defer func() { <-p.slots }()
	if inst == nil || inst.mod == nil || inst.mod.IsClosed() {
		return
	}
	if inst.logEnabled != p.hasGrant("env.log") {
		_ = inst.mod.Close(context.Background())
		return
	}
	select {
	case p.pool <- inst:
	default:
		// Pool full — close the extra instance.
		inst.mod.Close(context.Background())
	}
}

func (p *Plugin) discard(inst *pluginInstance) {
	defer func() { <-p.slots }()
	if inst != nil && inst.mod != nil {
		_ = inst.mod.Close(context.Background())
	}
}

func (p *Plugin) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.callTimeout)
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
	instanceCtx, cancel := p.callContext(ctx)
	defer cancel()
	config := wazero.NewModuleConfig().WithName(instanceName).
		WithSysWalltime().WithSysNanotime()
	// wazero defaults stdout/stderr to io.Discard. Only grant direct guest
	// output to plugins explicitly granted env.log; host env.log is gated too.
	logEnabled := p.hasGrant("env.log")
	if logEnabled {
		config = config.WithStdout(os.Stdout).WithStderr(os.Stderr)
	} else {
		config = config.WithStdout(io.Discard).WithStderr(io.Discard)
	}
	mod, err := p.runtime.InstantiateModule(instanceCtx, p.compiled, config)
	if err != nil {
		return nil, err
	}
	init := mod.ExportedFunction("_initialize")
	if init != nil {
		if _, err := init.Call(instanceCtx); err != nil {
			_ = mod.Close(context.Background())
			return nil, fmt.Errorf("wasm: %s initialize: %w", p.name, err)
		}
	}
	return &pluginInstance{mod: mod, logEnabled: logEnabled}, nil
}

// CallRequest passes byte payload to the WASM hook and returns the result.
// Uses instance pooling for concurrent request handling.
func (p *Plugin) CallRequest(ctx context.Context, hook string, reqID uint64, inBytes []byte, output *[]byte) error {
	if output == nil {
		return fmt.Errorf("wasm: %s nil output", p.name)
	}
	if uint64(len(inBytes)) > math.MaxUint32 {
		return fmt.Errorf("wasm: %s input exceeds 32-bit Wasm memory", p.name)
	}
	// Carry the request ID into host functions (wazero propagates the
	// fn.Call context) so meta state is scoped per request.
	ctx = context.WithValue(ctx, reqIDKey{}, reqID)
	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	// Acquire an instance from the pool.
	inst, err := p.acquire(callCtx)
	if err != nil {
		return err
	}
	healthy := false
	defer func() {
		if healthy {
			p.release(inst)
		} else {
			p.discard(inst)
		}
	}()

	mod := inst.mod

	fn := mod.ExportedFunction(hook)
	if fn == nil {
		healthy = true
		return nil
	}

	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		return fmt.Errorf("wasm: %s missing alloc", p.name)
	}
	deallocFn := mod.ExportedFunction("dealloc")

	// Allocate in WASM linear memory.
	r, err := allocFn.Call(callCtx, uint64(len(inBytes)))
	if err != nil {
		return err
	}
	if len(r) != 1 || r[0] > math.MaxUint32 {
		return fmt.Errorf("wasm: %s alloc returned invalid pointer", p.name)
	}
	inPtr := uint32(r[0])
	if !writeMemory(mod.Memory(), inPtr, inBytes) {
		return fmt.Errorf("wasm: %s alloc returned out-of-bounds input pointer", p.name)
	}

	// Call hook.
	ret, err := fn.Call(callCtx, reqID, uint64(inPtr), uint64(len(inBytes)))
	if deallocFn != nil {
		if _, deallocErr := deallocFn.Call(callCtx, uint64(inPtr), uint64(len(inBytes))); deallocErr != nil && err == nil {
			err = fmt.Errorf("wasm: %s dealloc input: %w", p.name, deallocErr)
		}
	}
	if err != nil {
		return err
	}

	// Read result.
	if len(ret) != 1 {
		return fmt.Errorf("wasm: %s hook %q returned invalid ABI result", p.name, hook)
	}
	if ret[0] != 0 {
		v := ret[0]
		outPtr := uint32(v >> 32)
		outLen := uint32(v & 0xFFFFFFFF)
		if outLen == 0 {
			return fmt.Errorf("wasm: %s hook %q returned a non-zero pointer with zero length", p.name, hook)
		}
		b, ok := readMemory(mod.Memory(), outPtr, outLen)
		if !ok {
			return fmt.Errorf("wasm: %s hook %q returned out-of-bounds result", p.name, hook)
		}
		res := append([]byte(nil), b...)
		if deallocFn != nil {
			if _, err := deallocFn.Call(callCtx, uint64(outPtr), uint64(outLen)); err != nil {
				return fmt.Errorf("wasm: %s dealloc result: %w", p.name, err)
			}
		}
		*output = res
	}
	healthy = true
	return nil
}

func memoryRangeOK(memory api.Memory, ptr, length uint32) bool {
	if memory == nil {
		return false
	}
	return uint64(ptr)+uint64(length) <= uint64(memory.Size())
}

func writeMemory(memory api.Memory, ptr uint32, b []byte) bool {
	if uint64(len(b)) > math.MaxUint32 || !memoryRangeOK(memory, ptr, uint32(len(b))) {
		return false
	}
	return memory.Write(ptr, b)
}

func readMemory(memory api.Memory, ptr, length uint32) ([]byte, bool) {
	if !memoryRangeOK(memory, ptr, length) {
		return nil, false
	}
	return memory.Read(ptr, length)
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
	options RuntimeOptions
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
	// OffloadResultFunc exposes provider/model/usage while preserving the old
	// OffloadFunc contract for external embedders.
	OffloadResultFunc func(ctx context.Context, payloadJSON string) (economics.OffloadResult, error)

	// SavingsFunc handles torana_record_savings host calls (compaction
	// byte savings reported by plugins), attributed to the calling plugin.
	// Set by the server.
	SavingsFunc func(plugin string, originalBytes, finalBytes int64)
	// CompactionReportFunc receives the richer, batch-aware savings ABI. When
	// set it supersedes SavingsFunc; the old callback remains for embedders and
	// tests using the original two-field contract.
	CompactionReportFunc func(ctx context.Context, plugin string, report economics.CompactionReport)
	// EvaluateCompactionFunc performs the optional operator-priced economic
	// gate before a plugin mutates history.
	EvaluateCompactionFunc func(ctx context.Context, report economics.CompactionReport) economics.CompactionDecision
	// RequestMutationFunc observes a plugin's returned canonical request. It is
	// used by the host to make an earlier routing verdict visible to later
	// plugins' economic-gate calls.
	RequestMutationFunc func(ctx context.Context, requestPB []byte)

	// OriginalRequestFunc returns the pristine pre-pipeline request as pb
	// bytes for env.original_request (empty when unavailable). Set by the
	// server; grant-gated at dispatch.
	OriginalRequestFunc func(ctx context.Context) []byte

	// OriginalResponseFunc returns the raw upstream response body for
	// env.original_response (empty when unavailable — e.g. streaming
	// responses, which are never buffered). Set by the server.
	OriginalResponseFunc func(ctx context.Context) []byte

	// PluginCounterFunc handles torana_plugin_counter host calls — plugins
	// increment named counters that appear in the /stats response.
	// Set by the server.
	PluginCounterFunc func(plugin string, counter string, delta int64)

	// StateGetFunc and StateSetFunc back env.state_get / env.state_set:
	// durable, plugin-namespaced storage that survives a restart. Unlike the
	// cache these are private per plugin, and unlike meta they outlive the
	// request. Nil when no data directory is configured.
	StateGetFunc  func(plugin, key string) (string, bool)
	StateSetFunc  func(plugin, key, value string) error
	StateKeysFunc func(plugin string) []string

	// CachePricingFunc answers torana_cache_pricing: given a provider and
	// model, what the prompt cache costs and how long it lives. Data, not a
	// decision — the host holds the prices, the plugin holds the policy.
	CachePricingFunc func(ctx context.Context, payloadJSON string) string

	// SendRequestFunc backs torana_send_request: a plugin-originated provider
	// request. The plugin name is passed so the host can meter it against that
	// plugin's budget and attribute it in the feed — spend a plugin initiates
	// must still be traceable to the plugin that initiated it.
	SendRequestFunc func(ctx context.Context, plugin, payloadJSON string) string
}

// ObserveRequestMutation forwards a defensive copy to the host callback.
func (r *Runtime) ObserveRequestMutation(ctx context.Context, requestPB []byte) {
	if r.RequestMutationFunc == nil || len(requestPB) == 0 {
		return
	}
	b := append([]byte(nil), requestPB...)
	r.RequestMutationFunc(ctx, b)
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
	r := newRuntime(ctx, cache.NewLocalCache(cacheTTL), true, defaultRuntimeOptions())
	return r
}

// NewRuntimeWithOptions creates a runtime with explicit resource limits.
func NewRuntimeWithOptions(ctx context.Context, options RuntimeOptions) *Runtime {
	return newRuntime(ctx, cache.NewLocalCache(cacheTTL), true, normalizeRuntimeOptions(options))
}

// NewRuntimeWithCache builds a Runtime on a caller-owned cache store. The
// store is shared across runtime instances — plugin cache state survives
// hot-reload swaps (each reload builds a fresh runtime) and, with a Redis
// store, restarts and multiple proxy instances. Close does NOT close a
// shared store; its owner does.
func NewRuntimeWithCache(ctx context.Context, store cache.Store) *Runtime {
	return newRuntime(ctx, store, false, defaultRuntimeOptions())
}

// NewRuntimeWithCacheAndOptions is the cache-sharing equivalent of
// NewRuntimeWithOptions.
func NewRuntimeWithCacheAndOptions(ctx context.Context, store cache.Store, options RuntimeOptions) *Runtime {
	return newRuntime(ctx, store, false, normalizeRuntimeOptions(options))
}

func newRuntime(ctx context.Context, store cache.Store, ownsCache bool, options RuntimeOptions) *Runtime {
	options = normalizeRuntimeOptions(options)
	r := &Runtime{
		ctx: ctx,
		runtime: wazero.NewRuntimeWithConfig(ctx,
			wazero.NewRuntimeConfig().
				WithCompilationCache(wasmCompilationCache).
				WithMemoryLimitPages(options.MemoryLimitPages).
				WithCloseOnContextDone(true)),
		plugins:   make(map[string]*Plugin),
		meta:      make(map[uint64]map[string]string),
		cache:     store,
		ownsCache: ownsCache,
		options:   options,
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
		name:        name,
		compiled:    compiled,
		runtime:     r.runtime,
		pool:        make(chan *pluginInstance, r.options.PoolSize),
		slots:       make(chan struct{}, r.options.PoolSize),
		poolSize:    r.options.PoolSize,
		callTimeout: r.options.CallTimeout,
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
	log.Printf("[wasm] loaded plugin %s (pool=%d timeout=%s memory_limit=%dMiB)", name, p.poolSize, p.callTimeout, int(r.options.MemoryLimitPages)/16)
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
		pluginName := pluginNameOf(mod)
		r.mu.RLock()
		p := r.plugins[pluginName]
		r.mu.RUnlock()
		if p == nil || !p.hasGrant("env.log") {
			log.Printf("[wasm] permission denied: %s tried env.log", mod.Name())
			return
		}
		msg := readStr(mod, ptr, length)
		if level == 0 {
			log.Printf("[plugin %s debug] %s", pluginName, msg)
		} else {
			log.Printf("[plugin %s] %s", pluginName, msg)
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
		case "env.state_set":
			// Durable, plugin-private, survives a restart. The plugin name
			// comes from the module, never the payload, so one plugin cannot
			// write into another's namespace.
			var kv struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			}
			if err := json.Unmarshal([]byte(args), &kv); err != nil {
				res = `{"status":"error","message":"invalid payload"}`
			} else if r.StateSetFunc == nil {
				res = `{"status":"error","message":"durable plugin state is not configured"}`
			} else {
				value := ""
				switch v := kv.Value.(type) {
				case nil:
					// Explicit null deletes, same as an empty string.
				case string:
					value = v
				default:
					b, _ := json.Marshal(v)
					value = string(b)
				}
				if err := r.StateSetFunc(pluginName, kv.Key, value); err != nil {
					res = fmt.Sprintf(`{"status":"error","message":%q}`, err.Error())
				} else {
					res = `{"status":"ok"}`
				}
			}
		case "env.state_get":
			if r.StateGetFunc == nil {
				res = ""
			} else {
				res, _ = r.StateGetFunc(pluginName, args)
			}
		case "env.state_keys":
			if r.StateKeysFunc == nil {
				res = "[]"
			} else {
				b, _ := json.Marshal(r.StateKeysFunc(pluginName))
				res = string(b)
			}
		case "env.now":
			// WASI preview1 gives the guest no clock, deliberately. Plugins
			// that reason about elapsed time — cache lifetimes, deadlines,
			// rate windows — otherwise have no way to ask.
			//
			// This is a capability, not a convenience: a plugin that writes the
			// result into a request makes its output non-deterministic, which
			// busts the provider's prompt cache on every turn. See
			// PLUGIN_SEMANTICS §6.
			res = strconv.FormatInt(time.Now().UnixMilli(), 10)
		case "torana_send_request":
			if r.SendRequestFunc == nil {
				res = `{"status":"error","message":"plugin egress is not configured"}`
			} else {
				res = r.SendRequestFunc(ctx, pluginName, args)
			}
		case "torana_cache_pricing":
			if r.CachePricingFunc == nil {
				res = `{"status":"unavailable","reason":"pricing_unconfigured"}`
			} else {
				res = r.CachePricingFunc(ctx, args)
			}
		case "env.plugin_config":
			// Return this plugin's config blob (plugins.config.<name>).
			res = p.pluginConfig()
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
			var report economics.CompactionReport
			if err := json.Unmarshal([]byte(args), &report); err != nil {
				res = `{"status":"error","message":"invalid payload"}`
				break
			}
			report.Normalize()
			if !report.Valid() {
				res = `{"status":"error","message":"invalid payload"}`
			} else if r.CompactionReportFunc != nil {
				r.CompactionReportFunc(ctx, pluginName, report)
				res = `{"status":"ok"}`
			} else if r.SavingsFunc != nil {
				r.SavingsFunc(pluginName, report.OriginalBytes, report.FinalBytes)
				res = `{"status":"ok"}`
			} else {
				res = `{"status":"error","message":"savings tracking not configured"}`
			}
		case "torana_plugin_counter":
			var counter struct {
				Counter string `json:"counter"`
				Delta   int64  `json:"delta"`
			}
			if err := json.Unmarshal([]byte(args), &counter); err != nil || counter.Counter == "" {
				res = `{"status":"error","message":"invalid payload"}`
			} else if r.PluginCounterFunc != nil {
				r.PluginCounterFunc(pluginName, counter.Counter, counter.Delta)
				res = `{"status":"ok"}`
			} else {
				res = `{"status":"error","message":"plugin counter tracking not configured"}`
			}
		case "torana_evaluate_compaction":
			var report economics.CompactionReport
			if err := json.Unmarshal([]byte(args), &report); err != nil {
				res = `{"apply":false,"reason":"invalid_payload"}`
				break
			}
			report.Normalize()
			if !report.Valid() {
				res = `{"apply":false,"reason":"invalid_payload"}`
			} else if r.EvaluateCompactionFunc == nil {
				res = `{"apply":false,"reason":"pricing_unconfigured"}`
			} else {
				decision := r.EvaluateCompactionFunc(ctx, report)
				payload, _ := json.Marshal(decision)
				res = string(payload)
			}
		case "torana_offload_completion":
			if r.OffloadResultFunc != nil {
				result, err := r.OffloadResultFunc(ctx, args)
				if err != nil {
					res = fmt.Sprintf(`{"status":"error","message":%q}`, err.Error())
				} else {
					payload, _ := json.Marshal(struct {
						Status string `json:"status"`
						economics.OffloadResult
					}{Status: "ok", OffloadResult: result})
					res = string(payload)
				}
			} else if r.OffloadFunc != nil {
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
	b, ok := readMemory(mod.Memory(), ptr, length)
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

	if res[0] > math.MaxUint32 {
		log.Printf("[wasm] writeStr: alloc returned invalid pointer in module %s", mod.Name())
		return 0
	}
	ptr := uint32(res[0])
	if !writeMemory(mod.Memory(), ptr, b) {
		log.Printf("[wasm] writeStr: alloc returned out-of-bounds pointer in module %s", mod.Name())
		return 0
	}
	return uint64(ptr)<<32 | uint64(len(b))
}
