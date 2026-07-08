# Torana Edge

Torana Edge is an **extensible, high-performance LLM reverse proxy and routing engine** built specifically for AI coding assistants. It sits transparently between your local agent (e.g., OpenCode, Claude Code) and cloud LLM providers, acting as a **Smart FinOps Filter**.

By parsing traffic into a unified Internal Representation (IR), Torana intercepts massive tool outputs (like 50,000-line log files or minified JS dumps), safely summarizes them using a dirt-cheap model (e.g., DeepSeek-Flash), and forwards only the relevant chunks to the expensive upstream model (e.g., GPT-4o, Claude 3.5 Sonnet).

```
[harness] ←→ [Torana Edge :8080] ←→ [LLM Providers]
             /provider/deepseek/...   → DeepSeek (OpenAI format)
             /provider/anthropic/...  → Anthropic
             /provider/openai/...     → OpenAI
             /provider/vertex/...     → GCP Vertex AI
```

## Key Features (v0.1.0)

- **Model Delegation (The Compactor):** Stop paying premium GPT-4/Claude prices to read massive walls of log text. Torana automatically intercepts heavy tool outputs and summarizes them via a cheaper model before they leave your network.
- **Contextual Intent Inference:** The proxy natively traces the user's conversational history to infer *exactly* what the agent is looking for, ensuring the cheap model summarizes precisely what is needed.
- **Zero-Cost Compaction Caching:** Because LLM APIs are stateless, local agents often re-send the exact same massive tool output on every subsequent turn. Torana caches its summaries by `ToolCallID`, turning these expensive repeated payloads into $0, 0-millisecond cache hits.
- **Unified IR Plugin Ecosystem:** Write plugins that work universally across OpenAI, Anthropic, Bedrock, and Vertex without dealing with provider-specific wire formats.

## Quick Start

1. (Optional) Create `config.json` to override defaults (and configure the offload model):
   ```json
   {
     "port": 8080,
     "providers": {
       "my-provider": {
         "url": "https://api.example.com",
         "format": "openai"
       }
     },
     "offload": {
       "enabled": true,
       "provider": "deepseek",
       "model": "deepseek-v4-flash"
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
3. **Pipeline Plugins** — Registered `RequestHook` and `ResponseHook` plugins intercept every request/response. The core `OffloadHook` and `SchemaTranslator` operate here to perform intent extraction, few-shot injection, and cost-saving compaction.
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
├── internal/
│   ├── engine/
│   │   ├── types.go               # Canonical IR: ChatRequest, StreamEvent, etc.
│   │   └── pipeline.go            # RequestHook / ResponseHook chain
│   ├── format/                    # Format translation adapters (OpenAI, Anthropic, etc.)
│   ├── middleware/
│   │   ├── offload.go             # Compactor, Intent Cache, Model Delegation
│   │   └── schema_translator.go   # Intent Extraction, Few-Shot Injection
│   ├── provider/                  # Config parsing, URI resolution
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

## License

Apache 2.0
