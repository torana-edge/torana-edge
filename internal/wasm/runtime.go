package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type Plugin struct {
	name    string
	mod     api.Module
	alloc   api.Function
	dealloc api.Function
	mu      sync.Mutex
}

func (p *Plugin) Name() string { return p.name }

type Runtime struct {
	ctx     context.Context
	runtime wazero.Runtime
	metaMu  sync.RWMutex
	meta    map[string]string
	cacheMu sync.RWMutex
	cache   map[string]string
}

func NewRuntime(ctx context.Context) *Runtime {
	r := &Runtime{
		ctx:     ctx,
		runtime: wazero.NewRuntime(ctx),
		meta:    make(map[string]string),
		cache:   make(map[string]string),
	}
	r.installHostFunctions()
	return r
}

func (r *Runtime) Close() error { return r.runtime.Close(r.ctx) }

func (r *Runtime) LoadPlugin(name string, wasmBytes []byte) (*Plugin, error) {
	wasi_snapshot_preview1.MustInstantiate(r.ctx, r.runtime)
	mod, err := r.runtime.InstantiateWithConfig(r.ctx, wasmBytes,
		wazero.NewModuleConfig().WithName(name))
	if err != nil {
		return nil, fmt.Errorf("wasm: instantiate %s: %w", name, err)
	}
	alloc := mod.ExportedFunction("alloc")
	dealloc := mod.ExportedFunction("dealloc")
	if alloc == nil || dealloc == nil {
		return nil, fmt.Errorf("wasm: %s missing alloc/dealloc", name)
	}
	log.Printf("[wasm] loaded plugin %s", name)
	return &Plugin{name: name, mod: mod, alloc: alloc, dealloc: dealloc}, nil
}

func (p *Plugin) CallRequest(ctx context.Context, hook string, input, output any) error {
	b, _ := json.Marshal(input)
	return p.call(ctx, hook, b, output)
}

func (p *Plugin) call(ctx context.Context, hook string, in []byte, out any) error {
	fn := p.mod.ExportedFunction(hook)
	if fn == nil {
		return nil // optional hook, skip
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, err := p.alloc.Call(ctx, uint64(len(in)))
	if err != nil {
		return err
	}
	ptr := uint32(r[0])
	p.mod.Memory().Write(ptr, in)
	ret, err := fn.Call(ctx, uint64(ptr), uint64(len(in)))
	p.dealloc.Call(ctx, uint64(ptr), uint64(len(in)))
	if err != nil {
		return err
	}
	if len(ret) >= 2 && ret[0] != 0 {
		b, _ := p.mod.Memory().Read(uint32(ret[0]), uint32(ret[1]))
		return json.Unmarshal(b, out)
	}
	return nil
}

func (r *Runtime) installHostFunctions() {
	env := r.runtime.NewHostModuleBuilder("env")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {
		key := readStr(mod, kPtr, kLen)
		r.metaMu.RLock()
		v := r.meta[key]
		r.metaMu.RUnlock()
		return writeStr(mod, v)
	}).Export("meta_get")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen, vPtr, vLen uint32) {
		key := readStr(mod, kPtr, kLen)
		val := readStr(mod, vPtr, vLen)
		r.metaMu.Lock()
		r.meta[key] = val
		r.metaMu.Unlock()
	}).Export("meta_set")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {
		key := readStr(mod, kPtr, kLen)
		r.cacheMu.RLock()
		v := r.cache[key]
		r.cacheMu.RUnlock()
		return writeStr(mod, v)
	}).Export("cache_get")

	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen, vPtr, vLen uint32) {
		key := readStr(mod, kPtr, kLen)
		val := readStr(mod, vPtr, vLen)
		r.cacheMu.Lock()
		r.cache[key] = val
		r.cacheMu.Unlock()
	}).Export("cache_set")

	env.Instantiate(r.ctx)
}

func readStr(mod api.Module, ptr, length uint32) string {
	b, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(b)
}

func writeStr(mod api.Module, s string) uint64 {
	b := []byte(s)
	if len(b) == 0 {
		return 0
	}
	ptr := mod.Memory().Size()
	mod.Memory().Write(ptr, b)
	return uint64(ptr)<<32 | uint64(len(b))
}
