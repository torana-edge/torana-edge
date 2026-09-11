# Official plugin-chain benchmark — 2026-08-18

This run measures a real four-plugin WASM chain under a coding-agent-shaped
request, not an empty guest or host-only microbenchmark. It is capacity evidence
for this machine and configuration—not a universal Torana throughput claim.
The [raw JSONL](benchmark-plugin-chain-2026-08-18.jsonl) is retained beside this
report.

## Setup

- Edge: `a7b3f076d960f934b730a9be153cdb2812c20422`, plus the benchmark-only
  `agent` request-shape change in this PR
- Official plugins: `ea45ece8c16318b6890572b2c1d76fd80c0d7491`
- Chain: `schema_translator → intent → keyword_compactor → otel`
- Request: 16 KiB historical tool result, system/user/assistant/tool turns, a
  tool call, and a map-valued tool schema
- Controlled upstream: 100 ms first-byte delay, 4 KiB OpenAI-compatible reply
- Host: Ryzen 7 4800H (8 cores/16 threads), 16 GB RAM, Linux 7.1.5, Go 1.26.6
- All processes shared one otherwise-idle machine over loopback.

The request exercises schema translation, intent's heuristic fill path,
keyword-compactor eligibility/cache lookup, and OTel metrics. No compaction is
expected: heuristic intent intentionally does not publish the shared intent
artifact that would authorize keyword replacement.

## Results

All measured requests succeeded.

| configuration | concurrency | rps | p50 | p99 | peak RSS | CPU/request |
|---|---:|---:|---:|---:|---:|---:|
| no plugins | 1 | 9.41 | 106.3 ms | 107.0 ms | 33.6 MiB | 8.7 ms |
| 4 plugins, pool 4 | 1 | 4.71 | 205.8 ms | 286.0 ms | 441.2 MiB | 113.4 ms |
| no plugins | 8 | 74.12 | 107.6 ms | 112.0 ms | 39.5 MiB | 9.4 ms |
| 4 plugins, pool 4 | 8 | 38.90 | 188.8 ms | 327.9 ms | 785.4 MiB | 99.0 ms |
| 4 plugins, pool 2 | 8 | 21.71 | 350.6 ms | 542.6 ms | 613.6 MiB | 110.0 ms |
| 4 plugins, pool 2, 32 MiB cap | 8 | 21.44 | 331.5 ms | 682.6 ms | 606.1 MiB | 111.9 ms |

The pool-4 concurrency-8 soak ran for five minutes: 12,269 requests, zero
errors, 40.88 rps, 186.0 ms p50, 312.5 ms p99, 356.8 ms maximum, and 915.9 MiB
peak RSS. RSS rose from 785.5 MiB as the pools warmed and then plateaued; this
run found no unbounded growth, but the steady footprint is too high to ignore.

## Conclusions

The official chain is stable under this five-minute load, but guest execution
is the dominant local cost. At concurrency 8, the chain roughly halves
throughput relative to the host-only proxy and consumes about 90 ms of CPU per
request beyond the no-plugin path. The 100 ms controlled upstream makes that
cost intentionally visible; with multi-second providers, wall-clock percentage
overhead will be much smaller while CPU cost remains.

Reducing the global pool from four to two is not a good default on this evidence:
it saves about 172 MiB at concurrency 8 but loses 44% throughput and raises p50
by 86%. Lowering the per-instance memory ceiling from 64 MiB to 32 MiB at pool
two saves only another 7.5 MiB in the concurrency-8 row and worsens tail latency.
Neither knob addresses the underlying per-instance footprint.

The next optimization should therefore profile guest linear memory and Go-WASM
runtime allocations, then consider per-plugin pool sizing or smaller guest
toolchains. Defaults should not change until those options are measured against
this same workload and the streaming integrity suite.

## Limits

This run is not a cloud benchmark, a multi-node result, a provider SLA, or a
cost estimate. It uses one laptop, loopback networking, one request shape, one
plugin order, non-streaming responses, and a controlled provider. Repeat on the
intended deployment hardware and representative plugin/configuration mix before
capacity planning.
