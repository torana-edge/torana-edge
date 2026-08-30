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

- **WASM Plugin Ecosystem:** Write ABI-v1 plugins in Go or Rust with the [torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk) and compile them to `.wasm`. Hot-loaded, no proxy restart. Both SDKs run through the host's executable conformance harness in CI.
- **Operator-approved plugins:** A plugin declares the capabilities it wants; it does not receive them. You inspect and approve each bundle, and the approval is bound to that bundle's SHA-256 digest — rebuild or change permissions and it needs approving again. Sandboxed in `wazero`: a plugin gets the IR and nothing else, no filesystem, no sockets you did not grant.
- **Tool-aware compaction:** Explicit policies keep source and failure evidence exact while allowing recoverable searches/listings to be reduced deterministically—even on first exposure when configured.
- **Economic model delegation:** Historical results can be summarized through a cheaper model only when route-aware cache/offload economics estimate positive net savings.
- **Responses-native compaction:** OpenAI Responses requests can opt into provider-side compaction without Torana storing a second conversation.
- **Provider Failover:** Automatic retry with fallback providers on 429/5xx errors.
- **Unified IR:** Format adapters translate OpenAI, Anthropic, Bedrock, and Gemini wire formats into a single canonical IR. Plugins work on the IR and never touch raw JSON.
- **MITM ingress (optional):** For harnesses that ignore base-URL overrides (e.g. the Antigravity CLI), an opt-in TLS-terminating proxy routes their traffic through the pipeline. Disabled unless configured.

## Quick Start

`config.example.json` is a seed read on the first start. Torana then owns the
managed store at `$TORANA_DATA_DIR/config.json`; make later changes through the
local control plane or CLI rather than editing the seed.

