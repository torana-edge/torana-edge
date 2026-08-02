package wasm

import pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"

// MinimalV2Module builds the smallest module the v2 host will load: alloc,
// run_hook(ptr,size)->i64 and supported_hooks()->i64.
//
// It lives beside the runtime rather than in a _test.go file because other
// internal packages need it — a proxy test that just wants "a loadable plugin"
// would otherwise hand-assemble its own WASM bytes, and two copies of
// hand-written section lengths is the maintenance trap this replaced.
//
// loopForever makes run_hook spin, for cancellation coverage. Otherwise it
// returns 0, which is ABI pass-through.
func MinimalV2Module(loopForever bool) []byte {
	return minimalModule(loopForever, true)
}

// MinimalV2ModuleTrapsAtInit builds a module that COMPILES successfully and
// deterministically fails during instantiation: a start section runs
// run_hook's body, which executes unreachable. The lifecycle construction-
// failure test asserts the error is post-compile via the
// compiled-acquired -> compiled-released -> construct-failed stream, without
// any large-memory fixture.
func MinimalV2ModuleTrapsAtInit() []byte {
	sec := func(id byte, body []byte) []byte {
		return append([]byte{id, byte(len(body))}, body...)
	}
	name := func(s string) []byte {
		return append([]byte{byte(len(s))}, s...)
	}
	types := sec(0x01, []byte{
		0x03,
		0x60, 0x01, 0x7f, 0x01, 0x7f, // (i32)->i32: alloc
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e, // (i32,i32)->i64: run_hook
		0x60, 0x00, 0x00, // ()->(): the start trap function (must be nullary)
	})
	funcs := sec(0x03, []byte{0x03, 0x00, 0x01, 0x02})
	mem := sec(0x05, []byte{0x01, 0x00, 0x01})
	exports := []byte{0x03}
	exports = append(exports, append(name("memory"), 0x02, 0x00)...)
	exports = append(exports, append(name("alloc"), 0x00, 0x00)...)
	exports = append(exports, append(name("run_hook"), 0x00, 0x01)...)
	start := sec(0x08, []byte{0x02}) // start function index 2: traps
	code := sec(0x0a, []byte{
		0x03,
		0x04, 0x00, 0x41, 0x00, 0x0b, // alloc: size 4, locals 0, i32.const 0, end
		0x04, 0x00, 0x42, 0x00, 0x0b, // run_hook: size 4, locals 0, i64.const 0, end
		0x03, 0x00, 0x00, 0x0b, // trap fn: size 3, locals 0, unreachable, end
	})
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, types...)
	out = append(out, funcs...)
	out = append(out, mem...)
	out = append(out, sec(0x07, exports)...)
	out = append(out, start...)
	return append(out, code...)
}

// MinimalV2ModuleNoHooks builds the same shape as MinimalV2Module but omits
// the supported_hooks export: compilation and instantiation succeed, and the
// hook-discovery step (supportedHooks) fails. Used to prove the post-compile
// hook-discovery error path releases the compiled handle.
func MinimalV2ModuleNoHooks() []byte {
	return minimalModule(false, false)
}

func minimalModule(loopForever, withHooks bool) []byte {
	sec := func(id byte, body []byte) []byte {
		return append([]byte{id, byte(len(body))}, body...)
	}
	name := func(s string) []byte {
		return append([]byte{byte(len(s))}, s...)
	}

	// Types: 0 = (i32)->i32 for alloc, 1 = (i32,i32)->i64 for run_hook,
	// 2 = ()->i64 for supported_hooks. run_hook takes TWO arguments: v2 moved
	// the request id into HookInput.
	types := sec(0x01, []byte{
		0x03,
		0x60, 0x01, 0x7f, 0x01, 0x7f,
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e,
		0x60, 0x00, 0x01, 0x7e,
	})
	funcs := sec(0x03, []byte{0x03, 0x00, 0x01, 0x02})
	if !withHooks {
		funcs = sec(0x03, []byte{0x02, 0x00, 0x01})
	}
	mem := sec(0x05, []byte{0x01, 0x00, 0x01})

	exports := []byte{0x04}
	if !withHooks {
		exports = []byte{0x03}
	}
	exports = append(exports, append(name("memory"), 0x02, 0x00)...)
	exports = append(exports, append(name("alloc"), 0x00, 0x00)...)
	exports = append(exports, append(name("run_hook"), 0x00, 0x01)...)
	if withHooks {
		exports = append(exports, append(name("supported_hooks"), 0x00, 0x02)...)
	}

	// run_hook: either spin forever, or return 0 (ABI pass-through).
	runHook := []byte{0x04, 0x00, 0x42, 0x00, 0x0b} // size=4: locals, i64.const 0, end
	if loopForever {
		runHook = []byte{0x08, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x00, 0x0b}
	}

	codeBody := []byte{
		0x03,
		0x04, 0x00, 0x41, 0x00, 0x0b, // alloc: i32.const 0
	}
	if !withHooks {
		codeBody = []byte{
			0x02,
			0x04, 0x00, 0x41, 0x00, 0x0b, // alloc: i32.const 0
		}
	}
	codeBody = append(codeBody, runHook...)
	if withHooks {
		codeBody = append(codeBody,
			// supported_hooks: claim before-request so dispatch does not skip it.
			0x04, 0x00, 0x42, byte(pb.Hook_HOOK_BEFORE_REQUEST.Bit()), 0x0b)
	}
	code := sec(0x0a, codeBody)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, types...)
	out = append(out, funcs...)
	out = append(out, mem...)
	out = append(out, sec(0x07, exports)...)
	return append(out, code...)
}

