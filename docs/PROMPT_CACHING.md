# Provider prompt caching

Torana can see conversations, preserve provider cache fields, and use your
configured prices. It does not infer the economic contract behind those fields:
how long a provider keeps a prefix, whether a read restarts that lifetime, and
what the read or write costs. Declare those facts explicitly. Cache plugins
use their own operator-approved policy resources; provider configuration alone
does not bind or authorize those resources.

This is configuration rather than a built-in table on purpose. A provider that
speaks the OpenAI wire format while pricing and caching like Anthropic is handled
by the same code path as Anthropic — nothing branches on a provider name.

## Configuration

There are three layers: provider pricing/cache settings, plugin settings, and
digest-bound plugin resource approvals. Setting the first two does not grant
the third. Numbers below illustrate a cache contract, not current price advice;
verify your actual provider/model before using them.

```json
{
  "providers": {
    "anthropic": {
      "url": "https://api.anthropic.com",
      "format": "anthropic",
      "pricing": {
        "claude-sonnet-4-5": {
          "input_usd_per_mtok": 3.0,
          "output_usd_per_mtok": 15.0,
          "cache_read_usd_per_mtok": 0.3,
          "cache_write_usd_per_mtok": 3.75
        }
      },
      "cache": {
        "refresh_on_read": true,
        "tiers": [
          { "ttl_seconds": 300,  "write_multiplier": 1.25, "marker": { "type": "ephemeral" } },
          { "ttl_seconds": 3600, "write_multiplier": 2.0,  "marker": { "type": "ephemeral", "ttl": "1h" } }
        ]
      }
    }
  }
}
```

| Field | Meaning |
|---|---|
| `refresh_on_read` | Whether reading an entry restarts its clock. **`false` means no periodic request can keep an entry alive.** |
| `tiers` | The cache lifetimes this provider sells. Most offer one or none. |
| `ttl_seconds` | How long an entry survives without being read. |
| `write_multiplier` | Cost of writing this tier relative to the model's base input rate. A multiplier because it holds across models while the base rate does not. |
| `marker` | The provider-specific JSON breakpoint value that selects this tier. Torana preserves its meaning and never invents provider fields; adapters may deterministically canonicalize object-member order at the input boundary. |
| `warm_interval_seconds` | Optional. How often to send a refresh. Defaults to 80% of the shortest tier's TTL. |

Omitting `cache` entirely is valid and means "unknown". Under unknown semantics,
do not authorize plugin spending until the required policy resources have
explicit rates and semantics.

**Verify these numbers against your provider's current pricing page.** Torana
ships no built-in rates, and a stale `write_multiplier` will produce confidently
wrong arithmetic.

## Provider support is not one boolean

Torana distinguishes three separate capabilities: preserving a provider cache
field, observing cache usage, and actively changing or refreshing a breakpoint.
Support for either of the first two does not imply the third.

| Provider format | Native behavior | What Torana does | Tier selector / warmer |
|---|---|---|---|
| **Anthropic Messages** | Explicit `cache_control` breakpoints; a 5-minute default and a 1-hour tier | Preserves ordered markers and observes cache creation/read tokens | Supported when configured. Reads refresh the default lifetime, so `refresh_on_read: true` is appropriate. |
| **OpenAI Chat/Responses** | Automatic prefix caching, with optional `prompt_cache_key` and, on eligible models, `prompt_cache_retention` such as `24h` | Preserves these provider fields and observes cached tokens; never chooses retention for you | Not supported. A periodic inference request is not Torana-owned TTL management. |
| **DeepSeek (OpenAI-compatible)** | Automatic disk prefix caching with hit/miss usage | Preserves compatible provider fields and observes hit tokens | Not supported. There is no Torana-managed breakpoint to select or refresh. |
| **Gemini / Code Assist** | Implicit caching is automatic. Explicit caching creates a separate `cachedContents` resource with its own TTL, then generation requests reference it with `cachedContent`. | Preserves the reference and observes cached-content tokens; does not create or PATCH the resource | Not supported. Sending `generateContent` does not perform the cache resource's TTL update operation. |

This matrix is about wire semantics, not provider branding. A compatible gateway
may implement different economics. Configure it only after verifying that its
inference-request marker really is refreshable by a read.

Current provider references:

- [Anthropic prompt caching](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching)
- [OpenAI prompt caching](https://platform.openai.com/docs/guides/prompt-caching)
- [DeepSeek context caching](https://api-docs.deepseek.com/guides/kv_cache)
- [Gemini context caching](https://ai.google.dev/gemini-api/docs/caching)

OpenAI extended retention can have different data-retention characteristics
from in-memory caching. Treat `prompt_cache_retention` as an explicit provider
choice and review the provider's current data controls before enabling it.

## Validation

`warm_interval_seconds` must be **less than** the shortest tier's `ttl_seconds`.
An interval at or beyond the TTL never refreshes anything while still paying for
every request it sends, so it is rejected at config load rather than discovered
on a bill.

Duplicate TTLs, non-positive TTLs, and negative multipliers are also rejected.

## Optional plugins

The proxy preserves supported cache fields and reports usage without cache
plugins. To change or refresh an explicit marker, configure and approve the
separate [tier selector](https://github.com/torana-edge/torana-plugins/blob/main/plugins/cache_tier_selector/README.md)
or [cache warmer](https://github.com/torana-edge/torana-plugins/blob/main/plugins/cache_warmer/README.md).
Their guides own resource bindings, lifecycle, failure behavior and
[warming economics](https://github.com/torana-edge/torana-plugins/blob/main/plugins/cache_warmer/ECONOMICS.md).

## Inspect cache usage

Run `torana conversations` and inspect the request feed or provider usage
records. Read tokens show reuse; write tokens show creation of a cache entry.
Those counters alone do not prove that Torana warmed an entry or explain why
it was created. Keep plugin-egress costs separate from the original request.
