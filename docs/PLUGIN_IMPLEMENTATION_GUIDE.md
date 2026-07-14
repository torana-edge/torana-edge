# Torana WASM Plugin Implementation Guide

This guide is designed to help engineers and AI agents implement Torana WASM plugins robustly, avoiding common pitfalls related to the Torana plugin architecture, Go's WASI integration, and JSON serialization.

## 1. WASI Build Mode

Torana uses `wazero` for executing WASM plugins. For standard Go (not TinyGo), compiling to `wasip1/wasm` with standard commands yields a command-oriented execution model, which means the runtime shuts down after `main()` completes. This breaks `Torana`'s hook-based reactor execution model where Torana calls exported functions multiple times.

**CRITICAL:** Always compile your standard Go plugins as a C-shared library to enable the reactor model.
```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm plugin.wasm.go
```

## 2. Protobuf Structure and Torana's Payload

Torana uses a strict Protobuf contract for all WASM boundaries to prevent schema corruption.
When Torana invokes `run_before_request`, it passes serialized bytes of `pb.ChatRequest`. 

The Go plugin SDK handles all the underlying memory allocation, pointer packing, and Protobuf marshaling for you.

**CRITICAL:** Do NOT attempt to read raw JSON or use `map[string]any`. You will lose the benefits of Protobuf unknown field preservation.

### The Correct Unmarshaling Pattern

Use the generated `pb` types and the `sdk` handlers. The SDK automatically unmarshals the request and marshals the response, fully preserving unknown fields under the hood.

```go
package main

import (
	"context"
	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		modified := false

		// Extract and modify the fields you care about
		if len(req.Tools) > 0 {
			// modify req.Tools...
			modified = true
		}

		// Short-circuit if no modifications are needed
		if !modified {
			return nil, nil
		}

		// Return the modified request
		return req, nil
	})
}
```

## 3. Wazero Engine Configuration

When setting up the host engine (in `internal/wasm/runtime.go`), note that any module performing timing operations (like garbage collection in standard Go) expects a system clock via WASI.

Torana's module instantiation must include:
```go
wazero.NewModuleConfig().WithName(name).
    WithSysWalltime().
    WithSysNanotime().
    WithStdout(os.Stdout).
    WithStderr(os.Stderr)
```
Failure to include `WithSysNanotime()` will result in `nil pointer dereference` panics inside `wasi_snapshot_preview1.clock_time_get` when Go attempts a GC pass.
