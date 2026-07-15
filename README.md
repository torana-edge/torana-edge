# Torana Edge

Torana Edge is an **extensible, high-performance LLM reverse proxy and routing engine** built specifically for AI coding assistants. It sits transparently between your local agent (e.g., OpenCode, Claude Code, Aider) and cloud LLM providers, acting as a **Smart FinOps Filter**.

All request/response mutations are handled by **WebAssembly (WASM) plugins** running in a sandboxed `wazero` runtime, communicating with the host via **Protobuf** serialization. This architecture enables hot-loadable, language-agnostic plugins with zero-downtime updates.

```
[harness] ←→ [Torana Edge :8080] ←→ [LLM Providers]
             /provider/deepseek/...   → DeepSeek (OpenAI format)
             /provider/anthropic/...  → Anthropic
             /provider/openai/...     → OpenAI
             /provider/vertex/...     → GCP Vertex AI
```

## Key Features

- **WASM Plugin Ecosystem:** Write plugins in Go (or any WASI-compatible language), compile to `.wasm`, and drop them into the `plugins/` directory. No proxy restarts needed.
- **Model Delegation (The Compactor):** Automatically intercepts heavy tool outputs and summarizes them via a cheaper model before they hit the expensive upstream, saving tokens and money.
- **Prompt Cache Optimization:** Deterministic payload normalization ensures upstream provider prompt caches are never busted across turns.
- **Provider Failover:** Automatic retry with fallback providers on 429/5xx errors.
- **Unified IR:** Format adapters translate OpenAI, Anthropic, Bedrock, and Vertex wire formats into a single canonical IR. Plugins work on the IR and never touch raw JSON.

## Quick Start

1. Copy and edit the example config:
   ```bash
   cp config.example.json config.json
   ```

2. Build the WASM plugins. **Plugin binaries are build artifacts — they are never
   committed to git** (`*.wasm` is gitignored). Always rebuild after pulling or
   editing plugin sources:
   ```bash
   make plugins

   # Or a single plugin via torana-cli:
   go run ./cmd/torana-cli plugin build plugins/schema_translator
   ```

3. Run the proxy:
   ```bash
   go run ./cmd/torana
   ```

4. Point your AI harness at Torana:
   ```bash
   export OPENAI_BASE_URL=http://localhost:8080/provider/deepseek/v1
   ```

## How It Works

1. **Path-based routing** — Requests arrive at `/provider/<name>/<upstream-path>`. Torana strips the provider prefix, looks up the upstream URL and format, and forwards.
2. **Canonical IR** — Format adapters (`internal/format/`) translate each provider's wire format into shared Go types (`ChatRequest`, `Message`, `ToolDef`, `StreamEvent`).
3. **Protobuf Serialization** — The IR is serialized to Protobuf via `internal/engine/pbconv` and handed to the WASM runtime.
4. **WASM Plugin Pipeline** — Loaded plugins execute sequentially (in `config.json` order). Each plugin receives the Protobuf bytes, mutates them via the SDK (`pkg/plugin-sdk`), and writes back.
5. **Pass-through** — Requests without a recognized `/provider/` prefix return 502.

## Supported Formats

| Prefix | Format | Streaming |
|---|---|---|
| `/provider/<name>/` (format: `openai`) | OpenAI Chat Completions + Responses API | SSE |
| `/provider/<name>/` (format: `anthropic`) | Anthropic Messages API | SSE |
| `/provider/<name>/` (format: `bedrock`) | AWS Bedrock Converse | JSON lines |
| `/provider/<name>/` (format: `vertex`) | GCP Vertex AI / Gemini | JSON lines |

## Bundled Plugins

| Plugin | Hooks | What it does |
|---|---|---|
| `schema_translator` | `run_before_request`, `run_on_stream_chunk` | Converts open-map tool schemas to strict KV arrays and reverses them on responses |
| `keyword_compactor` | `run_before_request` | Deterministic keyword-based tool result compaction using the cached intent |
| `compactor` | `run_before_request`, `run_on_stream_chunk` | Injects/extracts the `"i"` intent field; offloads huge tool results to a cheap model |
| `otel` | `run_before_request`, `run_after_response` | Emits request/response OTel metrics |
| `auth` | `run_before_request` | Normalizes caller identity from allowlisted auth headers |

## Project Structure

```
torana-edge/
├── cmd/
│   ├── torana/main.go              # Proxy entry point
│   └── torana-cli/main.go          # CLI for building WASM plugins
├── internal/
│   ├── engine/
│   │   ├── types.go                # Canonical IR: ChatRequest, StreamEvent, etc.
│   │   └── pbconv/                 # IR ↔ Protobuf converters
│   ├── format/                     # Wire format adapters (OpenAI, Anthropic, Bedrock, Vertex)
│   ├── metrics/                    # Request stats tracking
│   ├── plugin/                     # WASM plugin discovery and pipeline orchestration
│   ├── provider/                   # Config parsing, URI resolution
│   ├── proxy/                      # Reverse proxy with format dispatch
│   └── wasm/                       # Wazero runtime integration
├── pkg/
│   ├── pb/                         # Protobuf schemas and generated code
│   └── plugin-sdk/                 # SDK imported by WASM plugins
├── plugins/                        # WASM plugin source code (binaries built via `make plugins`)
│   ├── auth/
│   ├── compactor/
│   ├── keyword_compactor/
│   ├── otel/
│   └── schema_translator/
├── config.example.json             # Example configuration
└── go.mod
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TORANA_CONFIG` | `config.json` | Path to config file |
| `TORANA_PORT` | `8080` | Listen port (overrides config file) |
| `TORANA_DEFAULT_PROVIDER` | (none) | Provider name for non-prefixed paths |

## License

Apache 2.0
