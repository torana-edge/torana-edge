package wasm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"sort"
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
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
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
	// defaultInstanceIdleTimeout retires burst-created instances after a quiet
	// period while retaining one ready instance per plugin. PoolSize remains the
	// concurrency ceiling; it no longer implies permanent burst retention.
	defaultInstanceIdleTimeout = time.Minute
	// defaultMemoryLimitPages caps a plugin at 64 MiB of Wasm linear memory.
	// A page is 64 KiB. This is high enough for the current Go/WASI plugins while
	// preventing an absent module maximum from becoming wazero's 4 GiB default.
	defaultMemoryLimitPages uint32 = 1024
)

// RuntimeOptions bounds resources used by every plugin loaded in a Runtime.
// Zero values select conservative defaults, so existing callers retain working
// behavior without inheriting unbounded resource use.
type RuntimeOptions struct {
	// PoolSize is the maximum number of concurrent instances per plugin.
	PoolSize int
	// CallTimeout is the maximum duration of one guest function invocation.
	CallTimeout time.Duration
	// MemoryLimitPages is the maximum Wasm linear memory per instance (64 KiB/page).
	MemoryLimitPages uint32
	// InstanceIdleTimeout retires burst-created idle instances while retaining
	// one ready instance per plugin. Zero selects the default; a negative value
	// disables retirement for controlled comparisons.
	InstanceIdleTimeout time.Duration
}

func defaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{
		PoolSize:            defaultPoolSize,
		CallTimeout:         defaultCallTimeout,
		MemoryLimitPages:    defaultMemoryLimitPages,
		InstanceIdleTimeout: defaultInstanceIdleTimeout,
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
	if options.InstanceIdleTimeout == 0 {
		options.InstanceIdleTimeout = defaults.InstanceIdleTimeout
	} else if options.InstanceIdleTimeout < 0 {
		options.InstanceIdleTimeout = 0
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
	hooks     pbv1.HookBitmap
	config    string // per-plugin config JSON (plugins.config.<name>); "" if none
	resources PluginResources
	// privateCacheIdentity scopes derived plugin cache entries to the exact
	// approved resource snapshot. Rebinding a model, credential, endpoint,
	// file budget, or pricing catalog must not reuse a decision made under the
	// old authority. The plugin name remains the default for resource-free
	// guests. cacheIdentityValid is false only for a host-constructed invalid
	// resource graph; cache calls then fail closed.
	privateCacheIdentity string
	cacheIdentityValid   bool
	runtime              wazero.Runtime

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

	// poolClosed is set once by the owning runtime's close/unload path: after
	// it, acquire fails deterministically and release(inst) CLOSES the instance
	// instead of returning it to the (dead) pool. Guarded by stateMu.
	poolClosed bool

	// callMu is the call-admission lock: CallRequest holds it read for its
	// ENTIRE lifecycle (acquire through deferred release), and plugin cleanup
	// takes it write — blocking new calls and waiting until every active call
	// has returned before marking the pool closed and releasing the compiled
	// handle. Cleanup cannot wait forever: every call's context carries the
	// per-call timeout. This closes the check-then-close/requeue TOCTOU in
	// release: while cleanup holds the write lock, no release can run.
	callMu sync.RWMutex

	// lifecycle is the owning runtime's observer seam (nil in production).
	lifecycle *lifecycleHooks

	// compiledCloseOnce makes the compiled-handle release exactly once even
	// when close and unload race or repeat.
	compiledCloseOnce sync.Once

	poolSize    int
	callTimeout time.Duration
	idleTimeout time.Duration

	instanceCount uint64
}

// PluginResources is the immutable, approval-bound resource view installed on
// one loaded plugin generation. Host-call payloads name logical slots and
// paths; they never select credentials, host filesystem locations, or network
// origins directly.
type PluginResources struct {
	Credentials      map[string]string
	Files            map[string]FileResource
	HTTP             map[string]HTTPResource
	ModelServices    map[string]ModelServiceResource
	PricingResources map[string]PricingResource
}

type FileResource struct {
	Operations    map[string]bool
	MaxBytes      int64
	RetainedFiles int
}

type HTTPResource struct {
	Name              string
	Origin            string
	Methods           map[string]bool
	Timeout           time.Duration
	MaxRequestBytes   int64
	MaxResponseBytes  int64
	MaxCallsPerMinute int
}

type ModelServiceResource struct {
	Name              string
	Provider          string
	Model             string
	Path              string
	Timeout           time.Duration
	MaxTokens         uint32
	MaxInputBytes     int64
	MaxCallsPerMinute int
	MaxTokensPerHour  int64
}

type PricingResource struct {
	Name            string
	ForModelService string
	Prices          map[string]*pbv1.ModelPricing
}

// PricingCoordinate returns the collision-free map key for one operator-owned
// provider/model price. Provider and model are separately length-framed: a
// delimiter is insufficient because both identifiers are external strings.
func PricingCoordinate(provider, model string) string {
	buf := make([]byte, 16+len(provider)+len(model))
	binary.LittleEndian.PutUint64(buf[:8], uint64(len(provider)))
	copy(buf[8:], provider)
	offset := 8 + len(provider)
	binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(len(model)))
	copy(buf[offset+8:], model)
	return string(buf)
}

func clonePluginResources(in PluginResources) PluginResources {
	out := PluginResources{
		Credentials:      make(map[string]string, len(in.Credentials)),
		Files:            make(map[string]FileResource, len(in.Files)),
		HTTP:             make(map[string]HTTPResource, len(in.HTTP)),
		ModelServices:    make(map[string]ModelServiceResource, len(in.ModelServices)),
		PricingResources: make(map[string]PricingResource, len(in.PricingResources)),
	}
	for k, v := range in.Credentials {
		out.Credentials[k] = v
	}
	for k, v := range in.Files {
		ops := make(map[string]bool, len(v.Operations))
		for op, allowed := range v.Operations {
			ops[op] = allowed
		}
		v.Operations = ops
		out.Files[k] = v
	}
	for k, v := range in.HTTP {
		methods := make(map[string]bool, len(v.Methods))
		for method, allowed := range v.Methods {
			methods[method] = allowed
		}
		v.Methods = methods
		out.HTTP[k] = v
	}
	for k, v := range in.ModelServices {
		out.ModelServices[k] = v
	}
	for k, v := range in.PricingResources {
		prices := make(map[string]*pbv1.ModelPricing, len(v.Prices))
		for key, pricing := range v.Prices {
			if pricing != nil {
				prices[key] = proto.Clone(pricing).(*pbv1.ModelPricing)
			} else {
				prices[key] = nil
			}
		}
		v.Prices = prices
		out.PricingResources[k] = v
	}
	return out
}

