# Torana Edge — Known Architectural Gaps

This document outlines structural and architectural limitations in Torana Edge's current canonical IR and adapter pattern.

## 1. [RESOLVED] The "Lowest Common Denominator" IR Problem (Missing Parameters)
The canonical IR (`ChatRequest`) acts as a strict filter. Anything not explicitly defined in the IR is destroyed during the Unmarshal → Marshal cycle.
* **OpenAI:** Drops `response_format` (JSON mode), `tool_choice`, `presence_penalty`, `frequency_penalty`, and `seed`.
* **Anthropic:** Drops `tool_choice`, `metadata`, and prompt caching (`cache_control`).
* **Fix:** Introduce `ProviderExtensions map[string]any` to the IR to capture all unrecognized fields during unmarshaling and inject them back during marshaling. This ensures Torana operates transparently for advanced agent workflows.

## 2. Streaming Architecture Limitations (Bedrock & Vertex)
The current `StreamAdapter` interface assumes all upstream providers stream via SSE (Server-Sent Events) over `io.Pipe`. 
* **Bedrock:** AWS Bedrock uses a proprietary binary event stream protocol over HTTP/2, requiring the `ConverseStream` endpoint. Bedrock streaming is currently hardcoded to `false` and fundamentally unsupported by the SSE parser.
* **Vertex:** Vertex streams JSON array chunks rather than standard SSE. The Vertex stream adapter is mostly unimplemented.

## 3. [RESOLVED] Missing Context Cancellation
The `RequestHook` and `ResponseHook` pipeline interfaces do not accept a `context.Context`.
* **Impact:** If an agent disconnects mid-stream, Torana cannot propagate the cancellation to the pipeline hooks or the upstream provider. This causes goroutine leaks and wasted LLM provider costs as Torana downloads responses to a broken pipe.

## 4. Memory Footprint (No Zero-Copy Proxying)
Torana reads the entire request body into memory using `io.ReadAll` to convert it to the canonical IR.
* **Impact:** For massive context windows (e.g., 200k tokens + images), allocating megabytes of memory per request will cause high GC pressure and potential OOMs under concurrency. Torana currently acts as a heavy API gateway, not a lightweight zero-copy proxy.

## 5. Stream Mutation is Impossible
The `ResponseHook` architecture uses an `io.TeeReader` to passively "scan" the SSE stream.
* **Impact:** Hooks can only *observe* the stream; they cannot *mutate* it. Torana cannot currently redact sensitive information or strip out injected intents from the stream before it reaches the client.

## 6. Missing Token Usage Tracking
Stream adapters do not extract or emit token usage (Prompt/Completion tokens). This structurally prevents the implementation of billing, rate-limiting, or telemetry plugins in the pipeline.
