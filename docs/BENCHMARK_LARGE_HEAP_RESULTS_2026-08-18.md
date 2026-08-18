# Near-limit request heap and GC profile — 2026-08-18

This run decomposes the whole-process RSS signal from the earlier near-limit
request benchmark. It answers the next question in issue #199: is memory being
retained, or is Torana allocating and collecting many temporary copies while a
large request crosses the adapter and validation boundaries?

## Result

All 18 direct/Torana rows completed with zero request errors. A forced GC was
run immediately before and after each Torana row. The measured Torana rows were:

| Tool result | Concurrency | Requests | Allocated/request | Mallocs/request | GCs | GC pause | Heap growth after forced GC | Peak RSS |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 MiB | 1 | 22 | 53.7 MiB | 3,050 | 207 | 21.5 ms | 1.2 MiB | 50.5 MiB |
| 1 MiB | 4 | 106 | 53.7 MiB | 2,972 | 344 | 86.2 ms | 2.2 MiB | 80.8 MiB |
| 1 MiB | 8 | 254 | 53.6 MiB | 2,969 | 415 | 155.2 ms | 2.3 MiB | 134.8 MiB |
| 4 MiB | 1 | 8 | 216.4 MiB | 3,018 | 94 | 8.2 ms | 4.1 MiB | 98.5 MiB |
| 4 MiB | 4 | 35 | 215.9 MiB | 3,009 | 126 | 53.2 ms | 8.2 MiB | 219.5 MiB |
| 4 MiB | 8 | 68 | 215.7 MiB | 3,004 | 130 | 107.9 ms | 12.2 MiB | 421.6 MiB |
| 8 MiB | 1 | 5 | 424.3 MiB | 3,031 | 59 | 6.3 ms | 8.1 MiB | 160.5 MiB |
| 8 MiB | 4 | 22 | 426.1 MiB | 3,024 | 84 | 35.0 ms | 8.1 MiB | 436.6 MiB |
| 8 MiB | 8 | 35 | 424.7 MiB | 3,025 | 75 | 99.7 ms | 16.2 MiB | 825.0 MiB |

The bounded conclusions are:

- Temporary allocation, not live heap retention, is the dominant problem. The
  process allocates roughly 53 times the large text payload per completed
  request, while the forced-GC live-heap increase stays near one or two payload
  copies rather than 53.
- Allocation amplification is linear and nearly independent of concurrency:
  about 53.6, 54.0, and 53.2 bytes allocated per payload byte at 1, 4, and
  8 MiB. Concurrency raises the amount simultaneously in flight and the RSS
  peak, but does not create the per-request amplification.
- Allocation count stays near 3,000/request as payload size grows. The growth
  therefore comes from repeated large copies, not millions of tiny objects.
- GC is doing substantial work. The 1 MiB concurrency-8 row completed 254
  requests and triggered 415 collections in about 3.1 seconds; its cumulative
  stop-the-world pause was 155 ms. The low retained heap after forced GC does
  not make that churn free.
- The earlier 806 MiB RSS observation was not a permanently live 8 MiB request
  set. This run reached 825 MiB at the same size/concurrency, but forced GC left
  only about 16 MiB more live heap than the row baseline. The runtime retained
  about 862 MiB of heap address space (`heap_sys`) and reported about 690 MiB
  released, explaining why RSS and live heap answer different questions.

## Allocation owners

Before/after cumulative-allocation profiles were compared for each row. The
concurrency-8 profiles were stable across all three payload sizes:

| Flat allocation owner | 1 MiB | 4 MiB | 8 MiB |
|---|---:|---:|---:|
| `encoding/json.(*Decoder).refill` | 30.0% | 29.7% | 30.1% |
| SDK `jsontext.(*validator).decodeString` | 27.9% | 28.0% | 28.0% |
| `io.ReadAll` | 8.0% | 8.8% | 7.8% |
| `json.RawMessage.UnmarshalJSON` | 7.4% | 7.4% | 7.5% |
| object-span parsing/validation and member deletion | ~9.4% | ~9.3% | ~9.4% |

The first two sites alone account for roughly 58% of allocated bytes. A request
body pool would address only the `io.ReadAll` slice (about 8%) and would retain
large backing arrays between requests. The next implementation experiment
should instead remove whole-string copies in strict JSON validation and avoid
the decoder refill copy where the adapter already owns a complete byte slice.
Any optimization must retain duplicate-key, invalid-UTF-8, surrogate,
number-lexeme, ordering, and provider round-trip guarantees.

## Method and limitations

- Revision: `ea4880d86907f49a5563117252963ed36b020790`
- CPU: AMD Ryzen 7 4800H, 8 cores / 16 threads
- OS: Linux 7.1.5-arch1-2, amd64
- Go: 1.26.6, `GOMAXPROCS=16`
- No plugins; OpenAI chat format; coding-agent-shaped conversation with one
  large tool result and one tool definition; zero-delay local upstream.
- Each row ran for three seconds with no uncounted warmup. One Torana process
  was reused, but forced-GC before/after counters bound each numeric interval.
- Runtime profiles are sampled, so their byte totals need not exactly equal
  `runtime.MemStats.TotalAlloc`. Percentages are attribution signals, not an
  accounting ledger.
- Forced GC changes the workload and makes these rows unsuitable as ordinary
  latency claims. Use the provider-shaped report for normal overhead.
- `heap_sys` is virtual heap address space obtained from the OS; it is not the
  same as RSS or live heap. `heap_released` is reported separately in the raw
  summary.

Reproduce with:

```bash
TORANA_BENCH_PROFILE=large \
TORANA_BENCH_REQUEST_SHAPE=agent \
TORANA_BENCH_DURATION=3s \
TORANA_BENCH_WARMUP=0s \
TORANA_BENCH_PROFILE_DIR="$PWD/large-profiles" \
  ./scripts/benchmark-production.sh large-profile.jsonl
```

The measured request rows are in
[`benchmark-large-heap-2026-08-18.jsonl`](benchmark-large-heap-2026-08-18.jsonl).
The exact runtime-counter deltas are in
[`benchmark-large-heap-2026-08-18-summary.jsonl`](benchmark-large-heap-2026-08-18-summary.jsonl).
The profiler directory additionally contains before/after `heap` and `allocs`
profiles for local call-site inspection; those reproducible binary snapshots
are not committed to the source repository.
