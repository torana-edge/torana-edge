package wasm

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/torana-edge/torana-edge/internal/metrics"
)

// ============================================================================
// Plugin — WASM module with instance pooling and permission enforcement
// ============================================================================

// poolSize is the number of concurrent WASM instances per plugin.
// For single-user local use this is adequate; production should increase.
const poolSize = 4

type Plugin struct {
	name      string
	wasmBytes []byte
	grants    map[string]bool
	runtime   wazero.Runtime

	// Instance pool for concurrent request handling.
	pool   chan *pluginInstance
	poolMu sync.Mutex
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

func (p *Plugin) hasGrant(perm string) bool {
	if p.grants == nil {
		return false // fail-closed: no grants = no permissions
	}
	return p.grants[perm]
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

	mod, err := p.runtime.InstantiateWithConfig(ctx, p.wasmBytes,
		wazero.NewModuleConfig().WithName(p.name).
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
func (p *Plugin) CallRequest(ctx context.Context, hook string, inBytes []byte, output *[]byte) error {
	fn, allocFn, deallocFn, err := p.resolveExports()
	if err != nil {
		return err
	}
	if fn == nil {
		return nil
	}

	// Acquire an instance from the pool.
	inst, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(inst)

	mod := inst.mod

	// Allocate in WASM linear memory.
	r, err := allocFn.Call(ctx, uint64(len(inBytes)))
	if err != nil {
		return err
	}
	inPtr := uint32(r[0])
	mod.Memory().Write(inPtr, inBytes)

	// Call hook.
	ret, err := fn.Call(ctx, uint64(inPtr), uint64(len(inBytes)))
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

// resolveExports looks up hook, alloc, and dealloc functions.
// The hook is resolved per-instance since it's exported from each module.
func (p *Plugin) resolveExports() (hook, alloc, dealloc api.Function, err error) {
	// Get a quick instance just to resolve exports.
	inst, err := p.acquire(context.Background())
	if err != nil {
		return nil, nil, nil, err
	}
	defer p.release(inst)

	hook = inst.mod.ExportedFunction("on_chat_request")
	alloc = inst.mod.ExportedFunction("alloc")
	if alloc == nil {
		return nil, nil, nil, fmt.Errorf("wasm: %s missing alloc", p.name)
	}
	dealloc = inst.mod.ExportedFunction("dealloc")
	return hook, alloc, dealloc, nil
}

// ============================================================================
// Runtime
// ============================================================================

type Runtime struct {
	ctx     context.Context
	runtime wazero.Runtime
	plugins map[string]*Plugin
	mu      sync.RWMutex
	metaMu  sync.RWMutex
	meta    map[string]string
	cacheMu sync.RWMutex
	cache   map[string]string
}

func NewRuntime(ctx context.Context) *Runtime {
	r := &Runtime{
		ctx:     ctx,
		runtime: wazero.NewRuntime(ctx),
		plugins: make(map[string]*Plugin),
		meta:    make(map[string]string),
		cache:   make(map[string]string),
	}
	wasi_snapshot_preview1.MustInstantiate(r.ctx, r.runtime)
	r.installHostFunctions()
	return r
}

func (r *Runtime) Close() error { return r.runtime.Close(r.ctx) }

func (r *Runtime) LoadPlugin(name string, wasmBytes []byte) (*Plugin, error) {
	p := &Plugin{
		name:      name,
		wasmBytes: wasmBytes,
		runtime:   r.runtime,
		pool:      make(chan *pluginInstance, poolSize),
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

func (r *Runtime) installHostFunctions() {
	env := r.runtime.NewHostModuleBuilder("env")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {
		key := readStr(mod, kPtr, kLen)
		r.metaMu.RLock()
		v := r.meta[key]
		r.metaMu.RUnlock()
		return writeStr(ctx, mod, v)
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

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, metricType int32, ptr, length uint32, value float64) {
		name := readStr(mod, ptr, length)
		pluginName := mod.Name()
		metrics.EmitPluginMetric(ctx, pluginName, name, int(metricType), value)
	}).Export("emit_metric")

	// host_call — permission-enforced per-command.
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, cmdPtr, cmdLen, argsPtr, argsLen uint32) uint64 {
		cmd := readStr(mod, cmdPtr, cmdLen)
		args := readStr(mod, argsPtr, argsLen)

		// Enforce per-command permission: env.host_call.<command>
		r.mu.RLock()
		p := r.plugins[mod.Name()]
		r.mu.RUnlock()
		perm := "env.host_call"
		if cmd != "" {
			perm = "env.host_call." + cmd
		}
		if p == nil || !p.hasGrant(perm) {
			log.Printf("[wasm] permission denied: %s tried %s", mod.Name(), perm)
			return writeStr(ctx, mod, `{"status":"error","message":"permission denied"}`)
		}

		var res string
		switch cmd {
		case "torana_db_query":
			res = `{"status":"ok","db_result":"stub"}`
		case "torana_kms_decrypt":
			res = `{"status":"ok","decrypted":"` + args + `"}`
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
