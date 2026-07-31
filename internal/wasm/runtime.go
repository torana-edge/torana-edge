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
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// permissionDeniedJSON is what a guest receives when it calls a host function
// it was not granted.
//
// It is a WIRE CONSTANT, not an implementation detail: every SDK matches it
// verbatim to tell a refusal from an ordinary empty result, and
// already-published plugin binaries cannot be recompiled from this repository.
// Changing the bytes breaks them silently — the plugin carries on as though the
// call had succeeded.
//
// Named rather than repeated so a second denial site cannot drift from the
// first.
const permissionDeniedJSON = `{"status":"error","message":"permission denied"}`

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
	name   string
	grants map[string]bool
	// hooks is the guest's supported_hooks bitmap, read once at validation.
	// Dispatch consults it instead of probing for a per-hook export, which no
	// longer exists. Guarded by stateMu with the other mutable fields.
	hooks   pbv2.HookBitmap
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

// ValidateHooks checks the guest's declared hook set against its manifest.
//
// v2 guests export one run_hook, so there is no per-hook export to probe.
// Instead they publish a supported_hooks bitmap, read once at LoadPlugin, and
// this compares it against what the manifest declares. The ctx argument is
// retained for callers and future host-side checks.
//
// This is stricter than the v1 check it replaces. v1 could only ask "does this
// export exist", so a guest exporting MORE than it declared passed silently —
// the manifest is what an operator approves, so undeclared behaviour was
// invisible to the thing meant to authorise it. Exact equality closes that.
func (p *Plugin) ValidateHooks(ctx context.Context, declared []pbv2.Hook) error {
	p.stateMu.RLock()
	bitmap := p.hooks
	p.stateMu.RUnlock()
	if err := pbv2.ValidateManifestHooks(bitmap, declared); err != nil {
		return fmt.Errorf("wasm: %s: %w", p.name, err)
	}
	return nil
}

// supports reports whether the guest implements h, from the validated bitmap.
func (p *Plugin) supports(h pbv2.Hook) bool {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.hooks.Has(h)
}

