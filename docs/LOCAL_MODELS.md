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
      "auth": {"mode": "none"},
      "fallback": ["deepseek"]
    },
    "deepseek": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "credential", "credential": "deepseek-api-key"}
    }
  }
}
```

Route your harness to `http://localhost:8080/provider/ollama`.

The fallback declares its own Torana credential: on failover Torana removes the
caller's credential rather than forwarding it to a different vendor. Give every
authenticated fallback its own named credential; use `auth.mode: none` for a
local server that needs no authentication.

Typical use: bind Ollama as a compactor model service
while keeping your primary provider (DeepSeek/OpenAI) for reasoning.

## vLLM

```json
{
  "providers": {
    "vllm": {
      "url": "http://localhost:8000",
      "format": "openai",
      "auth": {"mode": "none"}
    }
  }
}
```

## Local summarization (free compaction)

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
      "auth": {"mode": "none"}
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
  }
}
```

When approving `compactor`, bind its `summarizer` model-service slot to provider
`ollama` and model `qwen2.5:3b`. Bind the associated `summarizer` pricing
resource to explicit zero rates, and bind `target` to the models that may carry
the original request. The plugin receives only the logical slot names; provider
URLs, credentials, models, and budgets remain operator-owned.

The local summarizer has zero marginal API cost, but the target resource still
needs operator-supplied cache-read/write pricing for the positive-net gate.
See [COMPACTION.md](COMPACTION.md) for the complete configuration.
