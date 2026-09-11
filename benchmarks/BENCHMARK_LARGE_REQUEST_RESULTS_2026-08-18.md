# Near-limit request-body benchmark — 2026-08-18

This run measures whole-process scaling as coding-agent-shaped request bodies
approach Torana's configured 10 MiB limit. It is evidence for issue #199, not a
universal capacity claim and not a plugin-runtime benchmark.

## Result

All 18 measured direct/Torana rows completed with zero errors. The Torana rows
were:

| Tool-result payload | Concurrency | Requests/s | p50 | p99 | RSS at row start | Peak RSS | Within-row RSS increase |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 MiB | 1 | 6.51 | 158.8 ms | 180.8 ms | 40.1 MiB | 42.6 MiB | 2.6 MiB |
| 1 MiB | 4 | 29.70 | 133.5 ms | 187.2 ms | 57.4 MiB | 79.9 MiB | 22.5 MiB |
| 1 MiB | 8 | 81.84 | 96.7 ms | 118.7 ms | 75.4 MiB | 125.3 MiB | 49.9 MiB |
| 4 MiB | 1 | 2.39 | 424.0 ms | 477.8 ms | 82.0 MiB | 99.6 MiB | 17.6 MiB |
| 4 MiB | 4 | 10.81 | 360.9 ms | 448.9 ms | 127.6 MiB | 218.1 MiB | 90.5 MiB |
| 4 MiB | 8 | 20.77 | 373.5 ms | 455.4 ms | 226.5 MiB | 401.6 MiB | 175.1 MiB |
| 8 MiB | 1 | 1.59 | 607.6 ms | 709.5 ms | 146.8 MiB | 175.6 MiB | 28.8 MiB |
| 8 MiB | 4 | 6.06 | 616.4 ms | 707.0 ms | 288.8 MiB | 443.3 MiB | 154.5 MiB |
| 8 MiB | 8 | 10.50 | 722.7 ms | 829.1 ms | 474.6 MiB | 806.2 MiB | 331.6 MiB |

The bounded conclusions are:

- Torana correctly handled the full matrix below its body limit without an
  error, including eight concurrent 8 MiB conversations.
- Memory grows materially with both body size and concurrency. The 8 MiB,
  concurrency-8 row added about 332 MiB during that row and reached about
  806 MiB process RSS. This keeps #199 open and makes near-limit concurrent
  conversations an explicit sizing concern.
- Latency is CPU-bound here because the controlled upstream has zero delay.
  These rows are deliberately unlike ordinary provider traffic and should not
  replace the 100 ms provider-shaped benchmark when describing normal overhead.
- Direct rows are load-generator controls. RSS in every row samples the Torana
  process, so RSS attached to a direct row is merely Torana's idle retained
  state and is not a direct-server memory measurement.

## Method and limitations

- Revision: `d0917b7c2ed0bc02c90293efa18eaf338b77caeb`
- CPU: AMD Ryzen 7 4800H, 8 cores / 16 threads
- OS: Linux 7.1.5-arch1-2, amd64
- Go: 1.26.6, `GOMAXPROCS=16`
- One Torana process, one controlled upstream process, and one external load
  generator shared the machine.
- No plugins were loaded. The request shape contains system/user/assistant
  messages, one tool call, one large tool result, and one tool definition.
- The upstream added no delay and returned a 4 KiB response.
- Each row ran for five seconds after a one-second warmup.
- One Torana process was reused across rows. Start RSS therefore includes heap
  retained by preceding rows; peak RSS is intentionally a saturation signal,
  not a per-request allocation decomposition. A follow-up heap/GC profile is
  still required before choosing pooling or incremental decoding.

Reproduce with:

```bash
TORANA_BENCH_PROFILE=large \
TORANA_BENCH_REQUEST_SHAPE=agent \
TORANA_BENCH_DURATION=5s \
TORANA_BENCH_WARMUP=1s \
  ./scripts/benchmark-production.sh large.jsonl
```

The complete machine-readable rows are retained in
[`benchmark-large-request-2026-08-18.jsonl`](benchmark-large-request-2026-08-18.jsonl).
