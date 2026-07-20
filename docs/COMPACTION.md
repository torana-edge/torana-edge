# Tool-output and Responses compaction

Torana has two different compaction paths because the proxy does not always
own the same history:

- When a harness resends visible chat history, a WASM compactor can replace
  selected historical tool results.
- When OpenAI owns history behind `previous_response_id`, Torana cannot rewrite
  that hidden history. It can instead opt the request into OpenAI Responses
  server-side compaction.

Neither path is enabled implicitly. Tool results that do not match an explicit
policy remain exact.

## Tool-result policies

Policies are evaluated in order using case-insensitive shell-style matches; the
first match wins. Torana recovers a result's tool name from the preceding tool
call when a provider only supplies a call ID.

The supported modes are:

| Mode | Behavior |
| --- | --- |
| `exact` | Never alter the output. This is also the default for unknown tools. |
| `source` | Reserved, but currently fails closed to `exact`. Live OMP dogfood showed that reread markers can make an agent repeatedly fetch different ranges of the same file. |
| `deterministic` | Retain bounded head/tail evidence plus size, SHA-256, omitted-byte count, and a rerun instruction. Set `first_pass` to compact the first model exposure. |
| `keyword` | `keyword_compactor` only: retain intent-matching lines after at least one exact exposure. |
| `model` | `compactor` only: create an intent-guided offload summary after at least one exact exposure, then apply it only when the economic gate passes. |

Mutation tools, diffs, failed commands, errors, stack traces, and similar
safety-sensitive outputs remain exact even if a broad rule matches them.
Model-generated compaction can never run on a fresh output.

Example deterministic coding-agent policy:

```json
{
  "plugins": {
    "order": ["schema_translator", "intent", "keyword_compactor"],
    "config": {
      "keyword_compactor": {
        "tool_policies": [
          {
            "match": "read*",
            "mode": "exact"
          },
          {
            "match": "web_search",
            "mode": "deterministic",
            "first_pass": true,
            "rerun": "Repeat the search to recover every result."
          },
          {"match": "grep*", "mode": "keyword"}
        ]
      }
    }
  }
}
```

`first_pass` is intentionally honored only by `deterministic`. This is useful
for large, reproducible listings, searches, and repetitive successful logs: the
model sees the same compact representation on every turn, so no later prompt
prefix rewrite is required. Do not enable it for source reads or exact records.
Keep source-reading tools `exact`: merely making their output recoverable does
not bound the number or cost of recovery calls an agent may make. The `source`
spelling is accepted for configuration compatibility, but currently behaves as
`exact` while recovery economics and loop detection are investigated in issue
#178.

## Model compaction economics

Model compaction is fail-closed. Configure both an expected number of
applications and explicit prices for the target and offload models; Torana
ships no price table because rates and cache semantics change.

```json
{
  "providers": {
    "primary": {
      "url": "https://api.example.com",
      "format": "openai",
      "pricing": {
        "my-model": {
          "input_usd_per_mtok": 1.0,
          "output_usd_per_mtok": 4.0,
          "cache_read_usd_per_mtok": 0.1,
          "cache_write_usd_per_mtok": 1.25
        }
      }
    },
    "local-offload": {
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
    "provider": "local-offload",
    "model": "qwen2.5:3b"
  }
}
```

The primary rates above are illustrative, not built-in defaults; replace them
with the rates for the configured provider and model. An explicit zero is valid
for a local model; an omitted rate is unknown.

The compactor first tests whether even a best-case reduction could repay the
cache rewrite. Only then does it call the offload model. Generated candidates
are evaluated as one batch using:

- estimated tokens removed;
- the prompt span rewritten from the earliest changed result;
- expected future applications;
- target cache-read and cache-write rates; and
- reported offload input, output, and cache usage.

The batch is applied only when estimated net savings are positive. Token counts
derived from bytes are labeled as estimates. Routing plugins must run before an
economically gated compactor so the decision uses the final provider and model.

`/stats` separates transformations, cache reuse, and applications, and exposes
estimated removed tokens, cache rewrite tokens, gross savings, net savings, and
reasons a dollar estimate was unavailable. These numbers support workload-
specific A/B comparisons; they are not a universal percentage of the total API
bill. It also exposes successful offload input, output, cache-read, and
cache-write tokens so an A/B test can include the summarizer's actual cost.
OpenAI-compatible DeepSeek responses are accounted using DeepSeek's
`prompt_cache_hit_tokens` fields when standard OpenAI cache details are absent.

## OpenAI Responses compaction

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