// ModuleImportingEnvFunc builds a guest that imports env.<name> as a
// (i32,i32)->i64 function, plus the v2 exports.
//
// Used to prove a handwritten guest cannot reach a host function that no
// longer exists. Asserting on the host module's export list is not possible —
// wazero forbids inspecting host modules — and would be the weaker claim
// anyway: what matters is whether a guest can link against it.
func ModuleImportingEnvFunc(name string) []byte {
	// Real signatures, so a restored side door produces an UNKNOWN-IMPORT
	// error rather than a signature mismatch. Declaring everything as
	// (i32,i32)->i64 meant a restored env.abort — four i32s, no result —
	// failed to link on the signature, and a test treating any error as
	// "absent" could not detect the regression it exists to catch.
	params, results := 2, 1
	switch name {
	case "abort":
		params, results = 4, 0
	case "host_call":
		params, results = 4, 1
	}
	return moduleImporting(name, params, results)
}

func moduleImporting(name string, params, results int) []byte {
	sec := func(id byte, body []byte) []byte {
		return append([]byte{id, byte(len(body))}, body...)
	}
	str := func(s string) []byte { return append([]byte{byte(len(s))}, s...) }

	// Types: 0 (i32)->i32 alloc, 1 (i32,i32)->i64 run_hook,
	// 2 ()->i64 supported_hooks, 3 the import's real signature.
	importType := []byte{0x60, byte(params)}
	for i := 0; i < params; i++ {
		importType = append(importType, 0x7f) // i32
	}
	importType = append(importType, byte(results))
	for i := 0; i < results; i++ {
		importType = append(importType, 0x7e) // i64
	}
	typeBody := []byte{0x04,
		0x60, 0x01, 0x7f, 0x01, 0x7f,
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e,
		0x60, 0x00, 0x01, 0x7e,
	}
	types := sec(0x01, append(typeBody, importType...))

	imp := []byte{0x01}
	imp = append(imp, str("env")...)
	imp = append(imp, str(name)...)
	imp = append(imp, 0x00, 0x03) // func, the import's own type
	imports := sec(0x02, imp)

	// Local funcs come after the imported one, so indexes shift by 1.
	funcs := sec(0x03, []byte{0x03, 0x00, 0x01, 0x02})
	mem := sec(0x05, []byte{0x01, 0x00, 0x01})

	exports := []byte{0x04}
	exports = append(exports, append(str("memory"), 0x02, 0x00)...)
	exports = append(exports, append(str("alloc"), 0x00, 0x01)...)
	exports = append(exports, append(str("run_hook"), 0x00, 0x02)...)
	exports = append(exports, append(str("supported_hooks"), 0x00, 0x03)...)

	code := sec(0x0a, []byte{
		0x03,
		0x04, 0x00, 0x41, 0x00, 0x0b,
		0x04, 0x00, 0x42, 0x00, 0x0b,
		0x04, 0x00, 0x42, byte(pb.Hook_HOOK_BEFORE_REQUEST.Bit()), 0x0b,
	})

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, types...)
	out = append(out, imports...)
	out = append(out, funcs...)
	out = append(out, mem...)
	out = append(out, sec(0x07, exports)...)
	return append(out, code...)
}
