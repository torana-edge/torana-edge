# <img src="./assets/logo/torana-color.svg" width="40" align="absmiddle" /> Torana Edge

Torana Edge is a **local-first, programmable reverse proxy for AI coding agents**. It sits between your harness (Claude Code, Codex, OpenCode, Aider) and your provider, and gives you a place to observe, redact, route, veto, or rewrite traffic — without replacing the agent or the model.

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

- **WASM Plugin Ecosystem:** Write plugins with the Go ABI v2 [torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk) and compile to `.wasm`. Hot-loaded, no proxy restart. The repository's older Rust SDK is still ABI v1 and is not compatible with this v2-only host.
- **Operator-approved plugins:** A plugin declares the capabilities it wants; it does not receive them. You inspect and approve each bundle, and the approval is bound to that bundle's SHA-256 digest — rebuild or change permissions and it needs approving again. Sandboxed in `wazero`: a plugin gets the IR and nothing else, no filesystem, no sockets you did not grant.
- **Tool-aware compaction:** Explicit policies keep source and failure evidence exact while allowing recoverable searches/listings to be reduced deterministically—even on first exposure when configured.
- **Economic model delegation:** Historical results can be summarized through a cheaper model only when route-aware cache/offload economics estimate positive net savings.
- **Responses-native compaction:** OpenAI Responses requests can opt into provider-side compaction without Torana storing a second conversation.
- **Provider Failover:** Automatic retry with fallback providers on 429/5xx errors.
- **Unified IR:** Format adapters translate OpenAI, Anthropic, Bedrock, and Gemini wire formats into a single canonical IR. Plugins work on the IR and never touch raw JSON.
- **MITM ingress (optional):** For harnesses that ignore base-URL overrides (e.g. the Antigravity CLI), an opt-in TLS-terminating proxy routes their traffic through the pipeline. Disabled unless configured.

## Quick Start

