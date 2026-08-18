# Unchanged inference wire fast path — 2026-08-18

## Result

Torana can validate and observe a supported inference request without
serializing it a second time when neither a plugin nor the host changes any
provider-visible request field. On the matched 8 MiB coding-agent-shaped row,
this reduced allocation by another **44.6%** over merged PR #311:

| Metric | PR #311 baseline | Unchanged-wire fast path | Change |
|---|---:|---:|---:|
| allocated bytes/request | 45,853,411 | 25,389,748 | -44.6% |
| requests/second | 5.23 | 5.79 | +10.7% |
| p50 latency | 193.93 ms | 172.93 ms | -10.8% |
| p95 latency | 231.85 ms | 213.18 ms | -8.1% |
| peak RSS | 103.31 MiB | 78.89 MiB | -23.6% |
| request errors | 0 | 0 | unchanged |

Across the matched PR #308-through-this-unit train, the same row moved from
121,303,100 to 25,389,748 allocated bytes/request: a **79.1%** reduction.
Allocation amplification is now about 3.03 bytes per payload byte rather than
14.46 at that retained matched baseline.

The full 1/4/8 MiB by concurrency 1/4/8 follow-up completed all 18 direct and
Torana rows with zero errors. Torana allocation stayed nearly independent of
concurrency at approximately 3.36 MiB, 12.80 MiB, and 25.38 MiB per request,
respectively. That consistency supports the ownership model: the remaining
cost is dominated by the bounded input, canonical IR, and cache-prefix
projection rather than concurrency-dependent retained copies.

## Correctness boundary

This is not a byte-proxy bypass. Every recognized inference request still:

1. passes strict JSON-text validation and provider parsing;
2. passes the canonical SDK replacement-domain closure;
3. contributes routing, identity, conversation, rate-limit, cache-prefix, and
   response-observation facts;
4. runs every configured request hook and its side effects; and
5. follows the existing block, respond, failure-mode, and host-error rules.

The original validated body is reused only when provider-visible state remains
unchanged. Accepted plugin replacements, route model overrides, OpenAI stream
usage injection, and Responses compaction all force the ordinary provider
marshal path. Cross-format real HTTP tests pin byte-exact unchanged forwarding
for OpenAI Chat Completions, Anthropic Messages, Bedrock Converse, Gemini, and
Gemini Code Assist; a real pass-only hook remains transparent and a real
request-mutating guest forces reserialization.

Auxiliary provider endpoints remain outside this path entirely. Status,
model-list, authentication/account, file, batch, telemetry, and other
non-inference traffic continues through Torana as transparent pass-through
traffic, which is separately pinned by the selective-interception suite.

## Method

- Candidate revision: `2e278a3fd9824b25b90c97498f53d789a351f455`.
- Baseline: merged PR #311's exact 10-second 8 MiB/concurrency-1 row.
- CPU: AMD Ryzen 7 4800H, 8 cores / 16 threads.
- OS: Linux amd64; Go 1.26.6; `GOMAXPROCS=16`.
- No plugins for the numeric row; OpenAI Chat Completions; one large tool
  result and one tool definition; zero-delay local upstream.
- Duration 10 seconds, no uncounted warmup, forced-GC counters before and
  after the Torana interval.
- The complete follow-up used the same agent-shaped request generator for
  1/4/8 MiB payloads at concurrency 1/4/8, three seconds per row.

The figures are local comparative measurements, not a public capacity SLA.
Peak RSS is a process high-water mark and differs from forced-GC live heap.
The exact matched raw row and runtime-counter summary are retained in
[`benchmark-unchanged-wire-2026-08-18.jsonl`](benchmark-unchanged-wire-2026-08-18.jsonl)
and
[`benchmark-unchanged-wire-2026-08-18-summary.jsonl`](benchmark-unchanged-wire-2026-08-18-summary.jsonl).
