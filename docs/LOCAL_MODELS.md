# Local Model Integration

Torana Edge can route to locally hosted models via Ollama, vLLM,
or any OpenAI-compatible local server.

To connect a client that speaks a different API from the local server, configure
a [protocol bridge](PROTOCOL_BRIDGES.md). For example, Anthropic Messages or
OpenAI Responses can target a Chat Completions backend for supported features.

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

For an OpenAI Chat Completions client that appends `/chat/completions`, set
its base URL to `http://localhost:8080/provider/ollama/v1`. A raw request uses
`http://localhost:8080/provider/ollama/v1/chat/completions`. A Responses client
needs an upstream that implements Responses or an explicit protocol bridge.

The fallback declares its own Torana credential: on failover Torana removes the
caller's credential rather than forwarding it to a different vendor. Give every
authenticated fallback its own named credential; use `auth.mode: none` for a
local server that needs no authentication.

To keep routing local, omit the remote fallback shown above. If it is enabled,
eligible failures send the request to DeepSeek using its named credential.
You can also use a local model as a plugin-bound scanner or summarizer.

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

## Use a local model from a plugin

Declaring a provider does not authorize plugin access. Bind a declared model-service
resource and approve its budget separately. The plugin owns the request it sends.
See the [local compactor setup](https://github.com/torana-edge/torana-plugins/blob/main/plugins/compactor/LOCAL_SUMMARIZER.md)
or [PII scanner setup](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md).