type pluginInstance struct {
	mod        api.Module
	logEnabled bool
	idleSince  time.Time
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) SetGrants(g []string) {
	// Grant installation and idle-instance recycling are one transaction. The
	// idle sweeper takes the read side of this lock, so it cannot temporarily
	// remove an old-policy instance and requeue it after this method drains the
	// pool. Calls are quiesced for the same reason: an instance observes one
	// immutable grant/logging policy for its whole invocation.
	p.callMu.Lock()
	defer p.callMu.Unlock()
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

// SetResources installs the digest-approved resources for this exact loaded
// generation. Calls are quiesced so an invocation cannot observe bindings
// from two approvals.
func (p *Plugin) SetResources(resources PluginResources) {
	p.callMu.Lock()
	defer p.callMu.Unlock()
	p.stateMu.Lock()
	cloned := clonePluginResources(resources)
	p.resources = cloned
	p.privateCacheIdentity, p.cacheIdentityValid = resourceCacheIdentity(p.name, cloned)
	p.stateMu.Unlock()
}

func resourceCacheIdentity(plugin string, resources PluginResources) (string, bool) {
	if len(resources.Credentials) == 0 && len(resources.Files) == 0 && len(resources.HTTP) == 0 && len(resources.ModelServices) == 0 && len(resources.PricingResources) == 0 {
		return plugin, true
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(raw)
	return plugin + "\x00resources\x00" + string(digest[:]), true
}

func (p *Plugin) cacheIdentity() (string, bool) {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.privateCacheIdentity, p.cacheIdentityValid
}

func (p *Plugin) resourceSnapshot() PluginResources {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return clonePluginResources(p.resources)
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
// plugin guests export one run_hook, so there is no per-hook export to probe.
// Instead they publish a supported_hooks bitmap, read once at LoadPlugin, and
// this compares it against what the manifest declares. The ctx argument is
// retained for callers and future host-side checks.
//
// This is stricter than the v1 check it replaces. v1 could only ask "does this
// export exist", so a guest exporting MORE than it declared passed silently —
// the manifest is what an operator approves, so undeclared behaviour was
// invisible to the thing meant to authorise it. Exact equality closes that.
func (p *Plugin) ValidateHooks(ctx context.Context, declared []pbv1.Hook) error {
	p.stateMu.RLock()
	bitmap := p.hooks
	p.stateMu.RUnlock()
	if err := pbv1.ValidateManifestHooks(bitmap, declared); err != nil {
		return fmt.Errorf("wasm: %s: %w", p.name, err)
	}
	return nil
}

// supports reports whether the guest implements h, from the validated bitmap.
func (p *Plugin) supports(h pbv1.Hook) bool {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.hooks.Has(h)
}

// supportedHooks reads the guest's declared hook set.
//
// A missing export is a v1 guest, or one built against an SDK predating the
// single-export ABI. Saying so beats "hook not found" at the first dispatch,
// which is where the same guest used to surface.
func supportedHooks(ctx context.Context, mod api.Module) (pbv1.HookBitmap, error) {
	fn := mod.ExportedFunction("supported_hooks")
	if fn == nil {
		return 0, fmt.Errorf("module exports no supported_hooks: it is a v1 guest, " +
			"and this host dispatches through a single run_hook export")
	}
	res, err := fn.Call(ctx)
	if err != nil {
		return 0, fmt.Errorf("supported_hooks: %w", err)
	}
	if len(res) != 1 {
		return 0, fmt.Errorf("supported_hooks returned %d values, want 1", len(res))
	}
	return pbv1.HookBitmap(res[0]), nil
}

// acquire returns a plugin instance from the pool.
func (p *Plugin) acquire(ctx context.Context) (*pluginInstance, error) {
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.stateMu.RLock()
	closed := p.poolClosed
	p.stateMu.RUnlock()
	if closed {
		<-p.slots
		return nil, fmt.Errorf("wasm: %s: plugin is closed", p.name)
	}
	select {
	case inst := <-p.pool:
		if inst != nil {
			inst.idleSince = time.Time{}
			p.fireInstanceAcquired()
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
	p.fireInstanceAcquired()
	return inst, nil
}

// fireInstanceAcquired invokes the observer seam when a call has acquired an
// instance (the admission read lock is held).
func (p *Plugin) fireInstanceAcquired() {
	if p.lifecycle != nil && p.lifecycle.instanceAcquired != nil {
		p.lifecycle.instanceAcquired(p.name)
	}
}

// release returns an instance to the pool.
func (p *Plugin) release(inst *pluginInstance) {
	defer func() { <-p.slots }()
	if inst == nil || inst.mod == nil || inst.mod.IsClosed() {
		return
	}
	p.stateMu.RLock()
	closed := p.poolClosed
	p.stateMu.RUnlock()
	if closed {
		// The pool is dead: a closed instance must never be returned to it.
		// This is the active-call-vs-close path — the in-flight call finishes
		// and its instance is closed here instead of re-queued.
		_ = inst.mod.Close(context.Background())
		return
	}
	if inst.logEnabled != p.hasGrant("env.log") {
		_ = inst.mod.Close(context.Background())
		return
	}
	inst.idleSince = time.Now()
	select {
	case p.pool <- inst:
	default:
		// Pool full — close the extra instance.
		inst.mod.Close(context.Background())
	}
}

// retireIdleInstances closes only stale burst-created instances. One newest
// idle instance is always retained so ordinary low-concurrency traffic stays
// warm. The channel is drained as a snapshot; concurrent acquire/release is
// safe, and a requeue that loses a race to a concurrent release closes the
// extra instance instead of exceeding PoolSize.
func (p *Plugin) retireIdleInstances(now time.Time) {
	if p.idleTimeout <= 0 {
		return
	}
	p.callMu.RLock()
	defer p.callMu.RUnlock()
	p.stateMu.RLock()
	closed := p.poolClosed
	p.stateMu.RUnlock()
	if closed {
		return
	}

	snapshotSize := len(p.pool)
	idle := make([]*pluginInstance, 0, snapshotSize)
snapshot:
	for range snapshotSize {
		select {
		case inst := <-p.pool:
			if inst != nil {
				idle = append(idle, inst)
			}
		default:
			break snapshot
		}
	}
	newest := -1
	for i, inst := range idle {
		if newest == -1 || inst.idleSince.After(idle[newest].idleSince) {
			newest = i
		}
	}
	for i, inst := range idle {
		stale := !inst.idleSince.IsZero() && now.Sub(inst.idleSince) >= p.idleTimeout
		if i != newest && stale {
			_ = inst.mod.Close(context.Background())
			continue
		}
		select {
		case p.pool <- inst:
		default:
			_ = inst.mod.Close(context.Background())
		}
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
	// Only unique-name allocation needs serialization. wazero runtimes support
	// concurrent instantiation; holding this lock through InstantiateModule
	// turned a cold four-request burst into three sequential guest startups.
	p.poolMu.Lock()
	p.instanceCount++
	instanceName := fmt.Sprintf("%s-%d", p.name, p.instanceCount)
	p.poolMu.Unlock()

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
// hook selects which handler the guest runs. plugin guests expose a single run_hook
// export and route internally on the HookInput payload, so the hook identity
// travels in the payload the caller already built; this argument exists to skip
// guests that do not implement it, and to name the hook in errors.
func (p *Plugin) CallRequest(ctx context.Context, hook pbv1.Hook, reqID uint64, inBytes []byte, output *[]byte) error {
	// Call admission: the read lock is held for the ENTIRE call lifecycle —
	// guards, acquire, the guest call, and the deferred release. Plugin
	// cleanup takes the write lock, so a call in flight here both blocks
	// close's resource release and is guaranteed to finish (its per-call
	// timeout bounds the wait) before the pool is drained and the compiled
	// handle is released.
	p.callMu.RLock()
	defer p.callMu.RUnlock()
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
		// call-finished fires only for a call that acquired an instance, and
		// only after its release/discard ran (the admission lock is still held).
		p.fireCallFinished()
	}()

	mod := inst.mod

	// A guest that does not implement this hook is not an error: the pipeline
	// offers every hook to every plugin. v1 detected it by a missing export;
	// current ABI asks the validated bitmap, so an unimplemented hook costs nothing
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
	// Two arguments, not three. v1 passed the request id separately; current ABI moved
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
	// Values are []byte, not string, so meta_append can grow them in place. A
	// string forces a full copy per fragment, which made assembling one tool
	// call O(total x fragments) on the hot stream path.
	meta      map[uint64]map[string][]byte
	verdictMu sync.RWMutex
	// verdicts holds request-scoped plugin verdicts: reqID → what plugins
	// asked the host to do about this request. v1 carried these back inside
	// the returned request's ToranaMeta, which meant the host could not know a
	// block had happened until every plugin had run — so a rejected,
	// PII-laden request was still handed to the compactor and warmer. As host
	// calls they arrive immediately and carry attribution.
	verdicts map[uint64]*RequestVerdicts

	// lifecycleMu serializes LoadPlugin, UnloadPlugin, and Close. LoadPlugin
	// holds it for its ENTIRE construction transaction (duplicate rejection
	// and publication are one critical section), and Close marks closing under
	// it before snapshotting the plugin map. After closed, loads and unloads
	// fail/no-op deterministically.
	lifecycleMu    sync.Mutex
	lifecycleState lifecycleState
	closeOnce      sync.Once
	closeErr       error
	idleStop       chan struct{}
	idleDone       chan struct{}

	// testHooks is a nil-by-default lifecycle observer seam used by the
	// reference-model tests. Production never installs it: the hooks cost
	// nothing when nil and no test counters live in the production struct.
	testHooks *lifecycleHooks

	// cache is the cross-request TTL store. Host-owned domain framing keeps
	// ordinary per-plugin keys disjoint from the separately granted shared
	// channel, even if a malicious guest chooses keys containing NULs or the
	// other domain's textual prefix.
	cache cache.Store
	// ownsCache marks a runtime-private store (NewRuntime) that Close must
	// release; shared stores (NewRuntimeWithCache) outlive the runtime.
	ownsCache bool

	// CompactionReportFunc handles torana_record_savings host calls
	// (compaction byte savings reported by plugins), attributed to the
	// calling plugin. Set by the server. This is the ONE savings callback —
	// the legacy two-field SavingsFunc form was removed.
	CompactionReportFunc func(ctx context.Context, plugin string, report economics.CompactionReport, target PricingResource, summarizer *PricingResource)
	// EvaluateCompactionFunc performs the optional operator-priced economic
	// gate before a plugin mutates history.
	EvaluateCompactionFunc func(ctx context.Context, report economics.CompactionReport, target PricingResource, summarizer *PricingResource) economics.CompactionDecision
	// RequestMutationFunc observes a plugin's returned canonical request. It is
	// used by the host to make an earlier routing verdict visible to later
	// plugins' economic-gate calls.
	RequestMutationFunc func(ctx context.Context, requestPB []byte)

	// OriginalRequestFunc returns the pristine pre-pipeline request as pb
	// bytes for env.original_request (empty when unavailable). Set by the
	// server; grant-gated at dispatch.
	// Returns (bytes, captured). Presence is NOT length: an all-default
	// ChatRequest marshals to zero bytes, and the server installs the callback
	// unconditionally, so "returned nil" means not captured on this path
	// (streaming, upstream error, pre-capture) rather than an empty request.
	OriginalRequestFunc func(ctx context.Context) ([]byte, bool)

	// OriginalResponseFunc returns the raw upstream response body for
	// env.original_response (empty when unavailable — e.g. streaming
	// responses, which are never buffered). Set by the server.
	// Returns (bytes, captured). An upstream body can legitimately be empty, so
	// again presence is separate from length.
	OriginalResponseFunc func(ctx context.Context) ([]byte, bool)

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
	// value, which made storing an empty string impossible; current ABI makes deletion
	// explicit and shares the env.state_set grant.
	StateDeleteFunc func(plugin, key string) error

	// CachePricingFunc answers torana_cache_pricing: given a provider and
	// model, what the prompt cache costs and how long it lives. Data, not a
	// decision — the host holds the prices, the plugin holds the policy.
	// Returns the classified outcome: malformed input is a refused
	// INVALID_ARGUMENT, an unknown provider a refused NOT_CONFIGURED, and a
	// legitimate query result — priced, or explicitly unpriced/unconfigured —
	// is a domain value whose status field the guest reads.
	CachePricingFunc func(ctx context.Context, payloadJSON string) ExtensionResult

	// SendRequestFunc backs torana_send_request: a plugin-originated provider
	// request. The plugin name is passed so the host can meter it against that
	// plugin's budget and attribute it in the feed — spend a plugin initiates
	// must still be traceable to the plugin that initiated it.
	//
	// It returns the domain envelope and a classified refusal. The value arm
	// carries provider outcomes only — never a refusal, which would leave the
	// SDK keying its sentinel off a status string. Refusals travel framed in
	// the HostError arm: INVALID_ARGUMENT for malformed payloads, NOT_CONFIGURED
	// for unknown providers, missing budgets or missing format adapters, and
	// UNAVAILABLE for transport failure.
	//
	// The outcome is a classified ExtensionResult: provider outcomes travel in
	// the value arm, refusals in the refusal arm.
	SendRequestFunc func(ctx context.Context, plugin, payloadJSON string) ExtensionResult

	// VerifyVirtualKeyFunc answers verify_virtual_key: whether a caller's
	// virtual key is valid. The OSS proxy does not wire it — private-nucleus
	// does — so an absent callback is a NOT_CONFIGURED refusal, never
	// UNAVAILABLE: a declared permission that can never succeed in this host
	// is a configuration gap, not a transient outage a plugin should retry.
	VerifyVirtualKeyFunc func(ctx context.Context, payloadJSON string) ExtensionResult

	// Resource callbacks receive only host-resolved, approval-bound resources.
	// The guest can name a manifest slot/path, but cannot choose an operator
	// credential ID, OS path, or origin.
	CredentialGetFunc func(ctx context.Context, plugin, credentialID string) ([]byte, error)
	FileAppendFunc    func(plugin, path string, data []byte, resource FileResource) error
	FileReadFunc      func(plugin, path string, resource FileResource) ([]byte, error)
	FileWriteFunc     func(plugin, path string, data []byte, resource FileResource) error
	FileListFunc      func(plugin, prefix string, resources map[string]FileResource) ([]string, error)
	FileDeleteFunc    func(plugin, path string, resource FileResource) error
	HTTPRequestFunc   func(ctx context.Context, plugin string, resource HTTPResource, request *pbv1.OutboundHTTPRequestArgs) (*pbv1.OutboundHTTPResponse, error)
	ModelCompleteFunc func(ctx context.Context, plugin string, resource ModelServiceResource, request *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError)
	ModelPricingFunc  func(ctx context.Context, plugin string, resource PricingResource) (*pbv1.ModelPricing, *pbv1.HostError)
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

// lifecycleState is the Runtime's ownership lifecycle.
type lifecycleState int

const (
	lifecycleOpen lifecycleState = iota
	lifecycleClosing
	lifecycleClosed
)

// lifecycleHooks is the deterministic observer seam for lifecycle tests. Every
// event fires exactly when the transition happens (including post-compile
// failure paths), and the reference model replays the recorded stream.
type lifecycleHooks struct {
	// loadBegin fires when a LoadPlugin transaction starts (after the
	// lifecycle/duplicate guards).
	loadBegin func(name string)
	// compiledAcquired fires immediately after a successful CompileModule.
	compiledAcquired func(name string)
	// published fires when a plugin is inserted into the map.
	published func(name string)
	// constructFailed fires when a LoadPlugin transaction fails after it
	// began (construction ends absent; resources already released).
	constructFailed func(name string)
	// unloadBegin fires when an UnloadPlugin transaction starts quiescing.
	unloadBegin func(name string)
	// quiesced fires when a plugin's callMu WRITE lock has been acquired —
	// the per-plugin quiescence boundary: no further instance acquisition is
	// legal from this point, and compiled release may not precede it.
	quiesced func(name string)
	// instanceAcquired fires when a plugin instance is successfully acquired
	// for a call.
	instanceAcquired func(name string)
	// callFinished fires when a CallRequest's deferred release has run (the
	// admission read lock is still held at this point).
	callFinished func(name string)
	// compiledReleased fires exactly when a compiled handle is released
	// (closePluginResources or a post-compile LoadPlugin failure path).
	compiledReleased func(name string)
	// closeBegin and closeEnd bracket Runtime.Close's resource release.
	closeBegin func()
	closeEnd   func()
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
		meta:      make(map[uint64]map[string][]byte),
		cache:     store,
		ownsCache: ownsCache,
		options:   options,
	}
	if options.InstanceIdleTimeout > 0 {
		r.idleStop = make(chan struct{})
		r.idleDone = make(chan struct{})
		go r.retireIdleLoop()
	}
	wasi_snapshot_preview1.MustInstantiate(r.ctx, r.runtime)
	r.installHostFunctions()
	return r
}

// Close releases every owned plugin (pool instances, then each compiled
// handle exactly once, in sorted name order for a deterministic joined error),
// the wazero runtime, and — when owned — the cache store. Repeated and
// concurrent Close calls return the SAME cached result; nothing is released
// twice.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.closeLocked()
	})
	return r.closeErr
}

func (r *Runtime) closeLocked() error {
	r.stopIdleRetirement()
	// The whole transaction holds lifecycleMu: loads/unloads see closing and
	// cannot mutate the ownership set. Each plugin stays published in the map
	// until ITS OWN write-lock quiescence completes (an admitted call's host
	// calls keep resolving itself through r.plugins while Close waits), then
	// is removed and its resources released under the same write lock.
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.lifecycleState = lifecycleClosing
	r.fireCloseBegin()

	r.mu.RLock()
	plugins := make(map[string]*Plugin, len(r.plugins))
	for name, p := range r.plugins {
		plugins[name] = p
	}
	r.mu.RUnlock()

	// Deterministic order: sorted names (map iteration is random, and the
	// joined error must be stable across runs and tests).
	names := make([]string, 0, len(plugins))
	for name := range plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, name := range names {
		p := plugins[name]
		// ONE atomic per-plugin transaction, exactly like UnloadPlugin:
		// quiesce under the write lock, delete the exact pointer while still
		// holding it, THEN close instances and the compiled handle. There is
		// never an interval in which a plugin is still published after its
		// resources have been released.
		p.callMu.Lock()
		r.mu.Lock()
		if cur, still := r.plugins[name]; still && cur == p {
			delete(r.plugins, name)
		}
		r.mu.Unlock()
		// The quiesced boundary fires under the write lock, immediately after
		// the exact-pointer deletion and before any resource release: no
		// further admission is legal and no release may precede it.
		r.fireQuiesced(name)
		if err := r.closePluginResourcesLocked(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		p.callMu.Unlock()
	}
	// Instances and compiled handles are closed before the runtime and cache:
	// an owned cache close must not prevent module cleanup, and vice versa.
	if err := r.runtime.Close(r.ctx); err != nil {
		errs = append(errs, fmt.Errorf("wazero runtime: %w", err))
	}
	if r.ownsCache {
		r.cache.Close()
	}

	r.lifecycleState = lifecycleClosed
	r.fireCloseEnd()
	return errors.Join(errs...)
}

func (r *Runtime) retireIdleLoop() {
	defer close(r.idleDone)
	interval := r.options.InstanceIdleTimeout / 2
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			r.mu.RLock()
			plugins := make([]*Plugin, 0, len(r.plugins))
			for _, p := range r.plugins {
				plugins = append(plugins, p)
			}
			r.mu.RUnlock()
			for _, p := range plugins {
				p.retireIdleInstances(now)
			}
		case <-r.idleStop:
			return
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *Runtime) stopIdleRetirement() {
	if r.idleStop == nil {
		return
	}
	close(r.idleStop)
	<-r.idleDone
	// closeOnce makes this one-shot, but nil the channels to make the ownership
	// transition explicit and keep test inspection unambiguous.
	r.idleStop = nil
	r.idleDone = nil
}

// UnloadPlugin removes a loaded plugin from the runtime, releasing its pool
// instances and compiled handle exactly once. Reachability from the plugin
// map is removed BEFORE any resource is closed. After Runtime.Close it is a
// deterministic no-op. Used by control-plane rejection paths (a pipeline that
// loads then rejects a plugin in non-strict mode must not retain it).
func (r *Runtime) UnloadPlugin(name string) error {
	// The ENTIRE transaction — lookup, quiescence, reachability removal, and
	// resource release — runs under lifecycleMu, so Close can never return
	// while an unloaded generation still quiesces and a same-name load can
	// never publish against a live generation.
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.lifecycleState != lifecycleOpen {
		return nil
	}
	r.mu.RLock()
	p, ok := r.plugins[name]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	r.fireUnloadBegin(name)

	// Quiesce BEFORE removing reachability: reachability is retained UNTIL
	// the write lock is held (an admitted call may still issue host calls
	// that resolve itself through r.plugins), then removed, then resources
	// are released — all under the write lock.
	p.callMu.Lock()
	defer p.callMu.Unlock()
	r.mu.Lock()
	delete(r.plugins, name)
	r.mu.Unlock()
	r.fireQuiesced(name)
	return r.closePluginResourcesLocked(p)
}

// closePluginResources closes one plugin's resources under the call-admission
// WRITE lock: new calls are blocked and every active call must finish (its
// per-call timeout bounds the wait) and release before the pool is marked
// closed, idle instances are drained, and the compiled handle is released —
// exactly once, in instance-before-compiled order. While the write lock is
// held no release() can run, which closes the check-then-close/requeue
// TOCTOU.
// closePluginResourcesLocked assumes the caller holds p.callMu (write) and
// that the quiesced boundary has already fired (the call sites fire it right
// after the exact-pointer deletion).
func (r *Runtime) closePluginResourcesLocked(p *Plugin) error {
	p.stateMu.Lock()
	p.poolClosed = true
	p.stateMu.Unlock()

	// Instances first: wazero documents compiled-close as safe with
	// outstanding instantiated modules, but instance-first is the clearer
	// ownership order and does not rely on that subtle guarantee.
	var instErrs []error
	for {
		select {
		case inst := <-p.pool:
			if inst != nil && inst.mod != nil {
				if err := inst.mod.Close(r.ctx); err != nil {
					instErrs = append(instErrs, err)
				}
			}
		default:
			goto drained
		}
	}
drained:

	var compiledErr error
	p.compiledCloseOnce.Do(func() {
		if p.compiled != nil {
			compiledErr = p.compiled.Close(r.ctx)
		}
		r.fireCompiledReleased(p.name)
	})
	return errors.Join(append(instErrs, compiledErr)...)
}

// fireCallFinished invokes the observer seam when a call's deferred release
// has run (the admission read lock is still held).
func (p *Plugin) fireCallFinished() {
	if p.lifecycle != nil && p.lifecycle.callFinished != nil {
		p.lifecycle.callFinished(p.name)
	}
}

// wrapCloseErr converts a cleanup error into a stable joined fragment (nil
// when the close succeeded).
func wrapCloseErr(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s close: %w", what, err)
}

// fireCompiledReleased invokes the observer seam for a compiled-handle
// release. Nil hooks are a no-op; tests install a recorder.
func (r *Runtime) fireCompiledReleased(name string) {
	if r.testHooks != nil && r.testHooks.compiledReleased != nil {
		r.testHooks.compiledReleased(name)
	}
}

func (r *Runtime) fireLoadBegin(name string) {
	if r.testHooks != nil && r.testHooks.loadBegin != nil {
		r.testHooks.loadBegin(name)
	}
}

func (r *Runtime) firePublished(name string) {
	if r.testHooks != nil && r.testHooks.published != nil {
		r.testHooks.published(name)
	}
}

func (r *Runtime) fireCompiledAcquired(name string) {
	if r.testHooks != nil && r.testHooks.compiledAcquired != nil {
		r.testHooks.compiledAcquired(name)
	}
}

func (r *Runtime) fireConstructFailed(name string) {
	if r.testHooks != nil && r.testHooks.constructFailed != nil {
		r.testHooks.constructFailed(name)
	}
}

func (r *Runtime) fireQuiesced(name string) {
	if r.testHooks != nil && r.testHooks.quiesced != nil {
		r.testHooks.quiesced(name)
	}
}

func (r *Runtime) fireUnloadBegin(name string) {
	if r.testHooks != nil && r.testHooks.unloadBegin != nil {
		r.testHooks.unloadBegin(name)
	}
}

func (r *Runtime) fireCloseBegin() {
	if r.testHooks != nil && r.testHooks.closeBegin != nil {
		r.testHooks.closeBegin()
	}
}

func (r *Runtime) fireCloseEnd() {
	if r.testHooks != nil && r.testHooks.closeEnd != nil {
		r.testHooks.closeEnd()
	}
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
// current ABI reports absence as NOT_FOUND and a stored empty string as a successful
// empty value. Reading through the map's zero value collapsed the two, which
// made a buffered or cached empty string unusable.
func (r *Runtime) metaGetPresence(reqID uint64, key string) (string, bool) {
	r.metaMu.RLock()
	defer r.metaMu.RUnlock()
	v, ok := r.meta[reqID][key]
	return string(v), ok
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
		bucket = make(map[string][]byte)
		r.meta[reqID] = bucket
	}
	bucket[key] = []byte(value)
}

// reqIDFrom extracts the request ID host calls were invoked under.
// Calls outside a request (e.g. hook validation) land in bucket 0.
func reqIDFrom(ctx context.Context) uint64 {
	id, _ := ctx.Value(reqIDKey{}).(uint64)
	return id
}

func (r *Runtime) LoadPlugin(name string, wasmBytes []byte) (*Plugin, error) {
	// The ENTIRE construction transaction runs under the lifecycle lock:
	// duplicate rejection, compilation, and publication are one critical
	// section, so a load can neither publish after Close's snapshot nor race
	// a duplicate into the map. Plugin loading is control-plane work.
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.lifecycleState != lifecycleOpen {
		return nil, fmt.Errorf("wasm: %s: runtime is closed", name)
	}
	r.mu.RLock()
	_, dup := r.plugins[name]
	r.mu.RUnlock()
	if dup {
		// A second load of the same name is a caller/configuration error;
		// reject BEFORE compilation so nothing is ever acquired for it.
		return nil, fmt.Errorf("wasm: plugin %q is already loaded", name)
	}
	r.fireLoadBegin(name)

	// Compile once here; every pool instance is then instantiated cheaply
	// from p.compiled. With the shared compilation cache (see NewRuntime),
	// a runtime built on hot-reload reuses an unchanged module's machine
	// code instead of recompiling it.
	compiled, err := r.runtime.CompileModule(r.ctx, wasmBytes)
	if err != nil {
		// No compiled handle is owned in this branch, but the construction
		// transaction that load-begin opened must end: the model treats
		// construct-failed as the terminal event for a constructing
		// generation, and a live constructing generation would otherwise be
		// rejected at close-end.
		r.fireConstructFailed(name)
		return nil, fmt.Errorf("wasm: %s: compile: %w", name, err)
	}
	r.fireCompiledAcquired(name)

	p := &Plugin{
		name:        name,
		compiled:    compiled,
		lifecycle:   r.testHooks,
		runtime:     r.runtime,
		pool:        make(chan *pluginInstance, r.options.PoolSize),
		slots:       make(chan struct{}, r.options.PoolSize),
		poolSize:    r.options.PoolSize,
		callTimeout: r.options.CallTimeout,
		idleTimeout: r.options.InstanceIdleTimeout,
	}
	p.privateCacheIdentity = name
	p.cacheIdentityValid = true

	// Pre-warm the pool with one instance. Any failure after a successful
	// compile must release the compiled handle: it owns a shared-cache
	// reference and would otherwise retain generated code forever.
	inst, err := p.newInstance(r.ctx)
	if err != nil {
		closeErr := compiled.Close(r.ctx)
		r.fireCompiledReleased(name)
		r.fireConstructFailed(name)
		return nil, errors.Join(fmt.Errorf("wasm: %s: %w", name, err), wrapCloseErr("compiled", closeErr))
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
		instErr := inst.mod.Close(r.ctx)
		compiledErr := compiled.Close(r.ctx)
		r.fireCompiledReleased(name)
		r.fireConstructFailed(name)
		return nil, errors.Join(fmt.Errorf("wasm: %s: %w", name, err),
			wrapCloseErr("instance", instErr), wrapCloseErr("compiled", compiledErr))
	}
	p.hooks = bitmap

	inst.idleSince = time.Now()
	p.pool <- inst

	r.mu.Lock()
	r.plugins[name] = p
	r.mu.Unlock()
	r.firePublished(name)
	log.Printf("[wasm] loaded plugin %s (pool=%d timeout=%s memory_limit=%dMiB idle_timeout=%s)", name, p.poolSize, p.callTimeout, int(r.options.MemoryLimitPages)/16, p.idleTimeout)
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
// Cross-plugin cache exchange is a separate, explicit capability.
func metaKey(plugin, key string) string { return plugin + "\x00" + key }

func privateCacheKey(plugin, key string) string { return PrivateCacheKey(plugin, key) }

// PrivateCacheKey is the host's per-plugin cache framing, exported on the same
// terms as SharedCacheKey: only a test inspecting the backing cache.Store
// should need it, and it should ask rather than re-derive.
//
// The plugin name is length-prefixed so ("ab","c") and ("a","bc") cannot
// collide, and the whole prefix is host-supplied from the module identity, so a
// guest cannot reach another plugin's entries whatever bytes it puts in key.
func PrivateCacheKey(plugin, key string) string {
	return "private\x00" + strconv.Itoa(len(plugin)) + "\x00" + plugin + key
}

func sharedCacheKey(key string) string { return SharedCacheKey(key) }

// SharedCacheKey is the host's shared-cache domain framing, exported ONLY so a
// test that inspects the backing cache.Store directly asks the host where a
// guest's shared key landed instead of re-deriving it.
//
// A test that hardcodes "shared\x00"+key passes until the framing changes and
// then fails for a reason that looks like a plugin bug. Worse, an EXISTENCE
// probe (`if _, ok := store.Get(k); ok { fail }`) written against the wrong key
// keeps passing forever — it is asserting that a key nobody writes is absent.
// Both happened when domains were introduced; see internal/plugin's coordinated
// intent tests.
//
// Callers outside a test have no reason for this: guests reach the shared
// domain through env.shared_cache_get / env.shared_cache_set, which apply the
// framing themselves.
func SharedCacheKey(key string) string { return "shared\x00" + key }

func (r *Runtime) installHostFunctions() {
	env := r.runtime.NewHostModuleBuilder("env")

	// There is deliberately NO raw env.meta_get export, and no env.abort.
	//
	// Both were unchecked side doors: meta_get read request metadata with no
	// grant check at all, so a handwritten guest could declare only
	// env.meta_set and still read — defeating the per-command boundary the
	// dispatcher enforces. abort logged without env.log, so a guest could spam
	// host logs it was never granted.
	//
	// Neither is imported by either current ABI SDK. Everything now goes through
	// host_call, where the grant is checked per plugin.

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
		var labelsJSON []byte
		if labelsLen > 0 {
			labelsJSON = []byte(readStr(mod, labelsPtr, labelsLen))
		}
		metrics.EmitPluginMetricJSON(ctx, pluginName, name, int(metricType), value, labelsJSON)
	}).Export("emit_metric")

	// host_call — permission-enforced per-command.
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, cmdPtr, cmdLen, argsPtr, argsLen uint32) uint64 {
		cmd := readStr(mod, cmdPtr, cmdLen)
		args := readStr(mod, argsPtr, argsLen)
		return writeBytes(ctx, mod, r.dispatchHostCall(ctx, pluginNameOf(mod), cmd, args))
	}).Export("host_call")

	env.Instantiate(r.ctx)
}

func compactionPricingResources(p *Plugin, report economics.CompactionReport) (PricingResource, *PricingResource, *pbv1.HostError) {
	resources := p.resourceSnapshot().PricingResources
	target, ok := resources[report.PricingResource]
	if !ok {
		return PricingResource{}, nil, hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "pricing resource %q is not approved", report.PricingResource)
	}
	if report.Summarizer == nil {
		return target, nil, nil
	}
	summarizer, ok := resources[report.Summarizer.PricingResource]
	if !ok {
		return PricingResource{}, nil, hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "pricing resource %q is not approved", report.Summarizer.PricingResource)
	}
	return target, &summarizer, nil
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
		case pbv1.MetaAppendCommand:
			perm = pbv1.MetaAppendPermission
		case pbv1.StateDeleteCommand:
			perm = pbv1.StateDeletePermission
		}
		if p == nil || !p.hasGrant(perm) {
			log.Printf("[wasm] permission denied: %s tried %s", pluginName, perm)
			// A framed refusal, not the v1 string. Guests decode
			// HostCallResult now, so the old envelope would surface as a
			// protocol error and a plugin could not tell a missing grant from
			// a broken boundary.
			return frameHostCall(nil,
				hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "permission denied: %s", perm))
		}

		// current ABI: every reply is a framed HostCallResult. Cases set value/herr; the
		// single exit below frames whichever was set. Extension commands put
		// their JSON body in the value arm — the BODY is opaque, the envelope
		// is not.
		var value []byte
		var herr *pbv1.HostError
		// res carries DOMAIN RESULTS only — refusals are framed classified
		// hostErr, never a status string smuggled through the value arm.
		var res string // domain JSON, moved into value at the exit
		switch cmd {
		case "env.block_request":
			var a pbv1.BlockRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid BlockRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setBlock(pluginName, &a)
		case "env.respond_request":
			var a pbv1.RespondRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid RespondRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setRespond(pluginName, a.Content)
		case "env.route_request":
			var a pbv1.RouteRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid RouteRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setRoute(pluginName, a.Provider, a.Model)
		case "env.set_identity":
			var a pbv1.SetIdentityArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid SetIdentityArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.verdictsBucket(reqIDFrom(ctx)).setIdentity(pluginName, a.Identity)
		case pbv1.MetaAppendCommand:
			var a pbv1.MetaAppendArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid MetaAppendArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// One atomic call. A meta_get + meta_set pair was two round trips
			// and a lost update: two fragments interleaving between the read
			// and the write silently drop one, and the corrupted tool call
			// surfaces much later as invalid JSON reaching the agent.
			existing, present, err := r.metaAppend(reqIDFrom(ctx),
				metaKey(pluginName, "append:"+strconv.FormatInt(int64(a.BlockIndex), 10)), a.Fragment)
			if err != nil {
				// Refused, not truncated: a truncated tool call is worse than
				// a refused one, because the agent will try to execute it.
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// Non-empty fragment acks with an empty value; an empty fragment
			// reads the buffer back. Returning the cumulative buffer after
			// every delta would be O(total x fragments) on the stream path.
			value = pbv1.MetaAppendSuccessValue(a.Fragment, []byte(existing), present)
		case "env.meta_set":
			// A decode failure used to be swallowed by `if err == nil`, so the
			// write silently did not happen. It is now a classified refusal.
			var a pbv1.MetaSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid MetaSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// An empty value STORES an empty value; it is not a delete.
			if err := r.metaSetBounded(reqIDFrom(ctx), metaKey(pluginName, a.Key), a.Value); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
			}
		case "env.meta_get":
			var a pbv1.MetaGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid MetaGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			v, present := r.metaGetPresence(reqIDFrom(ctx), metaKey(pluginName, a.Key))
			if !present {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "metadata key not found")
				break
			}
			value = []byte(v)
		case "env.cache_set":
			var a pbv1.CacheSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CacheSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			cacheIdentity, valid := p.cacheIdentity()
			if !valid {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "plugin resource cache scope is invalid")
				break
			}
			r.cache.Set(privateCacheKey(cacheIdentity, a.Key), a.Value)
		case "env.cache_get":
			var a pbv1.CacheGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CacheGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			// The presence bool was previously discarded, so a miss and a
			// stored empty string were the same answer.
			cacheIdentity, valid := p.cacheIdentity()
			if !valid {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "plugin resource cache scope is invalid")
				break
			}
			v, present := r.cache.Get(privateCacheKey(cacheIdentity, a.Key))
			if !present {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "cache key not found")
				break
			}
			value = []byte(v)
		case "env.shared_cache_set":
			var a pbv1.CacheSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CacheSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			r.cache.Set(sharedCacheKey(a.Key), a.Value)
		case "env.shared_cache_get":
			var a pbv1.CacheGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CacheGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			v, present := r.cache.Get(sharedCacheKey(a.Key))
			if !present {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "shared cache key not found")
				break
			}
			value = []byte(v)
		case "env.state_set":
			// Durable, plugin-private, survives a restart. The plugin name
			// comes from the module, never the payload, so one plugin cannot
			// write into another's namespace.
			var a pbv1.StateSetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid StateSetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			if r.StateSetFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			// An empty value STORES an empty value. v1 deleted here, which is
			// why deletion is now its own command.
			if err := r.StateSetFunc(pluginName, a.Key, a.Value); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "%v", err)
			}
		case pbv1.StateDeleteCommand:
			var a pbv1.StateDeleteArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid StateDeleteArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			if r.StateDeleteFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			// Deleting an absent key succeeds: the caller wants it gone.
			if err := r.StateDeleteFunc(pluginName, a.Key); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "%v", err)
			}
		case "env.state_get":
			var a pbv1.StateGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid StateGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			if r.StateGetFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
				break
			}
			v, present := r.StateGetFunc(pluginName, a.Key)
			if !present {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "state key not found")
				break
			}
			value = []byte(v)
		case "env.state_keys":
			if r.StateKeysFunc == nil {
				// An empty list said "no keys", which is not the same as "no
				// store" — a plugin would conclude its writes had vanished.
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "durable plugin state is not configured")
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
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin egress is not configured")
				break
			}
			value, herr = r.applyExtensionResult("torana_send_request", r.SendRequestFunc(ctx, pluginName, args))
		case "torana_cache_pricing":
			if r.CachePricingFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "cache pricing is not configured")
				break
			}
			value, herr = r.applyExtensionResult("torana_cache_pricing", r.CachePricingFunc(ctx, args))
		case "env.plugin_config":
			// Return this plugin's config blob (plugins.config.<name>).
			cfg := p.pluginConfig()
			if cfg == "" {
				cfg = "{}"
			}
			value = []byte(cfg)
		case "env.credential_get":
			var a pbv1.CredentialGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid CredentialGetArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			credentialID, approved := p.resourceSnapshot().Credentials[a.Slot]
			if !approved {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "credential slot %q is not approved", a.Slot)
				break
			}
			if r.CredentialGetFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "credential resolution is not configured")
				break
			}
			resolved, err := r.CredentialGetFunc(ctx, pluginName, credentialID)
			if err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "credential slot %q is unavailable", a.Slot)
				break
			}
			value = append([]byte(nil), resolved...)
		case "env.file_append":
			var a pbv1.FileAppendArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid FileAppendArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().Files[a.Path]
			if !approved || !resource.Operations["append"] {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "file %q is not approved for append", a.Path)
				break
			}
			if r.FileAppendFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin files are not configured")
				break
			}
			if err := r.FileAppendFunc(pluginName, a.Path, a.Data, resource); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "file append failed")
			}
		case "env.file_read":
			var a pbv1.FileReadArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid FileReadArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().Files[a.Path]
			if !approved || !resource.Operations["read"] {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "file %q is not approved for read", a.Path)
				break
			}
			if r.FileReadFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin files are not configured")
				break
			}
			read, err := r.FileReadFunc(pluginName, a.Path, resource)
			if err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "file read failed")
				break
			}
			value = read
		case "env.file_write":
			var a pbv1.FileWriteArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid FileWriteArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().Files[a.Path]
			if !approved || !resource.Operations["write"] {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "file %q is not approved for write", a.Path)
				break
			}
			if r.FileWriteFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin files are not configured")
				break
			}
			if err := r.FileWriteFunc(pluginName, a.Path, a.Data, resource); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "file write failed")
			}
		case "env.file_delete":
			var a pbv1.FileDeleteArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid FileDeleteArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().Files[a.Path]
			if !approved || !resource.Operations["delete"] {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "file %q is not approved for delete", a.Path)
				break
			}
			if r.FileDeleteFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin files are not configured")
				break
			}
			if err := r.FileDeleteFunc(pluginName, a.Path, resource); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "file delete failed")
			}
		case "env.file_list":
			var a pbv1.FileListArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid FileListArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resources := p.resourceSnapshot().Files
			allowed := make(map[string]FileResource)
			for path, resource := range resources {
				if resource.Operations["list"] && strings.HasPrefix(path, a.Prefix) {
					allowed[path] = resource
				}
			}
			if len(allowed) == 0 {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "file prefix %q is not approved for list", a.Prefix)
				break
			}
			if r.FileListFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin files are not configured")
				break
			}
			paths, err := r.FileListFunc(pluginName, a.Prefix, allowed)
			if err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "file list failed")
				break
			}
			value, _ = proto.Marshal(&pbv1.FileListResult{Paths: paths})
		case "env.http_request":
			var a pbv1.OutboundHTTPRequestArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid OutboundHTTPRequestArgs: %v", err)
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().HTTP[a.Endpoint]
			if !approved || !resource.Methods[a.Method] {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "endpoint %q is not approved for %s", a.Endpoint, a.Method)
				break
			}
			if r.HTTPRequestFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin HTTP is not configured")
				break
			}
			response, err := r.HTTPRequestFunc(ctx, pluginName, resource, &a)
			if err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "HTTP request failed")
				break
			}
			if response == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "HTTP request returned no response")
				break
			}
			if err := response.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "HTTP response invalid")
				break
			}
			value, _ = proto.Marshal(response)
		case "env.model_complete":
			var a pbv1.ModelCompleteArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid ModelCompleteArgs")
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().ModelServices[a.Service]
			if !approved {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "model service %q is not approved", a.Service)
				break
			}
			if r.ModelCompleteFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "model services are not configured")
				break
			}
			response, callErr := r.ModelCompleteFunc(ctx, pluginName, resource, &a)
			if callErr != nil {
				herr = callErr
				break
			}
			if response == nil || response.Validate() != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model service returned an invalid response")
				break
			}
			value, _ = proto.Marshal(response)
		case "env.model_pricing":
			var a pbv1.ModelPricingGetArgs
			if err := proto.Unmarshal([]byte(args), &a); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid ModelPricingGetArgs")
				break
			}
			if err := a.Validate(); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
				break
			}
			resource, approved := p.resourceSnapshot().PricingResources[a.Resource]
			if !approved {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "pricing resource %q is not approved", a.Resource)
				break
			}
			if r.ModelPricingFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "model pricing is not configured")
				break
			}
			pricing, callErr := r.ModelPricingFunc(ctx, pluginName, resource)
			if callErr != nil {
				herr = callErr
				break
			}
			if pricing == nil || pricing.Validate() != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "pricing resource returned invalid data")
				break
			}
			value, _ = proto.Marshal(pricing)
		case "env.original_request":
			// Pristine pre-pipeline request, pb-encoded. Absence is NOT_FOUND,
			// not an empty value: an all-default ChatRequest legitimately
			// marshals to zero bytes.
			if r.OriginalRequestFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no original request captured")
				break
			}
			raw, captured := r.OriginalRequestFunc(ctx)
			if !captured {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no original request captured")
				break
			}
			value = raw
		case "env.original_response":
			// Raw upstream response body (non-streaming only). An upstream body
			// can legitimately be empty, so absence is again the error arm.
			if r.OriginalResponseFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no original response captured")
				break
			}
			raw, captured := r.OriginalResponseFunc(ctx)
			if !captured {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no original response captured")
				break
			}
			value = raw
		case "torana_db_query":
			herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "database not configured — set plugins.config.compactor.dsn")
		case "torana_kms_decrypt":
			herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "KMS not configured — set TORANA_KMS_ENDPOINT")
		case "torana_record_savings":
			var report economics.CompactionReport
			if err := json.Unmarshal([]byte(args), &report); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
				break
			}
			report.Normalize()
			if !report.Valid() {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
			} else if target, summarizer, resourceErr := compactionPricingResources(p, report); resourceErr != nil {
				herr = resourceErr
			} else if r.CompactionReportFunc != nil {
				r.CompactionReportFunc(ctx, pluginName, report, target, summarizer)
				// Success is an EMPTY value arm: the savings were recorded, and
				// there is no domain body to acknowledge with.
			} else {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "savings tracking not configured")
			}
		case "torana_plugin_counter":
			var counter struct {
				Counter string `json:"counter"`
				Delta   int64  `json:"delta"`
			}
			if err := json.Unmarshal([]byte(args), &counter); err != nil || counter.Counter == "" {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
			} else if r.PluginCounterFunc != nil {
				r.PluginCounterFunc(pluginName, counter.Counter, counter.Delta)
				// Success is an EMPTY value arm: the counter was incremented, and
				// there is no domain body to acknowledge with.
			} else {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "plugin counter tracking not configured")
			}
		case "torana_evaluate_compaction":
			var report economics.CompactionReport
			if err := json.Unmarshal([]byte(args), &report); err != nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
				break
			}
			report.Normalize()
			if !report.Valid() {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
			} else if target, summarizer, resourceErr := compactionPricingResources(p, report); resourceErr != nil {
				herr = resourceErr
			} else if r.EvaluateCompactionFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "compaction pricing is not configured")
			} else {
				decision := r.EvaluateCompactionFunc(ctx, report, target, summarizer)
				payload, _ := json.Marshal(decision)
				res = string(payload)
			}
		case "verify_virtual_key":
			if r.VerifyVirtualKeyFunc == nil {
				herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "virtual key verification is not configured")
				break
			}
			value, herr = r.applyExtensionResult("verify_virtual_key", r.VerifyVirtualKeyFunc(ctx, args))
		default:
			herr = hostErr(pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, "unknown host call %q", cmd)
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
// frameHostCall builds the HostCallResult a plugin guest decodes.
//
// Exactly one arm is set. A nil value with no error is a successful EMPTY
// value, which is distinct from an error and from absence — that distinction
// is the whole reason the envelope exists, so it must survive here.
func frameHostCall(value []byte, herr *pbv1.HostError) []byte {
	result := &pbv1.HostCallResult{}
	if herr != nil {
		result.Result = &pbv1.HostCallResult_Error{Error: herr}
	} else {
		result.Result = &pbv1.HostCallResult_Value{Value: value}
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
func hostErr(code pbv1.ErrorCode, format string, args ...any) *pbv1.HostError {
	return &pbv1.HostError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func writeBytes(ctx context.Context, mod api.Module, b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}

	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		log.Printf("[wasm] writeBytes: missing alloc function in module %s", mod.Name())
		return 0
	}

	res, err := allocFn.Call(ctx, uint64(len(b)))
	if err != nil {
		log.Printf("[wasm] writeBytes: alloc failed: %v", err)
		return 0
	}
	if len(res) == 0 {
		return 0
	}

	if res[0] > math.MaxUint32 {
		log.Printf("[wasm] writeBytes: alloc returned invalid pointer in module %s", mod.Name())
		return 0
	}
	ptr := uint32(res[0])
	if !writeMemory(mod.Memory(), ptr, b) {
		log.Printf("[wasm] writeBytes: alloc returned out-of-bounds pointer in module %s", mod.Name())
		return 0
	}
	return uint64(ptr)<<32 | uint64(len(b))
}
