# Local Model Integration

Torana Edge can route to locally hosted models via Ollama, vLLM,
or any OpenAI-compatible local server.

## Ollama

```json
{
  "providers": {
    "ollama": {
      "url": "http://localhost:11434/v1",
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
      "url": "http://localhost:8000/v1",
      "format": "openai"
    }
  }
}
```

## Local offload (free compaction)

Use the `compactor` plugin to automatically route heavy summarization tasks to a local model:

```json
{
  "plugins": {
    "dir": "./plugins",
    "order": ["compactor"]
  }
}
```

This gives you token-free compaction — the summarization runs on your
local GPU without any API costs, as the compactor plugin can be configured to offload to Ollama.
