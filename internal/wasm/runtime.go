package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/torana-edge/torana-edge/internal/metrics"
)

type Plugin struct {
	name   string
	mod    api.Module
	grants map[string]bool
	mu     sync.Mutex
}

func (p *Plugin) Name() string { return p.name }
func (p *Plugin) SetGrants(g []string) {
	p.grants = make(map[string]bool)
	for _, x := range g { p.grants[x] = true }
}

// CallRequest marshals input, passes it to the WASM hook, and unmarshals result.
func (p *Plugin) CallRequest(ctx context.Context, hook string, input, output any) error {
	fn := p.mod.ExportedFunction(hook)
	if fn == nil { return nil }
	allocFn := p.mod.ExportedFunction("alloc")
	if allocFn == nil { return fmt.Errorf("wasm: %s missing alloc", p.name) }
	deallocFn := p.mod.ExportedFunction("dealloc")

	inBytes, _ := json.Marshal(input)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Allocate in WASM linear memory.
	r, err := allocFn.Call(ctx, uint64(len(inBytes)))
	if err != nil { return err }
	inPtr := uint32(r[0])

	// Write to WASM linear memory.
	p.mod.Memory().Write(inPtr, inBytes)

	// Call hook.
	ret, err := fn.Call(ctx, uint64(inPtr), uint64(len(inBytes)))
	if deallocFn != nil { deallocFn.Call(ctx, uint64(inPtr), uint64(len(inBytes))) }
	if err != nil { return err }

	// Read result from WASM linear memory.
	if len(ret) > 0 && ret[0] != 0 {
		v := ret[0]
		outPtr := uint32(v >> 32)
		outLen := uint32(v & 0xFFFFFFFF)
		if outPtr != 0 && outLen > 0 {
			b, ok := p.mod.Memory().Read(outPtr, outLen)
			if ok { 
				err := json.Unmarshal(b, output)
				if deallocFn != nil { deallocFn.Call(ctx, uint64(outPtr), uint64(outLen)) }
				return err
			}
		}
	}
	return nil
}

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
	mod, err := r.runtime.InstantiateWithConfig(r.ctx, wasmBytes,
		wazero.NewModuleConfig().WithName(name).
			WithSysWalltime().
			WithSysNanotime().
			WithStdout(os.Stdout).
			WithStderr(os.Stderr))
	if err != nil { return nil, fmt.Errorf("wasm: %s: %w", name, err) }

	// Must call _initialize for reactor libraries before alloc works.
	init := mod.ExportedFunction("_initialize")
	if init != nil {
		if _, err := init.Call(r.ctx); err != nil {
			return nil, fmt.Errorf("wasm: %s _initialize: %w", name, err)
		}
	}

	p := &Plugin{name: name, mod: mod}
	r.mu.Lock(); r.plugins[name] = p; r.mu.Unlock()
	log.Printf("[wasm] loaded plugin %s", name)
	return p, nil
}

func (r *Runtime) installHostFunctions() {
	env := r.runtime.NewHostModuleBuilder("env")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {
		key := readStr(mod, kPtr, kLen)
		r.metaMu.RLock(); v := r.meta[key]; r.metaMu.RUnlock()
		return writeStr(mod, v)
	}).Export("meta_get")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, level int32, ptr, length uint32) {
		msg := readStr(mod, ptr, length)
		if level == 0 {
			log.Printf("[plugin debug] %s", msg)
		} else {
			log.Printf("[plugin] %s", msg)
		}
	}).Export("log")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, metricType int32, ptr, length uint32, value float64) {
		name := readStr(mod, ptr, length)
		pluginName := mod.Name()
		metrics.EmitPluginMetric(ctx, pluginName, name, int(metricType), value)
	}).Export("emit_metric")

	env.Instantiate(r.ctx)
}

func readStr(mod api.Module, ptr, length uint32) string {
	b, ok := mod.Memory().Read(ptr, length)
	if !ok { return "" }
	return string(b)
}
func writeStr(mod api.Module, s string) uint64 {
	b := []byte(s)
	if len(b) == 0 { return 0 }
	ptr := mod.Memory().Size()
	mod.Memory().Write(ptr, b)
	return uint64(ptr)<<32 | uint64(len(b))
}
