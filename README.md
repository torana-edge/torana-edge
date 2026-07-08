# Torana Edge

Torana Edge is an **extensible, high-performance LLM reverse proxy and routing engine**. It sits transparently between AI coding harnesses (running locally for personal users, or deployed on-premise as an enterprise gateway) and cloud LLM providers. 

By parsing traffic into a unified Internal Representation (IR), Torana allows you to build and attach **plugins** that work universally across OpenAI, Anthropic, Bedrock, and Vertex. The core engine simply routes and runs the middleware pipeline—everything else (FinOps compression, OpenTelemetry, API Key routing, intent caching) is a plugin, allowing you to "build your own Torana."

```
[harness] ←→ [Torana Edge :8080] ←→ [LLM Providers]
             /provider/deepseek/...   → DeepSeek (OpenAI format)
             /provider/anthropic/...  → Anthropic
             /provider/openai/...     → OpenAI
             /provider/vertex/...     → GCP Vertex AI
```

## Quick Start

1. (Optional) Create `config.json` to override defaults:
   ```json
   {
     "port": 8080,
     "providers": {
       "my-provider": {
         "url": "https://api.example.com",
         "format": "openai"
       }
     }
   }
   ```
   Built-in defaults cover deepseek, openai, and anthropic — see `config.example.json`.

2. Run the proxy:
   ```bash
   go run ./cmd/torana
   ```

3. Point your AI harness at Torana:
   ```bash
   export OPENAI_BASE_URL=http://localhost:8080/provider/deepseek/v1
   ```

   For Anthropic-format providers:
   ```bash
   export ANTHROPIC_BASE_URL=http://localhost:8080/provider/anthropic
   ```

## How It Works

1. **Path-based routing** — Requests arrive at `/provider/<name>/<upstream-path>`. Torana strips the provider prefix, looks up the upstream URL and format, and forwards.
2. **Canonical IR** — Format adapters translate each provider's wire format into a shared set of Go types (`ChatRequest`, `Message`, `ToolDef`, `StreamEvent`). Plugins import `internal/engine` and work on IR, never on raw JSON.
3. **Pipeline** — Registered `RequestHook` and `ResponseHook` plugins intercept every request/response. On error, the pipeline continues with the current state — a broken plugin doesn't break the proxy.
4. **Pass-through** — Requests without a `/provider/` prefix (and no `DefaultProvider` configured) return 502. Unknown provider names also return 502. No silent forwarding to the wrong upstream.

## Supported Formats

| Prefix | Format | Streaming |
|---|---|---|
| `/provider/<name>/` (format: `openai`) | OpenAI Chat Completions + Responses API | SSE |
| `/provider/<name>/` (format: `anthropic`) | Anthropic Messages API | SSE |
| `/provider/<name>/` (format: `bedrock`) | AWS Bedrock Converse | JSON lines |
| `/provider/<name>/` (format: `vertex`) | GCP Vertex AI / Gemini | JSON lines |

## Project Structure

```
torana-edge/
├── cmd/torana/main.go             # Entry point, config loading
├── config.json                    # User-facing provider config
├── config.example.json
├── internal/
│   ├── engine/
│   │   ├── types.go               # Canonical IR: ChatRequest, StreamEvent, etc.
│   │   └── pipeline.go            # RequestHook / ResponseHook chain
│   ├── format/
│   │   ├── format.go              # RequestAdapter / StreamAdapter interfaces
│   │   ├── registry.go            # Format registration + Lookup
│   │   ├── openai/                # OpenAI ↔ IR (Chat Completions + Responses)
│   │   ├── anthropic/             # Anthropic Messages ↔ IR
│   │   ├── bedrock/               # Bedrock Converse ↔ IR
│   │   └── vertex/                # Vertex AI / Gemini ↔ IR
│   ├── middleware/
│   │   └── adapter.go             # Request logging hook
│   ├── provider/
│   │   ├── config.go              # Config types, DefaultConfig, Load/merge
│   │   └── resolver.go            # /provider/<name>/ path resolution
│   └── proxy/
│       └── server.go              # Reverse proxy with format dispatch
└── test/
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TORANA_CONFIG` | `config.json` | Path to config file |
| `TORANA_PORT` | `8080` | Listen port (overrides config file) |
| `TORANA_DEFAULT_PROVIDER` | (none) | Provider name for non-prefixed paths |
| `OPENAI_BASE_URL` | — | Set in your harness to `http://localhost:8080/provider/<name>/v1` |
| `ANTHROPIC_BASE_URL` | — | Set in your harness to `http://localhost:8080/provider/<name>` |

## Adding a Provider

Add to `config.json`:

```json
{
  "providers": {
    "my-llm": {
      "url": "https://my-llm.example.com",
      "format": "openai"
    }
  }
}
```

Then use `OPENAI_BASE_URL=http://localhost:8080/provider/my-llm/v1`. No code changes needed.

## License

Apache 2.0
