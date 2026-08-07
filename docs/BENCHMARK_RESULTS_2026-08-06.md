# Local data-plane benchmark — 2026-08-06

This is a reproducible local baseline, not a universal capacity claim.

## Method

- Edge base revision: `12931308221c419b9fa9c937fbb32b627119002f`
- Benchmark: `BenchmarkHTTPDataPlane` from this change
- Host: AMD Ryzen 7 4800H, 8 cores / 16 threads
- OS: Linux 7.1.5 x86-64
- Go: `go1.26.5-X:nodwarf5 linux/amd64`
- Command: `go test ./internal/proxy -run '^$' -bench BenchmarkHTTPDataPlane -benchmem -benchtime=1s -count=3`
- Payload: one small OpenAI-compatible chat request and one small completion
- Upstream: the same in-process loopback HTTP server for both arms
- Plugins: none
- Logs: discarded in both measured arms

The direct arm measures the local HTTP/client/test-server floor. The Torana arm
adds request parsing, OpenAI adaptation, routing, reverse proxying, response
parsing, usage accounting, and response rendering. Provider/network latency is
intentionally absent.

## Results

Medians of three samples:

| Concurrency | Direct throughput | Torana throughput | Torana elapsed/op | Torana allocations |
|---:|---:|---:|---:|---:|
| 1 | 6,644 req/s | 1,585 req/s | 631 µs | 110 KB, 944 allocs |
| 8 | 57,437 req/s | 6,316 req/s | 158 µs aggregate | 137 KB, 978 allocs |
| 32 | 85,036 req/s | 9,634 req/s | 104 µs aggregate | 113 KB, 974 allocs |

At concurrency 1, the paired median difference is approximately **481 µs per
request** on this machine. With 32 local clients, the harness sustained roughly
**9.6k non-streaming requests/s** through Torana. The concurrency rows report
aggregate elapsed time per completed operation; multiply by concurrency for a
rough mean in-flight latency (about 1.27 ms at 8 and 3.32 ms at 32).

## What this establishes

- Torana's local non-streaming overhead is sub-millisecond for this small
  request on this machine before plugins.
- The local data plane has substantial headroom relative to normal remote LLM
  request rates in this synthetic shape.
- Allocation volume (roughly 100+ KB and about 900 extra allocations per small
  request) is a more useful optimization target than raw single-request
  latency.

## What it does not establish

- A production SLA or a maximum supported traffic number.
- Streaming capacity; provider event cadence and stream plugins dominate that
  path and are measured separately in `internal/plugin`.
- Capacity with official plugins, rate-limit persistence, verbose logs, TLS,
  real provider latency, large conversations, or constrained containers.
- Infrastructure price. Torana adds no model-token charge. A dollar estimate
  requires deployment-specific CPU utilization, memory, region, and instance
  pricing; wall-clock microbenchmark time is not a CPU billing measurement.

The next publishable load report should run a built Torana binary in a clean
container, cover non-streaming and streaming traffic at several payload sizes,
compare zero/one/representative plugin sets, record p50/p95/p99 latency and RSS,
and hold each load level long enough to observe GC and saturation.