// supportedHooks reads the guest's declared hook set.
//
// A missing export is a v1 guest, or one built against an SDK predating the
// single-export ABI. Saying so beats "hook not found" at the first dispatch,
// which is where the same guest used to surface.
func supportedHooks(ctx context.Context, mod api.Module) (pbv2.HookBitmap, error) {
	fn := mod.ExportedFunction("supported_hooks")
	if fn == nil {
		return 0, fmt.Errorf("module exports no supported_hooks: it is a v1 guest, " +
			"and v2 hosts dispatch through a single run_hook export")
	}
	res, err := fn.Call(ctx)
	if err != nil {
		return 0, fmt.Errorf("supported_hooks: %w", err)
	}
	if len(res) != 1 {
		return 0, fmt.Errorf("supported_hooks returned %d values, want 1", len(res))
	}
	return pbv2.HookBitmap(res[0]), nil
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

// CallRequest dispatches one hook into the guest and returns its result bytes.
// Uses instance pooling for concurrent request handling.
//
// hook selects which handler the guest runs. v2 guests expose a single run_hook
// export and route internally on the HookInput payload, so the hook identity
// travels in the payload the caller already built; this argument exists to skip
// guests that do not implement it, and to name the hook in errors.
func (p *Plugin) CallRequest(ctx context.Context, hook pbv2.Hook, reqID uint64, inBytes []byte, output *[]byte) error {
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

	// A guest that does not implement this hook is not an error: the pipeline
	// offers every hook to every plugin. v1 detected it by a missing export;
	// v2 asks the validated bitmap, so an unimplemented hook costs nothing
	// rather than allocating and copying a payload the guest would discard.
	if !p.supports(hook) {
		healthy = true
		return nil
	}
	fn := mod.ExportedFunction("run_hook")
	if fn == nil {
		return fmt.Errorf("wasm: %s exports no run_hook", p.name)
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
	//
	// Two arguments, not three. v1 passed the request id separately; v2 moved
	// it into HookInput, so the caller has already encoded it and passing it
	// again is an arity mismatch that fails every guest call. reqID is still
	// carried on the context above, which is what scopes host-side meta state.
	ret, err := fn.Call(callCtx, uint64(inPtr), uint64(len(inBytes)))
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
	meta      map[uint64]map[string]string
	verdictMu sync.RWMutex
	// verdicts holds request-scoped plugin verdicts: reqID → what plugins
	// asked the host to do about this request. v1 carried these back inside
	// the returned request's ToranaMeta, which meant the host could not know a
	// block had happened until every plugin had run — so a rejected,
	// PII-laden request was still handed to the compactor and warmer. As host
	// calls they arrive immediately and carry attribution.
	verdicts map[uint64]*RequestVerdicts

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
	// StateDeleteFunc backs env.state_delete. v1 deleted by setting an empty
	// value, which made storing an empty string impossible; v2 makes deletion
	// explicit and shares the env.state_set grant.
	StateDeleteFunc func(plugin, key string) error

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

	// Verdicts are request-scoped too. Leaking a block into a later request
	// that happened to reuse the id would refuse traffic nobody objected to.
	r.verdictMu.Lock()
	delete(r.verdicts, reqID)
	r.verdictMu.Unlock()
}

// metaGet reads a request-scoped meta value.
func (r *Runtime) metaGet(reqID uint64, key string) string {
	v, _ := r.metaGetPresence(reqID, key)
	return v
}

// metaGetPresence distinguishes an absent key from one holding an empty value.
//
// v2 reports absence as NOT_FOUND and a stored empty string as a successful
// empty value. Reading through the map's zero value collapsed the two, which
// made a buffered or cached empty string unusable.
func (r *Runtime) metaGetPresence(reqID uint64, key string) (string, bool) {
	r.metaMu.RLock()
	defer r.metaMu.RUnlock()
	v, ok := r.meta[reqID][key]
	return v, ok
}

// metaSet writes a request-scoped meta value.
//
// An empty value STORES an empty value. v1 deleted the key here, so a plugin
// could not distinguish "nothing stored" from "I stored nothing" and could not
// store an empty string at all. Deletion is not part of the meta surface.
func (r *Runtime) metaSet(reqID uint64, key, value string) {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
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

	// Read the guest's hook set HERE, not in ValidateHooks.
	//
	// Dispatch consults this bitmap to skip hooks a guest does not implement.
	// If it were only populated by ValidateHooks, a plugin loaded without that
	// call would have a zero bitmap and every dispatch would silently no-op —
	// a plugin that appears loaded and does nothing, with no error anywhere.
	// Reading it at load means there is no unvalidated state to get wrong;
	// ValidateHooks then only compares it against the manifest.
	bitmap, err := supportedHooks(r.ctx, inst.mod)
	if err != nil {
		_ = inst.mod.Close(r.ctx)
		return nil, fmt.Errorf("wasm: %s: %w", name, err)
	}
	p.hooks = bitmap

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
		return writeBytes(ctx, mod, r.dispatchHostCall(ctx, pluginNameOf(mod), cmd, args))
	}).Export("host_call")

	env.Instantiate(r.ctx)
}

