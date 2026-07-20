# Local Model Integration

Torana Edge can route to locally hosted models via Ollama, vLLM,
or any OpenAI-compatible local server.

## Ollama

```json
{
  "providers": {
    "ollama": {
      "url": "http://localhost:11434",
      "format": "openai",
      "fallback": ["deepseek"]
    }
  }
}
```

Route your harness to `http://localhost:8080/provider/ollama`.

Typical use: use Ollama for offload compaction (mirrors the cheap model pattern)
while keeping your primary provider (DeepSeek/OpenAI) for reasoning.

## vLLM

```json
{
  "providers": {
    "vllm": {
      "url": "http://localhost:8000",
      "format": "openai"
    }
  }
}
```

## Local offload (free compaction)

Use the `compactor` plugin to route eligible summarization tasks to a local
model. Provider URLs must be host roots because the offload client appends
`/v1/chat/completions`. The compactor consumes intents captured by the `intent`
plugin, so `intent` must run before it:

```json
{
  "plugins": {
    "dir": "./plugins",
    "order": ["intent", "compactor"]
  },
  "offload": {
    "enabled": true,
    "provider": "ollama",
    "model": "qwen2.5:3b"
  }
}
```

This gives you token-free compaction — the summarization runs on your
local GPU without API costs.

The current compactor is lossy and considers fresh tool results before the
model has seen them. Do not enable it for coding-agent sessions that depend on
exact file contents until
[#166](https://github.com/torana-edge/torana-edge/issues/166) is resolved. It is
appropriate today for research, logs, and other outputs where summarization is
acceptable.
