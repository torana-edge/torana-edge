# Prompt caching

Torana can see conversations, preserve provider cache fields, and use your
configured prices. It does not infer the economic contract behind those fields:
how long a provider keeps a prefix, whether a read restarts that lifetime, and
what the read or write costs. You declare those facts once per provider, and
anything that would spend money on the cache reads them.

This is configuration rather than a built-in table on purpose. A provider that
speaks the OpenAI wire format while pricing and caching like Anthropic is handled
by the same code path as Anthropic — nothing branches on a provider name.

## Configuration

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
anything that would spend money must decline to act rather than guess — the same
rule the compactor follows when pricing is missing.

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
| **Amazon Bedrock Converse** | Explicit `cachePoint` elements on supported models and request arms | Preserves ordered markers and observes cache read/write tokens | Supported only when the configured model exposes refreshable inference-request breakpoints. |
| **OpenAI Chat/Responses** | Automatic prefix caching, with optional `prompt_cache_key` and, on eligible models, `prompt_cache_retention` such as `24h` | Preserves these provider fields and observes cached tokens; never chooses retention for you | Not supported. A periodic inference request is not Torana-owned TTL management. |
| **DeepSeek (OpenAI-compatible)** | Automatic disk prefix caching with hit/miss usage | Preserves compatible provider fields and observes hit tokens | Not supported. There is no Torana-managed breakpoint to select or refresh. |
| **Gemini / Code Assist** | Implicit caching is automatic. Explicit caching creates a separate `cachedContents` resource with its own TTL, then generation requests reference it with `cachedContent`. | Preserves the reference and observes cached-content tokens; does not create or PATCH the resource | Not supported. Sending `generateContent` does not perform the cache resource's TTL update operation. |

This matrix is about wire semantics, not provider branding. A compatible gateway
may implement different economics. Configure it only after verifying that its
inference-request marker really is refreshable by a read.

Current provider references:

- [Anthropic prompt caching](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching)
- [Amazon Bedrock prompt caching](https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html)
- [OpenAI prompt caching](https://platform.openai.com/docs/guides/prompt-caching)
- [DeepSeek context caching](https://api-docs.deepseek.com/guides/kv_cache)
- [Gemini context caching](https://ai.google.dev/gemini-api/docs/caching)

OpenAI extended retention can have different data-retention characteristics
from in-memory caching. Treat `prompt_cache_retention` as an explicit provider
choice and review the provider's current data controls before enabling it.

## The arithmetic that limits warming

A refresh costs one cache read over the prefix. Letting the entry lapse costs the
difference between a write and a read on the next turn. So refreshing is cheaper
than lapsing only while:

```
refreshes_spent  <  (write_rate / read_rate) - 1
```

**The prefix size cancels out.** This is a pure price ratio, independent of how
large the conversation is. With Anthropic's 5-minute tier at a 12.5x
write-to-read ratio, that is about **11 refreshes — roughly 45 minutes** at the
default interval. Past that, warming has cost more than the cache miss it was
avoiding.

The consequence is worth stating plainly: **keeping a conversation warm
indefinitely does not converge on break-even, it diverges.** Warming must be
opt-in per conversation and bounded by a deadline or a refresh budget, never left
on as a global default.

For gaps longer than roughly half an hour, buying a longer tier once is cheaper
than holding a short one open. With Anthropic's numbers, the 1-hour tier costs
`(2.0 - 1.25) = 0.75x` base extra to write, while refreshing the 5-minute tier
for that same hour costs about `1.5x` base — around twice as much.

| Idle gap | Cheapest move |
|---|---|
| under 5 min | nothing — the native TTL covers it |
| 5–30 min | refresh the short tier |
| 30–60 min | buy the 1-hour tier once |
| over 60 min | accept the miss |

## Validation

`warm_interval_seconds` must be **less than** the shortest tier's `ttl_seconds`.
An interval at or beyond the TTL never refreshes anything while still paying for
every request it sends, so it is rejected at config load rather than discovered
on a bill.

Duplicate TTLs, non-positive TTLs, and negative multipliers are also rejected.

## The two cache plugins

Both are optional, both are off unless you configure them, and both decline to
act when they cannot price what they are about to do. Like every official
plugin they are distributed from
[torana-plugins](https://github.com/torana-edge/torana-plugins) and installed
with `torana plugin install`; nothing is bundled into the proxy.

### cache_tier_selector

Chooses which lifetime to buy for a conversation. It watches how long a
conversation actually goes idle and, once it has seen a pause long enough to
lose the short tier, switches that conversation to the longer one.

```json
{ "plugins": { "config": { "cache_tier_selector": { "mode": "auto" } } } }
```

`auto` decides per conversation; `long` and `short` force it; `off` disables it.

It needs no budget and sends no requests — it only changes an existing,
representable cache marker on requests you were already making. If the request
has no explicit breakpoint carrier, it declines without reading state or
changing the request. The decision is made once per cached prefix and never
revisited, because changing the marker changes the prefix and would invalidate
the entry it is protecting.

### cache_warmer

Refreshes a chosen conversation's explicit, refresh-on-read breakpoint so an
idle gap does not cost you a rebuild. It is not a generic request repeater and
does not manage external cache resources such as Gemini `cachedContents`.
Requires `plugins.runtime.tick_interval_seconds` and an egress budget (see
[Running plugins](PLUGINS.md)).

```json
{
  "plugins": {
    "runtime": {
      "tick_interval_seconds": 60,
      "egress": { "cache_warmer": { "max_calls_per_minute": 4 } }
    },
    "config": {
      "cache_warmer": { "conversations": "a3f9c2e1", "warm_for_minutes": 45 }
    }
  }
}
```

Pick conversation IDs from `torana conversations`, or from the picker on the
plugin's page in the control plane, which shows each conversation's model, age
and cache state and greys out ones whose cache is long gone.

**It is opt-in per conversation on purpose.** Warming everything would lose
money on every conversation you never return to — see the arithmetic above. It
stops on whichever comes first: the deadline, the break-even refresh count, or a
refresh that reports a cache *write*, which means the entry had already lapsed
and holding it open is no longer preserving anything.

## Seeing what actually happened

Providers report cache token counts per request, and those are the ground truth:

```
torana conversations

ID            LAST ACTIVE  TURNS  MODEL              CACHE
a3f9c2e1      2m ago       12     claude-sonnet-4-5  118k read
7b1e04aa      41m ago      3      gemini-2.5-pro     62k read
```

**Read** tokens mean the prefix was served from cache. **Written** tokens mean it
had to be rebuilt — the entry had lapsed. A conversation resuming after an idle
gap that shows reads was kept warm; one that shows writes was not.

Add `--json` for the full record, including the cache prefix key.
