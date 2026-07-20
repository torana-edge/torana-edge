# Torana Edge

Torana Edge is an **extensible, high-performance LLM reverse proxy and routing engine** built specifically for AI coding assistants. It sits transparently between your local agent (e.g., OpenCode, Claude Code, Aider) and cloud LLM providers, acting as a **Smart FinOps Filter**.

All request/response mutations are handled by **WebAssembly (WASM) plugins** running in a sandboxed `wazero` runtime, communicating with the host via **Protobuf** serialization. This architecture enables hot-loadable, language-agnostic plugins with zero-downtime updates.

```
[harness] ←→ [Torana Edge :8080] ←→ [LLM Providers]
             /provider/deepseek/...   → DeepSeek (OpenAI format)
             /provider/anthropic/...  → Anthropic
             /provider/openai/...     → OpenAI
             /provider/gemini/...     → Google Gemini API / Vertex AI
```

For harnesses that can't be pointed at a base URL — notably the **Antigravity
CLI (`agy`)** — Torana also offers an optional TLS-terminating MITM ingress. See
[docs/GEMINI_ANTIGRAVITY.md](docs/GEMINI_ANTIGRAVITY.md).

## Key Features

- **WASM Plugin Ecosystem:** Write plugins in Go (or any WASI-compatible language), compile to `.wasm`, and drop them into the `plugins/` directory. No proxy restarts needed.
- **Model Delegation (The Compactor):** Can summarize selected historical tool outputs via a cheaper model before they hit the expensive upstream, saving tokens and money. Lossy compaction is currently opt-in and should not be used for coding-agent file reads; see [Compaction safety](#compaction-safety).
- **Prompt Cache Optimization:** Deterministic payload normalization ensures upstream provider prompt caches are never busted across turns.
- **Provider Failover:** Automatic retry with fallback providers on 429/5xx errors.
- **Unified IR:** Format adapters translate OpenAI, Anthropic, Bedrock, and Gemini wire formats into a single canonical IR. Plugins work on the IR and never touch raw JSON.
- **MITM ingress (optional):** For harnesses that ignore base-URL overrides (e.g. the Antigravity CLI), an opt-in TLS-terminating proxy routes their traffic through the pipeline. Disabled unless configured.

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
   TORANA_BIND=127.0.0.1 go run ./cmd/torana
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

| Format | Wire API | Streaming |
|---|---|---|
| `openai` | OpenAI Chat Completions + Responses API | SSE |
| `anthropic` | Anthropic Messages API | SSE |
| `bedrock` | AWS Bedrock Converse | JSON lines |
| `gemini` | Google Gemini API / Vertex AI (`generateContent`) | SSE |
| `gemini-codeassist` | Google Code Assist (Antigravity CLI) | SSE |

`gemini` and `gemini-codeassist` share one content model; they differ only in the
request envelope and SSE framing (see [docs/GEMINI_ANTIGRAVITY.md](docs/GEMINI_ANTIGRAVITY.md)).

## Bundled Plugins

| Plugin | Hooks | What it does |
|---|---|---|
| `schema_translator` | `run_before_request`, `run_on_stream_chunk` | Converts open-map tool schemas to strict KV arrays and reverses them on responses |
| `intent` | `run_before_request`, `run_on_stream_chunk` | Captures **why** each tool call is made: injects the required `"i"` field into tool schemas (plus a system-prompt example) and extracts it from the stream into the shared cache |
| `keyword_compactor` | `run_before_request` | Deterministic, local, free tool-result compaction guided by the intent cache |
| `compactor` | `run_before_request` | Cheap-model tool-result compaction guided by the intent cache |
| `pii` | `run_before_request` | Scans tool results (local model + regex) and blocks the request if PII is found |
| `otel` | `run_before_request`, `run_after_response` | Emits request/response OTel metrics |
| `auth` | `run_before_request` | Normalizes caller identity from allowlisted auth headers |

> **Order matters.** Put `intent` before whichever compactor you run — both
> compactors are pure consumers of the intent cache. `keyword_compactor` and
> `compactor` are **alternatives** (deterministic/local vs. cheap-model offload),
> not a pipeline: run **one**, not both, or whichever comes first starves the other.
> Recommended research/log-analysis order:
> `["schema_translator", "intent", "keyword_compactor"]`. For coding agents
> that edit files, use the safe baseline below.

### Compaction safety

Both compactors are lossy. They currently consider every tool result over 2,000
characters, including a fresh result that the model has not seen yet. Summarizing
or extracting lines from `Read`, `View`, or similar source-reading tools can
remove the exact text a coding agent needs for search-and-replace edits.

For coding-agent sessions that may edit files, use the safe baseline
`["schema_translator", "intent"]` until
[#166](https://github.com/torana-edge/torana-edge/issues/166) lands. Enable one
compactor only for workloads where lossy tool-output reduction is acceptable,
such as research or log analysis. The intended fix is tool-aware and
recency-aware: never compact newly returned results, preserve recent tool uses,
and allow exact-output tools to opt out.

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
│   ├── format/                     # Wire format adapters (OpenAI, Anthropic, Bedrock, Gemini)
│   ├── metrics/                    # Request stats tracking
│   ├── mitm/                       # Optional TLS-terminating ingress (Antigravity CLI)
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
│   ├── intent/
│   ├── keyword_compactor/
│   ├── otel/
│   ├── pii/
│   └── schema_translator/
├── config.example.json             # Example configuration
└── go.mod
```

## Endpoints

| Path | Purpose |
|---|---|
| `/provider/<name>/<upstream-path>` | Proxied request to the named provider |
| `/health` | Liveness check — `{"status":"ok"}` |
| `/stats` | Cumulative counters (requests, tokens, compactions, bytes saved) |

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TORANA_CONFIG` | `config.json` | Path to config file |
| `TORANA_PORT` | `8080` | Listen port (overrides config file) |
| `TORANA_DEFAULT_PROVIDER` | (none) | Provider name for non-prefixed paths |
| `TORANA_BIND` | all interfaces | Bind host; use `127.0.0.1` locally because Torana forwards caller credentials |

## Development

Before raising pull requests, developers and AI agents must ensure that all code complies with style guides, compiles, and passes all tests:

```bash
# Run local lint checks (golangci-lint)
make lint

# Run all unit and integration tests
make test
```

## License

Apache 2.0
