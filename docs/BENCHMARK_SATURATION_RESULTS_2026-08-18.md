# Saturation and payload-scaling benchmark — 2026-08-18

This run locates scaling knees for one local Torana process. It is not a
universal capacity claim and it is not a provider-latency benchmark.

## Result

All 24 measured rows completed with zero request errors. Torana sustained the
following rates while parsing, validating, converting, routing, and rendering
each request:

| Request body | Concurrency | Requests/s | p50 | p99 | Peak RSS | CPU/request |
|---:|---:|---:|---:|---:|---:|---:|
| 1 KiB | 1 | 737 | 1.29 ms | 2.46 ms | 29.9 MiB | 1.60 ms |
| 1 KiB | 8 | 6,072 | 1.30 ms | 2.10 ms | 34.6 MiB | 1.00 ms |
| 1 KiB | 32 | 7,720 | 3.68 ms | 11.23 ms | 46.8 MiB | 0.96 ms |
| 1 KiB | 128 | 9,652 | 11.86 ms | 39.36 ms | 60.7 MiB | 0.85 ms |
| 16 KiB | 1 | 238 | 4.25 ms | 5.21 ms | 36.8 MiB | 6.69 ms |
| 16 KiB | 8 | 2,072 | 3.82 ms | 5.26 ms | 40.7 MiB | 3.16 ms |
| 16 KiB | 32 | 2,528 | 11.96 ms | 27.37 ms | 50.5 MiB | 3.15 ms |
| 16 KiB | 128 | 3,326 | 36.86 ms | 98.10 ms | 91.9 MiB | 2.81 ms |
| 128 KiB | 1 | 45 | 22.40 ms | 24.65 ms | 36.3 MiB | 35.47 ms |
| 128 KiB | 8 | 539 | 14.79 ms | 17.83 ms | 53.9 MiB | 14.50 ms |
| 128 KiB | 32 | 667 | 45.56 ms | 95.32 ms | 149.6 MiB | 16.43 ms |
| 128 KiB | 128 | 758 | 154.29 ms | 617.08 ms | 288.6 MiB | 15.56 ms |

The useful conclusions are bounded:

- Small requests continue scaling through concurrency 128 on this machine,
  reaching about 9.65k requests/s.
- Payload size materially changes the cost. At 128 KiB and concurrency 128,
  p99 reaches 617 ms and peak process RSS reaches 289 MiB. Large-body memory
  and concurrency work therefore remains important; this run does not close
  the corresponding capacity issue.
- CPU/request falls with concurrency because fixed process and scheduling work
  is amortized. It is not a model-token charge or a cloud cost estimate.
- Direct rows are a harness control, not a Torana-overhead denominator. The
  controlled upstream only reads the body and returns a static response,
  whereas Torana performs the complete provider conversion path.

## Method

- Revision: `100c94715ffd8420a829e751ba32f7ae3c2faec3`
- CPU: AMD Ryzen 7 4800H, 8 cores / 16 threads
- OS: Linux 7.1.5-arch1-2, amd64
- Go: 1.26.6, `GOMAXPROCS=16`
- One Torana process, one controlled upstream process, and one external load
  generator shared the same machine.
- The upstream had zero artificial delay and returned 4 KiB responses.
- No plugins were loaded. Each row ran for 10 seconds after a 2-second warmup.
- Request bodies were 1, 16, and 128 KiB at concurrency 1, 8, 32, and 128.

Reproduce with:

```bash
TORANA_BENCH_PROFILE=saturation \
  ./scripts/benchmark-production.sh saturation.jsonl
```

The complete machine-readable output is retained in
[`benchmark-saturation-2026-08-18.jsonl`](benchmark-saturation-2026-08-18.jsonl).
The provider-shaped latency run remains the appropriate evidence for normal
traffic: [`BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md`](BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md).
