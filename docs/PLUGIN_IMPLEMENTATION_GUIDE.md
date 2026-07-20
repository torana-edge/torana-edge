# Torana WASM Plugin Implementation Guide

This guide is designed to help engineers and AI agents implement Torana WASM plugins robustly, avoiding common pitfalls related to the Torana plugin architecture, Go's WASI integration, and JSON serialization.

## 1. WASI Build Mode

Torana uses `wazero` for executing WASM plugins. For standard Go (not TinyGo), compiling to `wasip1/wasm` with standard commands yields a command-oriented execution model, which means the runtime shuts down after `main()` completes. This breaks `Torana`'s hook-based reactor execution model where Torana calls exported functions multiple times.

**CRITICAL:** Always compile your standard Go plugins as a C-shared library to enable the reactor model.
```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

**Plugin binaries are build artifacts and are NEVER committed to git** (`*.wasm`
is gitignored). Build all in-repo plugins with `make plugins`. Torana logs a
warning at load time if `plugin.wasm` is older than the plugin's Go sources.

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

## 3. Stream Hooks: Suppress, Replace, Fan-Out

`run_on_stream_chunk` handlers return a `*pb.StreamEventResult` describing
what replaces the input event. Use the SDK helpers:

| Helper | Meaning |
|---|---|
| `sdk.Pass()` (or `nil`) | forward the event unchanged |
| `sdk.Suppress()` | drop the event from the stream |
| `sdk.Replace(ev)` | substitute the event |
| `sdk.Emit(ev1, ev2, …)` | fan out multiple events in its place |

The canonical buffering pattern — reassemble fragmented tool-call arguments,
process them once, and emit a single complete delta:

```go
sdk.OnStreamChunk(func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEventResult, error) {
	if td := chunk.GetToolCallDelta(); td != nil {
		bufferFragment(td) // via env.meta_set — state is request-scoped
		return sdk.Suppress(), nil
	}
	if te := chunk.GetToolCallEnd(); te != nil {
		full := processArgs(assembleFragments(te.Index))
		// Fragments were suppressed, so the complete args MUST be emitted
		// here, followed by the ToolCallEnd itself.
		return sdk.Emit(deltaEvent(te.Index, full), chunk), nil
	}
	return sdk.Pass(), nil
})
```

**State scoping rules:**
- `env.meta_set` / `env.meta_get` — plugin-private AND request-scoped. Other
  plugins and other requests can never see these keys. Setting an empty
  value deletes the key.
- `env.cache_set` / `env.cache_get` — shared across plugins and requests
  (with a TTL). Use for cross-request handoff, e.g. the compactor caches
  intents by `tool_call_id` that the keyword_compactor reads next turn.

## 4. Response Hooks: `run_after_response` semantics differ by path

`run_after_response` fires on **both** response paths, but with **asymmetric**
mutation semantics — know which one you're on before relying on it:

| Response path | Mutations (assistant content, tool-call name/args) |
|---|---|
| Non-streaming JSON | **Applied** — written back into the response body before the client sees it (`internal/proxy/jsonresponse.go`). |
| Streaming SSE | **Observational only** — the hook runs *after* the stream has already been serialized to the client, so any mutation is discarded (`internal/proxy/server.go`). |

**Why:** buffering an entire SSE stream to allow post-hoc rewrites would defeat
streaming latency, so the streaming path invokes the hook purely for observation.
This is the right channel for **metrics / audit / usage** plugins (e.g. `otel`),
which only read the `_response` signal (latency, upstream status, token usage).

**If your plugin needs to *rewrite* the final response**, do it on the streaming
path via `run_on_stream_chunk` (mutate events as they flow — see §3), not
`run_after_response`. Torana logs a heads-up at load time for any plugin that
declares `run_after_response`, reminding you the streaming mutations are dropped.

## 5. Wazero Engine Configuration

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

## 6. Prompt-Cache Compliance

Provider prompt caching bills cached input tokens at ~10% of full price — it is
the single biggest cost lever an agent session has, and a plugin can silently
destroy it. Two rules keep a plugin compliant:

**1. Never strip cache breakpoints.** `Message.cache_control_json` and
`ToolDef.cache_control_json` carry the client's cache markers (Anthropic
`cache_control`, Bedrock `cachePoint`) through the plugin boundary. They
survive automatically — a plugin that returns a request keeps them without
doing anything. But a plugin that *restructures* messages (splits, merges,
reorders, drops) must carry the marker to the equivalent position in its
output. The SDK helpers do this:

```go
cc := sdk.CacheControl(msg)          // read a message's breakpoint (nil if none)
sdk.SetCacheBreakpoint(msg, cc)      // attach / clear one
sdk.MoveCacheBreakpoint(from, to)    // transfer when merging messages
```

**2. Be deterministic over the cacheable prefix.** Everything before the last
breakpoint — tools, system prompt, conversation history — must serialize to
the *same bytes* on every request that replays the same history. OpenAI
caching is an exact-prefix match (one changed token busts it); Anthropic
hashes the rendered prompt up to each breakpoint. Concretely:

- No wall-clock time, randomness, request IDs, or counters in anything you
  inject before a breakpoint.
- Any value derived for a *historical* message must be a pure function of
  that message (the intent plugin's heuristic fill once mixed in a snippet of
  the latest user message — every turn re-serialized the same history to
  different bytes, busting the cache from that point on; it now derives only
  from the call's own name+args).
- One-time changes are fine: the first compaction of a tool result re-caches
  once and then stays stable (keyed by `tool_call_id`) — a net win. What's
  fatal is *per-turn* variance.
- Per-request state belongs in `ToranaMetaJson` (never serialized to the
  wire), not in messages or tool schemas.

The guardrail test `internal/plugin/cache_compliance_test.go` runs every
in-repo plugin twice over an identical request and asserts byte-identical
output, and asserts markers survive the round-trip. New plugins are picked up
by adding their name to the list — do so.
