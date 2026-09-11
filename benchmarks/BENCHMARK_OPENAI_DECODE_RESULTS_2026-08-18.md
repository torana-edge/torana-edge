# OpenAI scalar decode allocation result (2026-08-18)

OpenAI Chat Completions scalar content previously crossed two avoidable
whole-value copies:

- variant detection bound the complete top-level `input` and `messages` values
  to `json.RawMessage` merely to test whether the members existed;
- each message bound `content` to another `json.RawMessage`, then decoded that
  copy into the durable scalar string.

Variant detection now uses a presence-only JSON target. Scalar content decodes
directly into its durable string, while structured arrays keep independent raw
elements because each arm still needs provider-grammar projection. Explicit
`null`, absent content, empty strings, structured arrays, wrong shapes, and the
top-level variant precedence are pinned against the prior semantics.

## Isolated adapter benchmark

`BenchmarkUnmarshalLargeScalar` uses a 1 MiB scalar user message. Three runs on
the same machine and Go 1.26.6 produced:

| Implementation | Allocated bytes/op | Allocs/op | Throughput range |
|---|---:|---:|---:|
| Edge main `1757b3b` | 4,221,981–4,221,996 | 55 | 27.4–31.2 MB/s |
| Presence/direct-scalar decode | 2,108,409–2,108,421 | 53 | 40.8–48.4 MB/s |

That is a 50.1% allocation reduction in the isolated decoder.

## Production-path check

The standard production profile used an 8 MiB agent-shaped OpenAI request,
concurrency one, no warmup, and a three-second measured window on exact main
`1757b3b` before and after only this decoder change.

| Metric | Before | After | Change |
|---|---:|---:|---:|
| Allocated bytes/request | 121,102,134 | 95,728,133 | -21.0% |
| Mallocs/request | 2,153.9 | 2,137.9 | -0.7% |
| GC cycles | 44 | 40 | -9.1% |
| Errors | 0 | 0 | unchanged |

The allocation result is the claim. The three-second latency, throughput, and
peak-RSS samples remain in the raw files but are not capacity claims.

## Combined result after the ingress fix

After PR #308 merged, a matched ten-second run compared the exact pre-#308 main
with the combined ingress and decoder changes. The longer window amortizes
startup over 35 and 45 Torana requests respectively:

| Metric | Before both units | Combined | Change |
|---|---:|---:|---:|
| Allocated bytes/request | 121,303,100 | 62,187,901 | -48.7% |
| Mallocs/request | 2,046.0 | 1,964.3 | -4.0% |
| GC cycles/request | 3.86 | 1.89 | -51.0% |
| Errors | 0 | 0 | unchanged |

The combined run also moved p50 from 287.1 ms to 222.9 ms and throughput from
3.46 to 4.44 requests/second on this controlled no-delay workload. Those are
useful regression signals, not universal capacity or provider-latency claims.

- [`benchmark-openai-decode-before-2026-08-18.jsonl`](benchmark-openai-decode-before-2026-08-18.jsonl)
- [`benchmark-openai-decode-before-2026-08-18-summary.jsonl`](benchmark-openai-decode-before-2026-08-18-summary.jsonl)
- [`benchmark-openai-decode-after-2026-08-18.jsonl`](benchmark-openai-decode-after-2026-08-18.jsonl)
- [`benchmark-openai-decode-after-2026-08-18-summary.jsonl`](benchmark-openai-decode-after-2026-08-18-summary.jsonl)
- [`benchmark-openai-decode-combined-before-2026-08-18.jsonl`](benchmark-openai-decode-combined-before-2026-08-18.jsonl)
- [`benchmark-openai-decode-combined-before-2026-08-18-summary.jsonl`](benchmark-openai-decode-combined-before-2026-08-18-summary.jsonl)
- [`benchmark-openai-decode-combined-after-2026-08-18.jsonl`](benchmark-openai-decode-combined-after-2026-08-18.jsonl)
- [`benchmark-openai-decode-combined-after-2026-08-18-summary.jsonl`](benchmark-openai-decode-combined-after-2026-08-18-summary.jsonl)

## Follow-up: direct scalar marshal

The corresponding output path used to encode scalar content into an 8 MiB
`json.RawMessage`, then copy it into the final request object. Keeping scalar
strings and structured arrays typed until the one final `json.Marshal` removes
that intermediate body while preserving byte-exact wire output.

The isolated 1 MiB marshal benchmark moved from 2.16–2.22 MiB and 44
allocations/op to 1.22–1.28 MiB and 41 allocations/op. Throughput increased
from 79.6–84.2 MB/s to 344.9–369.3 MB/s.

Against the matched ten-second combined row above, the production result moved
from 62,187,901 to 53,845,228 allocated bytes/request (-13.4%), 1,964.3 to
1,957.9 mallocs/request, and zero errors in both runs. Across all three narrow
units, the original matched baseline moves from 121,303,100 to 53,845,228
allocated bytes/request (-55.6%).

- [`benchmark-openai-marshal-after-2026-08-18.jsonl`](benchmark-openai-marshal-after-2026-08-18.jsonl)
- [`benchmark-openai-marshal-after-2026-08-18-summary.jsonl`](benchmark-openai-marshal-after-2026-08-18-summary.jsonl)

## Follow-up: fused provider-extension extraction

All provider adapters preserved unknown top-level fields by first copying the
entire request into an `OptionalJSONObject` and then deleting canonical fields.
The new fused span projection validates once and constructs only the retained
extension object. Its corpus is byte-compared against the former two-stage
result, including whitespace, escape-equivalent keys, large numeric lexemes,
nested objects, missing exclusions, and invalid JSON-text cases.

The 1 MiB engine benchmark moves from 1,058,805–1,058,809 bytes allocated/op to
2,032 bytes/op. The matched production row moves from 53,845,228 to 45,853,411
allocated bytes/request (-14.8%), with zero errors in both runs. Across all four
units the original 121,303,100-byte baseline falls 62.2% to 45,853,411 bytes.

- [`benchmark-extension-extract-after-2026-08-18.jsonl`](benchmark-extension-extract-after-2026-08-18.jsonl)
- [`benchmark-extension-extract-after-2026-08-18-summary.jsonl`](benchmark-extension-extract-after-2026-08-18-summary.jsonl)
