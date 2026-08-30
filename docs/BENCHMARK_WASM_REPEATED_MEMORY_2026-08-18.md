# WASM guest repeated-call memory profile — 2026-08-18

This follow-up to
[`BENCHMARK_WASM_LINEAR_MEMORY_2026-08-18.md`](./BENCHMARK_WASM_LINEAR_MEMORY_2026-08-18.md)
distinguishes bounded first-use heap growth from memory that grows with request
count. It is a retention probe, not a throughput benchmark.

## Method

The current Go and Rust logger guests, their source revision, SHA-256
digests, pool configuration, and 16 KiB coding-agent request are identical to
the one-call profile. The bundle-gated
`TestGuestLinearMemoryRepeatedProfile` deterministically drives every request
through each of four retained instances and records WASM linear memory after
1, 10, 100, 1,000, and 10,000 calls per instance. Each guest runs in a fresh
test process. Process RSS is informational; linear memory is the portable WASM
measurement.

Raw records are retained in
[`benchmark-wasm-repeated-memory-2026-08-18.jsonl`](./benchmark-wasm-repeated-memory-2026-08-18.jsonl).

## Result

| Guest | Init / instance | 1 call | 10 calls | 100 calls | 1,000 calls | 10,000 calls |
|---|---:|---:|---:|---:|---:|---:|
| standard Go | 7.00 MiB | 7.50 MiB | 7.50 MiB | 11.00 MiB | 12.00 MiB | 12.00 MiB |
| Rust | 1.0625 MiB | 1.0625 MiB | 1.0625 MiB | 1.0625 MiB | 1.0625 MiB | 1.0625 MiB |

The Go guest grows by 5 MiB per instance before reaching a stable 12 MiB by
1,000 calls. It remains exactly 12 MiB through another 9,000 calls per
instance. Fresh-process RSS similarly moves from 122.0 MiB after the first
call to 146.8 MiB at 1,000 and 148.0 MiB at 10,000. This is bounded Go runtime
heap retention, not evidence of an unbounded per-request leak.

The Rust guest's declared linear memory remains exactly 1.0625 MiB at every
checkpoint. Its RSS fluctuates between 27.9 and 30.5 MiB without a monotonic
trend.

## Decision

- Keep the current pool and memory ceiling unchanged: the retained growth
  plateaus well below the 64 MiB instance limit, and the measured pool-size
  reduction materially reduced throughput.
- Treat 12 MiB, rather than the 7–7.5 MiB first-call value, as the standard-Go
  steady-state linear-memory budget for this request shape.
- Continue recommending Rust for memory-sensitive plugin deployments. This
  evidence is about memory footprint, not a claim that one language is always
  faster or otherwise preferable.
- Investigate compiled-module and host-side per-instance retention before
  considering idle retirement. Any lifecycle change must preserve hook
  initialization, grants, logging, approvals, and hot reload.

This is a single-machine engineering measurement and does not replace an
end-to-end capacity benchmark.
