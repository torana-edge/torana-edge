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
a local model. The provider URL is the server origin or configured base path;
the `summarizer` model-service approval separately names the root-relative
inference path. For an OpenAI-compatible Ollama endpoint, bind that path to
`/v1/chat/completions`.

The `intent` plugin is optional. When enabled before `compactor`, its cached
signal improves summary guidance; without it, the compactor derives bounded
guidance from the historical request and tool call. This minimal example does
not enable `intent`:

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
    "order": ["compactor"],
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
`ollama`, model `qwen2.5:3b`, and path `/v1/chat/completions`. Bind the associated
`summarizer` pricing resource to explicit zero rates, and bind `target` to the
models that may carry the original request. The plugin receives only the
logical slot names; provider URLs, credentials, models, paths, and budgets
remain operator-owned.

The local summarizer has zero marginal API cost, but the target resource still
needs operator-supplied cache-read/write pricing for the positive-net gate.
See [COMPACTION.md](COMPACTION.md) for the complete configuration.
