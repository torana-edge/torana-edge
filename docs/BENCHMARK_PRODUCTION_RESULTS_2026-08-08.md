# Production-shaped local benchmark — 2026-08-08

This is a repeatable single-machine measurement, not a production SLA or a
universal capacity claim. The complete raw output is
[`benchmark-production-2026-08-08.jsonl`](benchmark-production-2026-08-08.jsonl).

## Method

- Harness revision: `146dfe5d5670d714e31f2850cd68d4147ca9f910`
- Host: AMD Ryzen 7 4800H, 8 cores / 16 threads, 14 GiB RAM
- OS: Linux 7.1.5 x86-64
- Go: `go1.26.5-X:nodwarf5`, `GOMAXPROCS=16`
- Command: `./scripts/benchmark-production.sh results.jsonl`
- Processes: a built static Torana binary, a separate controlled upstream, and
  an external load generator
- Request: 4 KiB OpenAI-compatible user message
- Non-streaming response: 100 ms upstream delay, then a 4 KiB completion
- Streaming response: 100 ms to first byte, then 100 text events at 10 ms
  intervals, a finish event, and `[DONE]`
- Plugins: none. Plugin boundary costs remain isolated in
  [`BENCHMARKS.md`](BENCHMARKS.md).
- Each row: 2-second warmup, then at least 10 measured seconds

Every row completed with zero errors. Every successful direct and Torana stream
contained exactly 101 data events, so the latency comparison did not hide a
truncated response.

## Non-streaming results

| Concurrency | Path | req/s | p50 | p95 | p99 | Torana CPU | Torana peak RSS |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | direct | 9.91 | 100.90 ms | 101.13 ms | 101.67 ms | idle floor | 26.6 MB |
| 1 | Torana | 9.75 | 102.56 ms | 103.38 ms | 103.67 ms | 0.25 s / 98 req | 30.9 MB |
| 8 | direct | 79.12 | 101.08 ms | 101.59 ms | 101.90 ms | idle floor | 30.6 MB |
| 8 | Torana | 77.47 | 103.11 ms | 104.66 ms | 106.75 ms | 2.58 s / 776 req | 35.3 MB |
| 32 | direct | 316.06 | 101.08 ms | 102.24 ms | 102.98 ms | idle floor | 33.7 MB |
| 32 | Torana | 307.97 | 103.16 ms | 105.75 ms | 109.41 ms | 9.19 s / 3,108 req | 41.3 MB |

Torana added 1.66–2.08 ms at p50. The p99 delta was 2.00 ms at concurrency 1,
4.85 ms at concurrency 8, and 6.43 ms at concurrency 32. Throughput was 1.6–2.6%
lower than the direct arm while the controlled 100 ms upstream remained the
bottleneck.

The Torana process consumed roughly 2.6–3.3 ms of CPU per completed request in
these rows. Put another way, this shape would consume about 0.7–0.9 CPU-hours
per million requests before plugins. That is a resource quantity, not a dollar
price: hosted cost depends on instance pricing and utilization, while a local
developer already owns the idle CPU.

## Streaming results

| Concurrency | Path | streams/s | p50 complete stream | p95 | events/stream | Torana CPU |
|---:|---|---:|---:|---:|---:|---:|
| 1 | direct | 0.880 | 1,135.78 ms | 1,138.56 ms | 101 | idle floor |
| 1 | Torana | 0.879 | 1,137.93 ms | 1,139.85 ms | 101 | 0.32 s / 9 streams |
| 8 | direct | 6.957 | 1,151.24 ms | 1,168.63 ms | 101 | idle floor |
| 8 | Torana | 6.794 | 1,182.00 ms | 1,189.48 ms | 101 | 1.88 s / 72 streams |

At concurrency 1 the complete 100-event stream was 2.15 ms slower at p50. At
concurrency 8 it was 30.76 ms slower at p50, spread across roughly 1.15 seconds
of delivery. The Torana process consumed about 26–36 ms CPU per complete
100-event stream (0.26–0.35 ms per event). A million streams of this exact shape
would therefore consume roughly 7.2–9.9 CPU-hours before stream plugins.

## Memory and model cost

The largest observed Torana RSS was 41.3 MB. The process was reused across rows,
so start-to-peak deltas are useful for spotting growth but are not isolated
per-scenario heap measurements; allocation profiling remains owned by
torana-edge issue #276.

The proxy itself adds no model-token charge. Plugins that deliberately make
provider calls—such as model-service compaction or cache warming—can add model spend,
but those calls require explicit capabilities and operator budgets and are not
present in this run. Infrastructure dollars should be calculated from the CPU
hours and memory above using the deployment's own price, not from an invented
universal Torana fee.

## Boundaries

- This is loopback with controlled latency and cadence, not the public internet.
- No plugin is loaded. The separate plugin benchmarks measure request and
  per-event boundary cost, including the known allocation pressure.
- Ten seconds is enough for a release baseline, not a soak or saturation test.
- The 9-sample concurrency-1 streaming tail percentiles are descriptive only.
- The next capacity step is a longer containerized saturation/soak run with a
  representative official plugin set and allocation profiles from issue #276.
