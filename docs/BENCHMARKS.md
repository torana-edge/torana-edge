# Plugin pipeline benchmarks

`internal/plugin/bench_test.go`. Run them with:

```bash
make testdata
go test ./internal/plugin -run '^$' -bench . -benchmem
```

These exist to answer design questions with numbers. The first one they were
built to answer is recorded below.

## Should the host verify what each plugin changed?

**Yes. It costs 2–10% of pipeline time, worst case 1.6 ms absolute, which is
well under 0.1% of end-to-end request latency.**

Today `RunBeforeRequest` converts once, marshals once, then chains raw bytes
from plugin to plugin without looking inside (`discovery.go:942-967`). Enforcing
write grants means, per plugin, unmarshalling that plugin's output and
fingerprinting each grantable section so it can be compared against the
previously accepted request.

`BenchmarkWriteGrantVerification` measures **both halves** — the decode and the
fingerprinting — against a prototype verifier. That matters: the decode alone is
under half the cost.

| | msgs=1 | msgs=20 | msgs=100 |
|---|---:|---:|---:|
| `proto.Unmarshal` only | 2.9 µs | 26 µs | 134 µs |
| **decode + fingerprint** | **10.5 µs** | **56 µs** | **311 µs** |

Measured on an AMD Ryzen 7 4800H, Go 1.26, `-benchtime=100x` (pipeline) and
`2000x` (serialization):

| plugins | msgs | pipeline | verification | overhead |
|--------:|-----:|---------:|-------------:|---------:|
| 1 | 1 | 0.44 ms | 0.010 ms | 2.4% |
| 1 | 100 | 4.33 ms | 0.311 ms | 7.2% |
| 3 | 20 | 2.84 ms | 0.169 ms | 5.9% |
| 3 | 100 | 8.98 ms | 0.934 ms | 10.4% |
| 5 | 20 | 5.02 ms | 0.281 ms | 5.6% |
| 5 | 100 | 16.55 ms | 1.557 ms | 9.4% |

Worst case is 1.6 ms on a request whose upstream call takes 2–30 seconds.
Fully-granted plugins skip the check via the fast path, so most pipelines pay
less.

The fingerprint prototype uses FNV-1a, the cheapest credible choice, so these
are a lower bound on the hashing. A stronger hash would move the second row of
the first table, not the conclusion.

## The WASM boundary dominates everything else

`BenchmarkBoundaryCrossing` uses fixtures that do nothing but return
pass-through, so the delta between 1, 2 and 3 plugins is crossing cost with no
guest work mixed in. The mixed pipeline cannot give this — its fixtures scan
messages, emit metrics and make cache calls, so its plugin-count delta measures
crossings *and* guest work together.

| | 1 plugin | 2 plugins | 3 plugins | marginal |
|---|---:|---:|---:|---:|
| msgs=100 | 2.40 ms | 4.65 ms | 7.27 ms | **~2.4 ms per plugin** |
| msgs=1 | 179 µs | 243 µs | 486 µs | did not stabilise |

The 100-message series is clean and linear. The single-message series did not
converge across runs (deltas ranged from 60 µs to 240 µs), so **no per-crossing
figure is claimed for small payloads** — it is somewhere in that range, and the
benchmark as written cannot narrow it.

What does hold: at 100 messages a crossing costs ~2.4 ms, while the entire
host-side conversion for that request — `pbconv` plus `proto.Marshal`, run once
— is ~137 µs. Two consequences:

1. **Optimising host-side encoding is not worth doing.** `pbconv` is the most
   expensive host-side step (`json.Marshal` per message, per tool call and per
   tool — `pbconv.go:33-84`), and at 100 messages it is 69 µs against a 16.5 ms
   five-plugin pipeline: 0.4%.
2. **Plugin count is what costs.** Anything that reduces crossings — skipping
   plugins with no interest in a request, short-circuiting on a block verdict —
   is worth far more than encoding work.

## Streaming is where the cost actually lives

`run_on_stream_chunk` fires **once per SSE event**, so a per-event cost
multiplies by three orders of magnitude across a response. The request hook is
paid once; this one is paid a thousand times.

With a single stream plugin loaded:

| | per event | per 100-token response | per 1000-token response |
|---|---:|---:|---:|
| time | 77 µs | 4.2 ms | 44 ms |
| allocations | 37 KB | 3.7 MB | **37 MB** |

Two of the nine official plugins declare `run_on_stream_chunk` (`intent`,
`schema_translator`), so a realistic pipeline roughly doubles both columns.

**Latency is fine; allocation is not.** 77 µs added to time-to-next-token is
imperceptible — the whole 44 ms is spread across a stream that takes ten to
twenty seconds to arrive. But 37 KB allocated to process a six-byte text delta
is a ~6000× amplification, and 33,000 allocations per streamed response is real
GC pressure that shows up under concurrency, not in this serial benchmark.

Two design consequences:

1. **Do not extend write-grant verification to the stream path** without
   measuring it there first. On the request path an extra unmarshal per plugin
   costs 2–4%; on the stream path the same work is multiplied by the event
   count.
2. **The per-event allocation is the optimisation worth doing**, if any is. It
   is `pbconv` → `proto.Marshal` → guest memory copy → result copy, per event,
   per plugin.

## Total added latency, in units a user feels

For one realistic coding-agent turn — a 20-message conversation, three request
plugins, a 1000-token streamed reply, two stream plugins:

| | |
|---|---|
| request hooks | ~3 ms, once |
| stream hooks | ~88 ms, spread across the stream |
| **total CPU added by the plugin pipeline** | **~91 ms** |
| against an upstream call of | 10–20 s |
| **share of end-to-end** | **~0.5–0.9%** |

Write-grant verification would add ~0.2 ms to that (three plugins, 20 messages),
which does not move the figure.

This is the plugin pipeline only. It excludes format adapter parsing, the HTTP
proxy itself, and response-side hooks, none of which are benchmarked yet.

## What the numbers still do not cover

- Format adapters, the HTTP proxy layer, and `run_after_response`.
- Host calls made from inside a hook. `torana_offload_completion` reaches a
  local model and takes hundreds of milliseconds — real plugin work dwarfs
  everything measured here.
- Concurrency. The pool is 4 (`wasm.Runtime`), so a fifth concurrent request
  waits on a slot. Every benchmark here is serial and says nothing about that,
  which matters most for the allocation figures above.
