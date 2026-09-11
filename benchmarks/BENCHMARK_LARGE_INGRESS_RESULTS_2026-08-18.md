# Large-request ingress allocation result (2026-08-18)

This measurement isolates two redundant allocations at Torana's HTTP ingress:

1. the request boundary read the bounded body and reset `Request.Body`;
2. `ReverseProxy.Rewrite` read that same reset body again;
3. both reads used `io.ReadAll`, whose geometric growth allocated roughly twice
   the final body size for an 8 MiB request.

The implementation now keeps the already bounded bytes in the request-scoped
host state, reuses that immutable snapshot in `Rewrite`, and seeds byte
accounting from the boundary read. A content-length-aware reader preallocates
the common exact-length case while treating `Content-Length` only as a hint;
`http.MaxBytesReader` remains the limit and error authority. Direct
`ReverseProxy` test entry points retain the historical read-once fallback.

## Method

Both runs used the production benchmark harness, the same machine, Go 1.26.6,
one non-streaming client, an 8 MiB agent-shaped OpenAI request, no warmup, and a
three-second measured window:

```sh
TORANA_BENCH_PROFILE=large \
TORANA_BENCH_REQUEST_SHAPE=agent \
TORANA_BENCH_DURATION=3s \
TORANA_BENCH_WARMUP=0s \
TORANA_BENCH_PAYLOAD_BYTES=8388608 \
TORANA_BENCH_NONSTREAM_CONCURRENCY=1 \
TORANA_BENCH_RUN_STREAM=0 \
TORANA_BENCH_PROFILE_DIR=profiles \
./scripts/benchmark-production.sh results.jsonl
```

The baseline is Edge main `d44fc76a3b9cb240bdbc5ea2e26644813fc90064`.
The after run used the same revision plus only the ingress-body working-tree
change described above.

## Result

| Metric | Before | After | Change |
|---|---:|---:|---:|
| Allocated bytes/request | 119,223,944 | 92,615,125 | -22.3% |
| Mallocs/request | 2,171.2 | 2,099.6 | -3.3% |
| GC cycles | 50 | 30 | -40.0% |
| Torana requests/second | 3.25 | 3.51 | +8.1% |
| p50 latency | 301.7 ms | 276.4 ms | -8.4% |
| Errors | 0 | 0 | unchanged |

The allocation and GC reductions are the primary result. The short-run
throughput and latency movement is directionally consistent but is not a
capacity claim. Peak RSS is intentionally not claimed: Go heap retention and
the small request counts make that value noisy across isolated runs.

Raw inputs:

- [`benchmark-large-ingress-before-2026-08-18.jsonl`](benchmark-large-ingress-before-2026-08-18.jsonl)
- [`benchmark-large-ingress-before-2026-08-18-summary.jsonl`](benchmark-large-ingress-before-2026-08-18-summary.jsonl)
- [`benchmark-large-ingress-after-2026-08-18.jsonl`](benchmark-large-ingress-after-2026-08-18.jsonl)
- [`benchmark-large-ingress-after-2026-08-18-summary.jsonl`](benchmark-large-ingress-after-2026-08-18-summary.jsonl)
