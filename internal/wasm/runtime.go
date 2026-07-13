package wasm
import ("context";"encoding/json";"fmt";"log";"sync"
	"github.com/tetratelabs/wazero";"github.com/tetratelabs/wazero/api";"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1")

type Plugin struct {name string;mod api.Module;grants map[string]bool;mu sync.Mutex}
func (p *Plugin) Name() string { return p.name }
func (p *Plugin) SetGrants(g []string) {p.grants=make(map[string]bool);for _,x:=range g{p.grants[x]=true}}

func (p *Plugin) CallRequest(ctx context.Context, hook string, input, output any) error {
	fn := p.mod.ExportedFunction(hook)
	if fn == nil { return nil }
	allocFn := p.mod.ExportedFunction("alloc")
	if allocFn == nil { return fmt.Errorf("wasm: %s missing alloc", p.name) }
	inBytes, _ := json.Marshal(input)
	p.mu.Lock(); defer p.mu.Unlock()
	r, err := allocFn.Call(ctx, uint64(len(inBytes)))
	if err != nil { return err }
	inPtr := uint32(r[0])
	p.mod.Memory().Write(inPtr, inBytes)
	ret, err := fn.Call(ctx, uint64(inPtr), uint64(len(inBytes)))
	if err != nil { return err }
	// Unpack packed (ptr<<32)|len from single uint64 return
	if len(ret) > 0 && ret[0] != 0 {
		v := ret[0]
		outPtr, outLen := uint32(v>>32), uint32(v&0xFFFFFFFF)
		b, ok := p.mod.Memory().Read(outPtr, outLen)
		if ok && len(b) > int(outLen) { b = b[:outLen] }
		if ok && len(b) > 0 { return json.Unmarshal(b, output) }
	}
	return nil
}

type Runtime struct {ctx context.Context;runtime wazero.Runtime;plugins map[string]*Plugin;mu sync.RWMutex;metaMu sync.RWMutex;meta map[string]string;cacheMu sync.RWMutex;cache map[string]string}
func NewRuntime(ctx context.Context) *Runtime {r:=&Runtime{ctx:ctx,runtime:wazero.NewRuntime(ctx),plugins:make(map[string]*Plugin),meta:make(map[string]string),cache:make(map[string]string)};r.installHostFunctions();return r}
func (r *Runtime) Close() error { return r.runtime.Close(r.ctx) }
func (r *Runtime) LoadPlugin(name string, wasmBytes []byte) (*Plugin, error) {
	wasi_snapshot_preview1.MustInstantiate(r.ctx, r.runtime)
	mod, err := r.runtime.InstantiateWithConfig(r.ctx, wasmBytes, wazero.NewModuleConfig().WithName(name))
	if err != nil { return nil, fmt.Errorf("wasm: %s: %w", name, err) }
	init := mod.ExportedFunction("_initialize")
	if init != nil { init.Call(r.ctx) }
	p := &Plugin{name: name, mod: mod}
	r.mu.Lock(); r.plugins[name] = p; r.mu.Unlock()
	log.Printf("[wasm] loaded plugin %s", name)
	return p, nil
}
func (r *Runtime) installHostFunctions() {
	env := r.runtime.NewHostModuleBuilder("env")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {key:=readStr(mod,kPtr,kLen);r.metaMu.RLock();v:=r.meta[key];r.metaMu.RUnlock();return writeStr(mod,v)}).Export("meta_get")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen, vPtr, vLen uint32) {r.metaMu.Lock();r.meta[readStr(mod,kPtr,kLen)]=readStr(mod,vPtr,vLen);r.metaMu.Unlock()}).Export("meta_set")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen uint32) uint64 {key:=readStr(mod,kPtr,kLen);r.cacheMu.RLock();v:=r.cache[key];r.cacheMu.RUnlock();return writeStr(mod,v)}).Export("cache_get")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, kPtr, kLen, vPtr, vLen uint32) {r.cacheMu.Lock();r.cache[readStr(mod,kPtr,kLen)]=readStr(mod,vPtr,vLen);r.cacheMu.Unlock()}).Export("cache_set")
	env.NewFunctionBuilder().WithFunc(func(ctx context.Context, mod api.Module, lvPtr,lvLen,msgPtr,msgLen uint32) {log.Printf("[wasm:%s] %s: %s",mod.Name(),readStr(mod,lvPtr,lvLen),readStr(mod,msgPtr,msgLen))}).Export("log")
	env.Instantiate(r.ctx)
}
func readStr(mod api.Module, ptr, length uint32) string {b,ok:=mod.Memory().Read(ptr,length);if !ok{return ""};return string(b)}
func writeStr(mod api.Module, s string) uint64 {b:=[]byte(s);if len(b)==0{return 0};ptr:=mod.Memory().Size();mod.Memory().Write(ptr,b);return uint64(ptr)<<32|uint64(len(b))}
