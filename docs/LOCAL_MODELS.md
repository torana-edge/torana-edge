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
      "auth": {"mode": "none"}
    }
  }
}
```

For an OpenAI Chat Completions client that appends `/chat/completions`, set
its base URL to `http://localhost:8080/provider/ollama/v1`. A raw request uses
`http://localhost:8080/provider/ollama/v1/chat/completions`. A Responses client
needs an upstream that implements Responses or an explicit protocol bridge.

This route calls only your local Ollama server; no hosted-provider account is
needed. Add it through Torana Settings or the [CLI configuration workflow](CLI.md#settings-read-edit-apply).
The snippet shows the provider entry, not a replacement for your other settings.

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

Declaring a provider does not authorize plugin access. Bind a declared
model-service resource and approve its budget separately. The provider URL
selects the origin and optional base path. Torana derives the standard
inference path from the configured API format and avoids repeating a base such
as `/v1`. Set a provider default model when the API needs one; single-model
local endpoints may leave it blank. Custom servers can set an inference-path
override under the provider's advanced settings.
See the [local compactor setup](https://github.com/torana-edge/torana-plugins/blob/main/plugins/compactor/LOCAL_SUMMARIZER.md)
or [PII scanner setup](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md).

## Optional: add a fallback

If you want a second endpoint for eligible failures, configure it explicitly
using the [fallback guide](QUICKSTART.md#provider-authentication-and-fallbacks).
Give an authenticated fallback its own named credential; use `auth.mode: none`
for a local server that needs no authentication. A remote fallback sends the
request to that provider and uses its billing, so add one only when you want
that behavior.