1. Copy and edit the example config:
   ```bash
   cp config.example.json config.json
   ```
   This seed is read **only on the first start**. Torana then copies it into a
   managed store (`~/.config/torana/config.json`, or
   `$TORANA_DATA_DIR/config.json`) and
   reads that from then on, so later edits to `config.json` have no effect —
   change things in the control plane instead. See
   [Quickstart](docs/QUICKSTART.md#configuration).

2. Install the plugins you want. They live in
   [torana-plugins](https://github.com/torana-edge/torana-plugins), not in this
   repository, and `torana plugin install` fetches the source, builds it
   locally and prints the digest — so nothing runs that you could not have read:
   ```bash
   # The official set:
   go run ./cmd/torana plugin install --official

   # Or one at a time, from any repository:
   go run ./cmd/torana plugin install github.com/torana-edge/torana-plugins/plugins/schema_translator
   ```
   **Plugin binaries are build artifacts and are never committed** (`*.wasm` is
   gitignored), so rebuild after pulling or editing plugin sources.

3. Run the proxy:
   ```bash
   go run ./cmd/torana
   ```

4. Point your AI harness at Torana:
   ```bash
   export OPENAI_BASE_URL=http://localhost:8080/provider/deepseek/v1
   ```

## Documentation

| Guide | What it covers |
| --- | --- |
| [Quickstart](docs/QUICKSTART.md) | Install, add a provider, point a harness at Torana |
| [Running plugins](docs/PLUGINS.md) | Install, inspect, approve, order, and what the sandbox does |
| [Agent control plane](docs/AGENT_CONTROL_PLANE.md) | The versioned JSON API and `agent.json` operation contracts |
| [Local models](docs/LOCAL_MODELS.md) | Point a coding harness at Ollama or vLLM through Torana |
| [Antigravity CLI](docs/GEMINI_ANTIGRAVITY.md) | The optional TLS-terminating MITM ingress |
| [Prompt caching](docs/PROMPT_CACHING.md) | Declaring cache prices and lifetimes, and the arithmetic that bounds cache warming |
| [Context compaction](docs/COMPACTION.md) | Policies, the economic gate, and why it is off by default |
| [Dogfood results](docs/DOGFOOD_166_RESULTS.md) | 75 paired sessions measuring whether compaction actually saves money |

**Writing a plugin?** That lives with the SDK:
[torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk) — the
the ABI v2 Go SDK and authoring guides. The bundled Rust ABI v1 crate is
historical and unsupported by the current host until it is ported or removed.

**Official plugins** live in [torana-plugins](https://github.com/torana-edge/torana-plugins).

## How It Works

1. **Path-based routing** — Requests arrive at `/provider/<name>/<upstream-path>`. Torana strips the provider prefix, looks up the upstream URL and format, and forwards.
2. **Canonical IR** — Format adapters (`internal/format/`) translate each provider's wire format into shared Go types (`ChatRequest`, `Message`, `ToolDef`, `StreamEvent`).
3. **Protobuf Serialization** — The IR is serialized to Protobuf via `internal/engine/pbconv` and handed to the WASM runtime.
4. **WASM Plugin Pipeline** — Loaded plugins execute sequentially (in `config.json` order). Each plugin receives the Protobuf bytes, mutates them via the SDK ([torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk)), and writes back.
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

## Official Plugins

None are bundled. They live in
[torana-plugins](https://github.com/torana-edge/torana-plugins) and are installed
deliberately, one at a time:

```bash
torana plugin install github.com/torana-edge/torana-plugins/plugins/pii
```

`install` fetches the source, builds it locally, and prints the SHA-256 digest of
what it built — so nobody runs a binary they could not have read. Installing
still enables nothing: you approve the digest and its requested capabilities in
the control plane first. See [docs/PLUGINS.md](docs/PLUGINS.md).

| Plugin | Hooks | What it does |
|---|---|---|
| `schema_translator` | `run_before_request`, `run_on_stream_chunk` | Converts open-map tool schemas to strict KV arrays and reverses them on responses |
| `intent` | `run_before_request`, `run_on_stream_chunk` | Captures **why** each tool call is made: injects the required `"i"` field into tool schemas (plus a system-prompt example) and extracts it from the stream into the shared cache |
| `keyword_compactor` | `run_before_request` | Policy-driven source markers, deterministic reductions, and intent-guided extractive compaction |
| `compactor` | `run_before_request` | The same safety policy plus economically gated cheap-model summaries |
| `pii` | `run_before_request` | Scans tool results (local model + regex) and blocks the request if PII is found |
| `otel` | `run_before_request`, `run_after_response` | Emits request/response OTel metrics |
| `cache_tier_selector` | `run_before_request` | Buys the cheapest prompt-cache lifetime for a conversation, and never changes its mind for a given prefix |
| `cache_warmer` | `run_before_request`, `run_on_tick` | Refreshes a chosen conversation's cached prefix before it lapses, bounded by a deadline and a break-even budget |

`auth` is deliberately **not** in that list and is not installed by
`--official`. Its own manifest says it is not published to the public registry:
it demonstrates the identity capability surface and performs no verification, so
installing it by default would put something explicitly not built as an access
control into an access-control position. Its source remains available for study
in [torana-plugins](https://github.com/torana-edge/torana-plugins).

> **Order matters.** Put `intent` before whichever compactor you run — both
> compactors are pure consumers of the intent cache. `keyword_compactor` and
> `compactor` are **alternatives** (deterministic/local vs. cheap-model offload),
> not a pipeline: run **one**, not both, or whichever comes first starves the other.
> Recommended order: `["schema_translator", "intent", "keyword_compactor"]`.
> Unmatched tools remain exact. See [Tool-output and Responses compaction](docs/COMPACTION.md)
> for policy modes, first-pass behavior, pricing, recovery, and OpenAI Responses.

## Project Structure

```
torana-edge/
├── cmd/
│   ├── torana/main.go              # Proxy entry point
│   └── torana-cli/main.go          # thin compatibility wrapper; the real
│                                   # commands are `torana plugin ...`
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
├── config.example.json             # Example configuration
└── go.mod
```

## Endpoints

| Path | Purpose |
|---|---|
| `/provider/<name>/<upstream-path>` | Proxied request to the named provider |
| `/health` | Liveness check — `{"status":"ok"}` |
| `/stats` | Requests/tokens plus separate compaction transformations, applications, cache reuse, and estimated gross/net savings |

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TORANA_CONFIG` | `config.json` | Path to the seed config file |
| `TORANA_DATA_DIR` | `os.UserConfigDir()/torana` | Directory holding the managed store (`$TORANA_DATA_DIR/config.json`) |
| `TORANA_PORT` | `8080` | Listen port (overrides config file) |
| `TORANA_BIND` | `127.0.0.1` | Bind address — see the note below before changing it |
| `TORANA_DEFAULT_PROVIDER` | (none) | Provider name for non-prefixed paths |
| `TORANA_PLUGINS_DIR` | `./plugins` | Plugin directory for the `torana plugin` commands |

`torana help` prints the same table, so it cannot drift out of the binary.

**On `TORANA_BIND`.** Torana binds loopback by default. The control plane
shares this listener, and it can rewrite configuration and approve plugins —
approving a plugin means allowing code to run.

Setting `TORANA_BIND` to a reachable address exposes **the data plane only**.
The control plane has a second, independent check: it requires the request's
*source* address to be loopback, so remote requests to `/_torana/*` are refused
with `403` regardless of what you bind to.

**That check is defeated by putting a reverse proxy in front.** nginx, Caddy or
anything else forwarding to `127.0.0.1` makes every request arrive from a
loopback source, and the control plane will then accept all of them — with no
authentication of any kind, from anyone who can reach your proxy. If you put
Torana behind one, the proxy **must block or authenticate `/_torana/*`
itself**. Torana cannot do it for you.

Until the control plane gets its own listener and real authentication, the safe
configurations are: leave the bind on loopback, or expose only the data plane
and deny `/_torana/` at whatever sits in front.

## Development

Before raising pull requests, developers and AI agents must ensure that all code complies with style guides, compiles, and passes all tests:

```bash
# Run local lint checks (golangci-lint)
make lint

# Run the quick iteration gate — fixtures + full suite (a few minutes)
make test

# Run the slow pre-merge race gate — same suite under -race (~15 minutes)
make test-race
```

## License

Apache 2.0
