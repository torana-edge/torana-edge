# Torana Edge

A state-aware AI FinOps reverse proxy that optimizes context windows between developer agent harnesses and cloud LLM providers.

## How It Works

Torana Edge sits between your AI coding tools (Claude Code, Aider) and cloud LLM providers (Anthropic, OpenAI). It intercepts tool calls, extracts the model's intent, and compacts large tool results before they consume your context window.

```
[Claude Code] ←→ [Torana Edge :8080] ←→ [Cloud LLM Provider]
```

## Quick Start

1. Copy `.env.example` to `.env` and configure your provider:
   ```
   UPSTREAM_PROVIDER=anthropic
   UPSTREAM_URL=https://api.anthropic.com
   ```

2. Run the proxy:
   ```bash
   go run ./cmd/torana
   ```

3. Point your AI tool at the proxy:
   ```bash
   export ANTHROPIC_BASE_URL=http://localhost:8080
   ```

## Project Structure

```
torana-edge/
├── cmd/torana/main.go        # Entry point
├── internal/
│   ├── proxy/server.go       # Reverse proxy engine
│   ├── middleware/            # Request/response mutators
│   └── cache/                 # Intent store
└── test/                     # Integration tests
```

## License

Apache 2.0
