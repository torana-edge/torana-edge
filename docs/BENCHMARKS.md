# Plugin pipeline benchmarks

`internal/plugin/bench_test.go`. Run them with:

```bash
make testdata
go test ./internal/plugin -run '^$' -bench . -benchmem
```

These exist to answer design questions with numbers. The first one they were
built to answer is recorded below.

## Should the host verify what each plugin changed?

**Yes, using exact structural comparison. It costs 2–9% of pipeline time, worst
case 1.5 ms absolute, which is well under 0.1% of end-to-end request latency.**

Today `RunBeforeRequest` converts once, marshals once, then chains raw bytes
from plugin to plugin without looking inside (`discovery.go:942-967`). Enforcing
write grants means, per plugin, decoding that plugin's output and establishing
which grantable sections it changed.

**This is enforcement, and the plugin author is the threat model.** A method that
merely usually notices a change is not a candidate. Two safe methods are
measured, in `writegrant_prototype_test.go`, each with a mutation suite proving
it detects every change a plugin could make — cross-role reorder, same-role
edit, field-boundary shift, insertion, deletion, tool-call rewrite, schema
rewrite, model swap, parameter change:

- **exact** — structural comparison against the previously accepted request.
  Cannot collide, because it never summarises.
- **fingerprint** — SHA-256 over length-framed fields with the message index
  folded in. Collision-resistant and reorder-sensitive.

| | msgs=1 | msgs=20 | msgs=100 |
|---|---:|---:|---:|
| `proto.Unmarshal` only | 2.9 µs | 25 µs | 121 µs |
| **exact comparison** | **11.7 µs** | **69 µs** | **299 µs** |
| safe fingerprint | 11.1 µs | 78 µs | 376 µs |

**Exact comparison wins on both axes** — safer *and* 20% cheaper at scale, with
half the allocations. The trade it makes is memory: it needs the accepted
request kept decoded, where fingerprints carry 32 bytes per section. At these
sizes that is not worth paying anything for.

Measured on an AMD Ryzen 7 4800H, Go 1.26, `-benchtime=100x` (pipeline) and
`2000x` (serialization), using exact comparison:

| plugins | msgs | pipeline | verification | overhead |
|--------:|-----:|---------:|-------------:|---------:|
| 1 | 1 | 0.44 ms | 0.012 ms | 2.6% |
| 1 | 100 | 4.33 ms | 0.299 ms | 6.9% |
| 3 | 20 | 2.84 ms | 0.206 ms | 7.2% |
| 3 | 100 | 8.98 ms | 0.896 ms | 10.0% |
| 5 | 20 | 5.02 ms | 0.343 ms | 6.8% |
| 5 | 100 | 16.55 ms | 1.494 ms | 9.0% |

Worst case is 1.5 ms on a request whose upstream call takes 2–30 seconds.
Fully-granted plugins skip the check via the fast path, so most pipelines pay
less.

> An earlier revision of this document priced a cheaper prototype that folded a
> per-role accumulator over unframed field concatenation. It was **not**
> enforcement-safe: it missed a cross-role reorder entirely, and collided when a
> field boundary moved. Both are now regression tests. The lesson is that a
> verification benchmark has to price a method that actually verifies.

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

`run_on_stream_chunk` fires **once per SSE event**, so its cost is multiplied by
the event count while the request hook is paid once.

Everything below is **per event**. How many events a provider emits per token is
provider behaviour these benchmarks do not measure — providers coalesce deltas,
and content-block and message boundaries add events of their own — so no
token-to-event ratio is assumed anywhere. Convert with your own traffic in hand.

| plugins | event | time | allocations |
|--------:|---|---:|---:|
| 1 | text delta | 60 µs | 37 KB |
| 1 | tool-call delta | 76 µs | 59 KB |
| 2 | text delta | 113 µs | 73 KB |
| 2 | tool-call delta | 296 µs | 131 KB |

Two plugins is **not** double one: 1.9× for text deltas and **3.9×** for
tool-call deltas, because the second fixture buffers fragments and does real
work per event.

Sustained, one plugin, text deltas only — `BenchmarkStreamedResponse`:

| events | time | allocations |
|---:|---:|---:|
| 100 | 4.4 ms | 3.7 MB |
| 1000 | 45.9 ms | **36.7 MB** |

That works out to 46 µs/event sustained against 60 µs measured per single
dispatch. The gap is warm-up: the first crossings pay instantiation, and a short
benchmark amortises that over too few operations. An earlier revision of this
document reported **77 µs** for exactly that reason. The benchmark now warms the
pool before timing, and the sustained figure is the one to trust.

**Latency is fine; allocation is not.** 60 µs added to time-to-next-token is
imperceptible. But 37 KB allocated to process a six-byte text delta is a ~6000×
amplification, and ~34,000 allocations per thousand events is real GC pressure —
which shows up under concurrency, and every benchmark here is serial.

Two design consequences:

1. **Do not extend write-grant verification to the stream path** without
   measuring it there. On the request path it costs 2–9%; on the stream path the
   same work is multiplied by the event count.
2. **The per-event allocation is the optimisation worth doing**, if any is. It is
   `pbconv` → `proto.Marshal` → guest memory copy → result copy, per event, per
   plugin.

## Total added latency

For one coding-agent turn — a 20-message conversation, three request plugins,
one stream plugin, and a response of 1000 SSE events:

| | |
|---|---|
| request hooks | ~2.8 ms, once |
| write-grant verification | ~0.2 ms, once |
| stream hooks | ~46 ms, spread across the stream |
| **total CPU added by the plugin pipeline** | **~49 ms** |
| against an upstream call of | 10–20 s |
| **share of end-to-end** | **~0.2–0.5%** |

Scale the stream row by your own event count and stream-plugin count; the table
above shows those do not combine linearly.

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
