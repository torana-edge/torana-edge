# OpenAI Responses: provider-managed compaction

This is an optional proxy setting for the native OpenAI Responses route.
OpenAI compacts the conversation it owns; Torana does not run a summarizer
plugin or maintain another transcript.

For tool results visible in a harness's replayed history, use the separate
[compactor plugin guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/compactor/COMPACTION.md).

Configure native compaction on an OpenAI-format provider with an explicit
threshold:

```json
{
  "providers": {
    "openai": {
      "url": "https://api.openai.com",
      "format": "openai",
      "responses_compaction": {
        "compact_threshold": 100000
      }
    }
  }
}
```

For Responses requests, Torana adds:

```json
{
  "context_management": [
    {"type": "compaction", "compact_threshold": 100000}
  ]
}
```

Caller-supplied `context_management` always wins. Chat Completions requests are
unchanged.

Torana does not store a second transcript or start a new conversation. With
`previous_response_id`, OpenAI owns and compacts the hidden conversation. With
stateless input-array chaining, Torana preserves opaque reasoning and
compaction items in their original order and forwards them with the next turn.

See the [OpenAI compaction guide](https://developers.openai.com/api/docs/guides/compaction)
for the provider-side lifecycle.