The two limits in the example are per identity: `concurrency` bounds active
upstream requests and `rpm` bounds starts per minute. A value of `0` disables
that limit. JSON comments are intentionally unsupported so configuration has
one strict, portable grammar; the Control Plane and
[Quickstart](docs/QUICKSTART.md#configure) explain every first-run field.

Torana is currently pre-release, so this walkthrough builds the reviewed
`main` branch rather than pretending a stable release exists. You need Git, Go
1.26.6 or newer, and a DeepSeek API key.

1. Clone, build, and create a minimal configuration:

   ```bash
   git clone https://github.com/torana-edge/torana-edge.git
   cd torana-edge
   go build -o ./torana ./cmd/torana
   cp config.example.json config.json
   export DEEPSEEK_API_KEY='replace-with-your-deepseek-key'
   ```

2. Start with **no plugins** so the first check proves the proxy itself works.
   For a disposable evaluation, keep managed state inside the checkout:

   ```bash
   export TORANA_DATA_DIR="$PWD/.torana-data"
   ./torana --debug
   ```

   Keep that terminal open and use another terminal for the remaining commands.
   `--debug` logs one safe received/completed line per inference request (route,
   provider, status, latency, plugins, and verdict; never bodies or credentials),
   so you can tell immediately that your harness is reaching Torana. Torana
   imports `config.json` only on this first
   start; `$TORANA_DATA_DIR/config.json` is authoritative afterward. To repeat a
   genuinely clean first run, use a new empty data directory. Do not edit the
   seed and expect a running installation to change.

3. In another terminal, verify health and send one real request:

   ```bash
   curl --fail-with-body http://127.0.0.1:8080/health

   curl --fail-with-body http://127.0.0.1:8080/provider/deepseek/v1/chat/completions \
     -H "Authorization: Bearer ${DEEPSEEK_API_KEY}" \
     -H 'Content-Type: application/json' \
     -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Reply with exactly: Torana works"}]}'
   ```

   The first command returns `{"status":"ok"}`. The second returns a normal
   provider response through Torana. The example uses `auth.mode: caller`, so
   Torana forwards the credential on this request to this provider only. You do
   not have to store an API key in Torana for ordinary harness passthrough.

4. Point your harness at the same provider route:

   ```bash
   export OPENAI_BASE_URL=http://127.0.0.1:8080/provider/deepseek/v1
   ```

   Harness-specific examples, including Claude Code and OpenCode, are in the
   [Quickstart](docs/QUICKSTART.md#route-your-harness).

5. Only after the proxy works, install one plugin from readable source:

   ```bash
   ./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
   ./torana plugin list
   ```

   Installation does **not** run the plugin. Open
   [http://127.0.0.1:8080/_torana/](http://127.0.0.1:8080/_torana/), inspect its
   requested capability and private-file budget, approve it, enable it, and
   place it in the pipeline. Send a few requests, then inspect the tangible
   result without opening the plugin sandbox:

   ```bash
   ./torana plugin file tail usage_logger usage.jsonl
   ```

   Each line contains provider, model, latency, status, and input/output/cache
   token counts—never prompts, responses, or headers. A changed bundle must be
   approved again.

6. When you are comfortable with that lifecycle, install the maintained set:

   ```bash
   ./torana plugin install --official
   ```

   These plugins still remain disabled until you approve and order them. Build
   artifacts are local and never committed (`*.wasm` is gitignored).

Only recognized inference calls enter the IR and plugin pipeline. To exercise
that boundary, request an auxiliary endpoint and confirm it does not appear in
the live inference feed:

```bash
curl -i http://127.0.0.1:8080/provider/deepseek/models
```

The provider may return success or its own error for that path; Torana forwards
it as ordinary HTTP rather than attempting to decode it as an inference call.

## Documentation

| Guide | What it covers |
| --- | --- |
| [Quickstart](docs/QUICKSTART.md) | Install, add a provider, point a harness at Torana |
| [Harness compatibility](docs/HARNESS_COMPATIBILITY.md) | Which inference endpoints enter plugins and which client traffic passes through |
| [Structured audit log](docs/AUDIT_LOG.md) | Default-off operator JSONL records, sensitive fields, rotation, and failure monitoring |
| [Production benchmark](docs/BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md) | Direct-vs-Torana latency, throughput, CPU, memory, raw data, and limits |
| [Running plugins](docs/PLUGINS.md) | Install, inspect, approve, order, and what the sandbox does |
| [Agent control plane](docs/AGENT_CONTROL_PLANE.md) | The versioned JSON API and `agent.json` operation contracts |
| [Local models](docs/LOCAL_MODELS.md) | Point a coding harness at Ollama or vLLM through Torana |
| [Antigravity CLI](docs/GEMINI_ANTIGRAVITY.md) | The optional TLS-terminating MITM ingress |
| [Prompt caching](docs/PROMPT_CACHING.md) | Declaring cache prices and lifetimes, and the arithmetic that bounds cache warming |
| [Context compaction](docs/COMPACTION.md) | Policies, the economic gate, and why it is off by default |
| [Dogfood results](docs/DOGFOOD_166_RESULTS.md) | 75 paired sessions measuring whether compaction actually saves money |

**Writing a plugin?** That lives with the SDK:
[torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk) — the
ABI-v1 Go and Rust SDKs, examples, conformance guests, and authoring guides.

**Official plugins** live in [torana-plugins](https://github.com/torana-edge/torana-plugins).

## How It Works

1. **Path-based routing** — Requests arrive at `/provider/<name>/<upstream-path>`. Torana strips the provider prefix, looks up the upstream URL and format, and forwards.
2. **Selective interception** — Only recognized inference endpoints for that format enter Torana's IR. Model-list, account, status, telemetry, update, and unknown auxiliary endpoints are forwarded as ordinary HTTP without running inference plugins.
3. **Canonical IR** — Format adapters (`internal/format/`) translate inference requests into shared Go types (`ChatRequest`, `Message`, `ToolDef`, `StreamEvent`).
4. **Protobuf Serialization** — The IR is serialized to Protobuf via `internal/engine/pbconv` and handed to the WASM runtime.
5. **WASM Plugin Pipeline** — Loaded plugins execute sequentially (in `config.json` order). Each plugin receives the Protobuf bytes, mutates them via the SDK ([torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk)), and writes back.
6. **No route guessing** — Requests without a matching provider (and no configured default provider) return 502.

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

The inference boundary is explicit and method-sensitive:

| Format | POST path suffixes that enter IR/plugins |
|---|---|
| `openai` | `/chat/completions`, `/responses` |
| `anthropic` | `/messages` |
| `bedrock` | `/converse`, `/converse-stream` |
| `gemini`, `gemini-codeassist` | `:generateContent`, `:streamGenerateContent` |

Version, deployment, and model path prefixes may precede those suffixes. Other
methods and paths are forwarded unchanged to the selected provider; a new vendor
status or account endpoint cannot accidentally be decoded as a chat request.

## Official Plugins

None are bundled. They live in
[torana-plugins](https://github.com/torana-edge/torana-plugins) and are installed
deliberately, either individually or as the maintained set:

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
```

`install` fetches the source, builds it locally, and prints the SHA-256 digest of
what it built — so nobody runs a binary they could not have read. Installing
still enables nothing: you approve the digest and its requested capabilities in
the control plane first. See [docs/PLUGINS.md](docs/PLUGINS.md).

| Plugin | Hooks | What it does |
|---|---|---|
| `usage_logger` | `run_after_response` | Writes content-free provider/model/status/latency/token records to a private rotating JSONL file |
| `schema_translator` | `run_before_request`, `run_on_stream_chunk` | Converts open-map tool schemas to strict KV arrays and reverses them on responses |
| `intent` | `run_before_request`, `run_on_stream_chunk` | Captures **why** each tool call is made: injects the required `"i"` field into tool schemas (plus a system-prompt example) and extracts it from the stream into the shared cache |
| `keyword_compactor` | `run_before_request` | Policy-driven source markers, deterministic reductions, and extractive compaction with cached or locally derived guidance |
| `compactor` | `run_before_request` | The same safety policy plus economically gated cheap-model summaries with cached or locally derived guidance |
| `pii` | `run_before_request` | Scans tool results (local model + regex) and blocks the request if PII is found |
| `otel` | `run_before_request`, `run_after_response` | Emits request/response OTel metrics |
| `cache_tier_selector` | `run_before_request` | Buys the cheapest prompt-cache lifetime for a conversation, and never changes its mind for a given prefix |
| `cache_warmer` | `run_before_request`, `run_on_tick` | Refreshes a chosen conversation's cached prefix before it lapses, bounded by a deadline and a break-even budget |
| `tool_governor` | `run_before_request` | Restricts or replaces the tool definitions advertised to the model; it is model-input policy, not an execution sandbox |

`auth` is deliberately **not** in that list and is not installed by
`--official`. Its own manifest says it is not published to the public registry:
it demonstrates the identity capability surface and performs no verification, so
installing it by default would put something explicitly not built as an access
control into an access-control position. Its source remains available for study
in [torana-plugins](https://github.com/torana-edge/torana-plugins).

> **Order matters.** If enabled, put `intent` before whichever compactor you
> run: its cached signal improves relevance, but is no longer an availability
> dependency. Both compactors derive bounded local guidance when it is absent.
> `keyword_compactor` and
> `compactor` are **alternatives** (deterministic/local vs. cheap-model offload),
> not a pipeline: run **one**, not both, or whichever comes first starves the other.
> Recommended order: `["schema_translator", "intent", "keyword_compactor"]`.
> Unmatched tools remain exact. See [Tool-output and Responses compaction](docs/COMPACTION.md)
> for policy modes, first-pass behavior, pricing, recovery, and OpenAI Responses.

## Project Structure

```
torana-edge/
├── cmd/
│   └── torana/main.go              # Proxy and CLI entry point
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

Torana's initial operating model is **one developer on one workstation**. The
conversation registry, plugin cache, limits, and control-plane permissions are
not tenant-partitioned. Exposing the data plane can be useful for that
developer's own tools, but it does not turn Torana into a multi-user or team
gateway. Put a separately authenticated, tenant-aware service in front before
using it that way.

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
