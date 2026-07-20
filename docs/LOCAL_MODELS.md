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

Use the `compactor` plugin to route explicitly eligible historical results to
a local model. Provider URLs are host roots because Torana appends
`/v1/chat/completions`. The compactor consumes intents captured by `intent`, so
`intent` must run first:

```json
{
  "providers": {
    "ollama": {
      "url": "http://localhost:11434",
      "format": "openai",
      "pricing": {
        "qwen2.5:3b": {
          "input_usd_per_mtok": 0,
          "output_usd_per_mtok": 0,
          "cache_read_usd_per_mtok": 0,
          "cache_write_usd_per_mtok": 0
        }
      }
    }
  },
  "plugins": {
    "dir": "./plugins",
    "order": ["intent", "compactor"],
    "config": {
      "compactor": {
        "expected_applications": 6,
        "tool_policies": [
          {"match": "web_search", "mode": "model"},
          {"match": "read*", "mode": "exact"}
        ]
      }
    }
  },
  "offload": {
    "enabled": true,
    "provider": "ollama",
    "model": "qwen2.5:3b"
  }
}
```

The offload has zero marginal API cost, but the target provider still needs
operator-supplied cache-read/write pricing for the positive-net economic gate.
See [COMPACTION.md](COMPACTION.md) for the complete configuration.
