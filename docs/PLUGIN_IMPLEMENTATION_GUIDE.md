# Torana WASM Plugin Implementation Guide

This guide is designed to help engineers and AI agents implement Torana WASM plugins robustly, avoiding common pitfalls related to the Torana plugin architecture, Go's WASI integration, and JSON serialization.

## 1. WASI Build Mode

Torana uses `wazero` for executing WASM plugins. For standard Go (not TinyGo), compiling to `wasip1/wasm` with standard commands yields a command-oriented execution model, which means the runtime shuts down after `main()` completes. This breaks `Torana`'s hook-based reactor execution model where Torana calls exported functions multiple times.

**CRITICAL:** Always compile your standard Go plugins as a C-shared library to enable the reactor model.
```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm plugin.wasm.go
```

## 2. JSON Structure and Torana's Payload

When Torana invokes `on_chat_request`, it passes a stringified version of the `engine.ChatRequest`. 

The payload your plugin receives looks like this:
```json
{
  "chat": "{\"Model\": \"claude-3-5-sonnet-20241022\", \"Messages\": [...], \"Tools\": [...]}"
}
```

**CRITICAL:** Do NOT attempt to unmarshal the Torana payload directly into an `engine.ChatRequest` or a struct mirroring it. If you do, you will miss the outer `chat` wrapper.

### The Correct Unmarshaling Pattern

Use this two-step pattern to modify specific parts of the request without losing fields:

```go
sdk.OnChatRequest(func(input []byte) ([]byte, error) {
	// Step 1: Unwrap the Torana envelope
	var wrapper struct {
		Chat string `json:"chat"`
	}
	json.Unmarshal(input, &wrapper)
	if wrapper.Chat == "" { 
        return nil, nil // Nothing to modify
    }

	// Step 2: Decode the actual ChatRequest payload.
    // Use map[string]any to ensure that fields you don't explicitly handle 
    // (e.g. streaming, temperature) are preserved during serialization.
	var fullReq map[string]any
	json.Unmarshal([]byte(wrapper.Chat), &fullReq)

	// Step 3: Extract and modify the fields you care about
    modified := false
	if toolsAny, ok := fullReq["Tools"].([]any); ok {
        // modify toolsAny...
        modified = true
    }

	// Step 4: Short-circuit if no modifications are needed
	if !modified {
		return nil, nil
	}

	// Step 5: Repackage and return
	chatBytes, _ := json.Marshal(fullReq)
	wrapper.Chat = string(chatBytes)
	return json.Marshal(wrapper)
})
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
