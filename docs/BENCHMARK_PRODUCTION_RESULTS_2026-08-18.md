# Post-rewrite production-shaped benchmark — 2026-08-18

This is a repeatable single-machine comparison, not a production SLA or a
universal capacity claim. It reruns the existing production harness after the
ABI-v1 migration and security hardening. The complete raw output is
[`benchmark-production-2026-08-18.jsonl`](benchmark-production-2026-08-18.jsonl).

## Method

- Runtime tree: `9d9979c292371bf2be9b3d7783f7f3c0be5adc1e` (documentation-only
  child of Edge main; no runtime delta)
- Host: AMD Ryzen 7 4800H, 8 cores / 16 threads, 14 GiB RAM
- OS: Linux 7.1.5-arch1-2 amd64
- Go: 1.26.6, `GOMAXPROCS=16`
- Command: `./scripts/benchmark-production.sh results.jsonl`
- Processes: a static Torana binary, a separate controlled upstream, and an
  external load generator
- Request: 4 KiB OpenAI-compatible user message
- Non-streaming response: 100 ms upstream delay and a 4 KiB completion
- Streaming response: 100 ms to first byte, 100 text events 10 ms apart, a
  finish event, and `[DONE]`
- Plugins: none; plugin boundary costs are isolated in
  [BENCHMARKS.md](BENCHMARKS.md)
- Per row: 2-second warmup followed by at least 10 measured seconds

Every row completed with zero errors. Every stream contained exactly 101 data
events, so the comparison does not hide truncation.

## Non-streaming

| Concurrency | Path | req/s | p50 | p95 | p99 | Torana CPU | Torana peak RSS |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | direct | 9.89 | 101.14 ms | 101.43 ms | 101.52 ms | idle floor | 26.6 MB |
| 1 | Torana | 9.73 | 102.66 ms | 103.77 ms | 104.24 ms | 0.27 s / 98 req | 29.7 MB |
| 8 | direct | 79.02 | 101.24 ms | 101.64 ms | 101.86 ms | idle floor | 28.9 MB |
| 8 | Torana | 77.29 | 103.39 ms | 104.76 ms | 105.43 ms | 2.67 s / 776 req | 33.4 MB |
| 32 | direct | 315.71 | 101.18 ms | 102.39 ms | 103.23 ms | idle floor | 32.6 MB |
| 32 | Torana | 307.68 | 103.31 ms | 106.19 ms | 109.20 ms | 9.14 s / 3,107 req | 42.2 MB |

Torana added 1.52–2.15 ms at p50 and 2.72–5.98 ms at p99. Throughput
was 1.6–2.5% lower while the controlled 100 ms upstream remained the
bottleneck. Torana used approximately 2.8–3.4 ms CPU per request: roughly
0.8–1.0 CPU-hours per million requests of this exact shape before plugins.

## Streaming

| Concurrency | Path | streams/s | p50 complete stream | p95 | events/stream | Torana CPU |
|---:|---|---:|---:|---:|---:|---:|
| 1 | direct | 0.864 | 1,156.22 ms | 1,161.62 ms | 101 | idle floor |
| 1 | Torana | 0.864 | 1,156.99 ms | 1,162.35 ms | 101 | 0.33 s / 9 streams |
| 8 | direct | 6.861 | 1,164.72 ms | 1,177.40 ms | 101 | idle floor |
| 8 | Torana | 6.841 | 1,168.08 ms | 1,177.23 ms | 101 | 1.95 s / 72 streams |

The complete stream p50 delta was 0.77 ms at concurrency 1 and 3.37 ms at
concurrency 8, spread across roughly 1.16 seconds of delivery. The Torana
process used 27–37 ms CPU per complete 101-event stream, or approximately
7.5–10.2 CPU-hours per million streams of this exact shape before stream
plugins.

## Interpretation

- The rewritten platform retains low single-digit-millisecond median overhead
  on provider-shaped traffic.
- The proxy itself adds no model-token charge. Plugins that deliberately make
  model-service or cache-warming calls can spend model tokens, but those capabilities
  and budgets are explicit and were absent here.
- Infrastructure cost should be calculated from the measured CPU-hours and
  memory using the operator's deployment price. A universal dollar figure
  would be misleading.
- The largest observed Torana RSS was 42.2 MB. Rows share one process, so this
  is a process high-water mark, not isolated per-request heap usage.

## Limits and next measurements

- Loopback controls variance but is not the public internet.
- No plugin was loaded. Existing microbenchmarks cover WASM boundary and
  verifier cost; a long soak with a representative official plugin chain is
  still needed.
- Ten seconds per row is a release baseline, not a saturation ceiling.
- The next matrix should vary payload size and hold the upstream delay near
  zero to identify CPU saturation. Keep that result separate from this
  provider-shaped latency comparison.