// dispatchHostCall executes one host call and returns the framed reply.
//
// Extracted from the export closure so it is testable at all. The permission
// boundary lives here — the SDK's guest-side checks are ergonomics, and a
// handwritten guest never runs them — so it needs tests that call it directly
// rather than only through a compiled fixture.
func (r *Runtime) dispatchHostCall(ctx context.Context, pluginName, cmd, args string) []byte {
	{
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
		// Two commands are NOT operator-facing capabilities, so deriving their
		// permission from the command string looks for a grant that cannot
		// exist and refuses every call. Both mutate a namespace the plugin can
		// already write, so they share that namespace's grant rather than
		// adding approval ceremony for no new security line.
		switch cmd {
		case pbv2.MetaAppendCommand:
			perm = pbv2.MetaAppendPermission
		case pbv2.StateDeleteCommand:
			perm = pbv2.StateDeletePermission
		}
		if p == nil || !p.hasGrant(perm) {
			log.Printf("[wasm] permission denied: %s tried %s", pluginName, perm)
			// A framed refusal, not the v1 string. Guests decode
			// HostCallResult now, so the old envelope would surface as a
			// protocol error and a plugin could not tell a missing grant from
			// a broken boundary.
			return frameHostCall(nil,
				hostErr(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "permission denied: %s", perm))
		}

		// v2: every reply is a framed HostCallResult. Cases set value/herr; the
		// single exit below frames whichever was set. Extension commands put
		// their JSON body in the value arm — the BODY is opaque, the envelope
		// is not.
		var value []byte
		var herr *pbv2.HostError
		var res string // extension/domain JSON, moved into value at the exit
		switch cmd {
		case "env.block_request":
			var a pbv2.BlockRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid BlockRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setBlock(pluginName, &a)
		case "env.respond_request":
			var a pbv2.RespondRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid RespondRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setRespond(pluginName, a.Content)
		case "env.route_request":
			var a pbv2.RouteRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid RouteRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setRoute(pluginName, a.Provider, a.Model)
		case "env.set_identity":
			var a pbv2.SetIdentityArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid SetIdentityArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setIdentity(pluginName, a.Identity)
		case pbv2.MetaAppendCommand:
			var a pbv2.MetaAppendArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid MetaAppendArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// One atomic call. A meta_get + meta_set pair was two round trips
			// and a lost update: two fragments interleaving between the read
			// and the write silently drop one, and the corrupted tool call
			// surfaces much later as invalid JSON reaching the agent.
			existing, present := r.metaAppend(reqIDFrom(ctx),
				metaKey(pluginName, "append:"+strconv.FormatInt(int64(a.BlockIndex), 10)), a.Fragment)
			// Non-empty fragment acks with an empty value; an empty fragment
			// reads the buffer back. Returning the cumulative buffer after
			// every delta would be O(total x fragments) on the stream path.
			value = pbv2.MetaAppendSuccessValue(a.Fragment, []byte(existing), present)
		case "env.meta_set":
			// A decode failure used to be swallowed by `if err == nil`, so the
			// write silently did not happen. It is now a classified refusal.
			var a pbv2.MetaSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid MetaSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// An empty value STORES an empty value; it is not a delete.
			r.metaSet(reqIDFrom(ctx), metaKey(pluginName, a.Key), a.Value)
		case "env.meta_get":
			var a pbv2.MetaGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid MetaGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			v, present := r.metaGetPresence(reqIDFrom(ctx), metaKey(pluginName, a.Key))
			if !present {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "metadata key not found")
				break
			}
			value = []byte(v)
		case "env.cache_set":
			var a pbv2.CacheSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CacheSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.cache.Set(a.Key, a.Value)
		case "env.cache_get":
			var a pbv2.CacheGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CacheGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// The presence bool was previously discarded, so a miss and a
			// stored empty string were the same answer.
			v, present := r.cache.Get(a.Key)
			if !present {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "cache key not found")
				break
			}
			value = []byte(v)
		case "env.state_set":
			// Durable, plugin-private, survives a restart. The plugin name
			// comes from the module, never the payload, so one plugin cannot
			// write into another's namespace.
			var a pbv2.StateSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid StateSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			if r.StateSetFunc == nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			// An empty value STORES an empty value. v1 deleted here, which is
			// why deletion is now its own command.
			if err := r.StateSetFunc(pluginName, a.Key, a.Value); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INTERNAL, "%v", err)
			}
		case pbv2.StateDeleteCommand:
			var a pbv2.StateDeleteArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid StateDeleteArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			if r.StateDeleteFunc == nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			// Deleting an absent key succeeds: the caller wants it gone.
			if err := r.StateDeleteFunc(pluginName, a.Key); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INTERNAL, "%v", err)
			}
		case "env.state_get":
			var a pbv2.StateGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid StateGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			if r.StateGetFunc == nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			v, present := r.StateGetFunc(pluginName, a.Key)
			if !present {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "state key not found")
				break
			}
			value = []byte(v)
		case "env.state_keys":
			if r.StateKeysFunc == nil {
				// An empty list said "no keys", which is not the same as "no
				// store" — a plugin would conclude its writes had vanished.
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			b, _ := json.Marshal(r.StateKeysFunc(pluginName))
			value = b
		case "env.now":
			// WASI preview1 gives the guest no clock, deliberately. Plugins
			// that reason about elapsed time — cache lifetimes, deadlines,
			// rate windows — otherwise have no way to ask.
			//
			// This is a capability, not a convenience: a plugin that writes the
			// result into a request makes its output non-deterministic, which
			// busts the provider's prompt cache on every turn. See
			// PLUGIN_SEMANTICS §6.
			value = []byte(strconv.FormatInt(time.Now().UnixMilli(), 10))
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
			cfg := p.pluginConfig()
			if cfg == "" {
				cfg = "{}"
			}
			value = []byte(cfg)
		case "env.original_request":
			// Pristine pre-pipeline request, pb-encoded. Absence is NOT_FOUND,
			// not an empty value: an all-default ChatRequest legitimately
			// marshals to zero bytes.
			if r.OriginalRequestFunc == nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "no original request captured")
				break
			}
			value = r.OriginalRequestFunc(ctx)
		case "env.original_response":
			// Raw upstream response body (non-streaming only). An upstream body
			// can legitimately be empty, so absence is again the error arm.
			if r.OriginalResponseFunc == nil {
				herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "no original response captured")
				break
			}
			value = r.OriginalResponseFunc(ctx)
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
			herr = hostErr(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "unknown host call %q", cmd)
		}

		if herr == nil && value == nil && res != "" {
			value = []byte(res)
		}
		return frameHostCall(value, herr)
	}
}

