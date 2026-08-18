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

## Measured follow-up: validated span projection

The two identified owners were then changed without weakening those guarantees:

- SDK JSON-text validation stopped retaining decoded bytes for ordinary value
  strings; decoded bytes remain mandatory for object keys, where they enforce
  escape-equivalent duplicate detection.
- Edge stopped decoding a validated raw object merely to learn its top-level
  shape, and provider-extension projection now discovers member spans once and
  removes canonical fields in one pass. Surviving key/value lexemes and order
  remain exact; an independent decoder/reference corpus covers nested values,
  escaped delimiters, large numbers, and every removal suffix.

The exact same 18-row command was repeated on production revision
`974ac35077668c7bc8fced0d92d82777c79879a8`. All rows again completed with zero
errors:

| Tool result | Concurrency | Allocated/request before | After | Reduction | Throughput before | After | Peak RSS before | After |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 MiB | 1 | 53.7 MiB | 14.6 MiB | 72.9% | 7.0 req/s | 13.0 req/s | 50.5 MiB | 39.4 MiB |
| 1 MiB | 4 | 53.7 MiB | 14.4 MiB | 73.2% | 34.2 req/s | 66.9 req/s | 80.8 MiB | 63.1 MiB |
| 1 MiB | 8 | 53.6 MiB | 14.4 MiB | 73.1% | 82.0 req/s | 168.4 req/s | 134.8 MiB | 96.7 MiB |
| 4 MiB | 1 | 216.4 MiB | 59.4 MiB | 72.6% | 2.6 req/s | 4.9 req/s | 98.5 MiB | 79.4 MiB |
| 4 MiB | 4 | 215.9 MiB | 58.8 MiB | 72.8% | 10.6 req/s | 21.5 req/s | 219.5 MiB | 162.6 MiB |
| 4 MiB | 8 | 215.7 MiB | 58.8 MiB | 72.7% | 20.5 req/s | 42.1 req/s | 421.6 MiB | 294.4 MiB |
| 8 MiB | 1 | 424.3 MiB | 113.3 MiB | 73.3% | 1.6 req/s | 3.1 req/s | 160.5 MiB | 112.5 MiB |
| 8 MiB | 4 | 426.1 MiB | 114.6 MiB | 73.1% | 6.1 req/s | 12.5 req/s | 436.6 MiB | 288.3 MiB |
| 8 MiB | 8 | 424.7 MiB | 113.3 MiB | 73.3% | 9.8 req/s | 22.0 req/s | 825.0 MiB | 547.0 MiB |

At concurrency 8, median latency fell by 50.6–52.5%, throughput increased
2.05–2.25 times, and peak RSS fell by 28.3–33.7%. Allocation amplification is
now about 14.2–14.6 bytes per payload byte instead of about 53. The forced-GC
live heap remains small, so the residual is still temporary-copy/GC work rather
than evidence of a growing retained heap.

The follow-up raw rows are in
[`benchmark-large-heap-after-json-optimization-2026-08-18.jsonl`](benchmark-large-heap-after-json-optimization-2026-08-18.jsonl),
with exact runtime deltas in
[`benchmark-large-heap-after-json-optimization-2026-08-18-summary.jsonl`](benchmark-large-heap-after-json-optimization-2026-08-18-summary.jsonl).
This closes the two dominant profile owners, but not issue #199: the next
profile should identify the remaining roughly 14x amplification before another
implementation choice is made.

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