// dispatchHostCallForTest exposes the dispatcher to tests in this package.
func (r *Runtime) dispatchHostCallForTest(ctx context.Context, pluginName, cmd, args string) []byte {
	return r.dispatchHostCall(ctx, pluginName, cmd, args)
}

func readStr(mod api.Module, ptr, length uint32) string {
	b, ok := readMemory(mod.Memory(), ptr, length)
	if !ok {
		return ""
	}
	return string(b)
}

// writeStr calls the WASM module's 'alloc' function to allocate space, then writes the string.
// frameHostCall builds the HostCallResult a v2 guest decodes.
//
// Exactly one arm is set. A nil value with no error is a successful EMPTY
// value, which is distinct from an error and from absence — that distinction
// is the whole reason the envelope exists, so it must survive here.
func frameHostCall(value []byte, herr *pbv2.HostError) []byte {
	result := &pbv2.HostCallResult{}
	if herr != nil {
		result.Result = &pbv2.HostCallResult_Error{Error: herr}
	} else {
		result.Result = &pbv2.HostCallResult_Value{Value: value}
	}
	raw, err := proto.Marshal(result)
	if err != nil {
		// Marshalling a two-field message cannot realistically fail, but a
		// zero-length reply is a protocol error to the guest rather than a
		// silent success, so say something rather than nothing.
		log.Printf("[wasm] frame host-call result: %v", err)
		return nil
	}
	return raw
}

// hostErr builds a classified refusal.
func hostErr(code pbv2.ErrorCode, format string, args ...any) *pbv2.HostError {
	return &pbv2.HostError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func writeBytes(ctx context.Context, mod api.Module, b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	return writeStr(ctx, mod, string(b))
}

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
