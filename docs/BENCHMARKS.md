# Plugin pipeline benchmarks

## Full HTTP data plane

`internal/proxy/bench_test.go` measures the complete local non-streaming HTTP
path against the same in-process upstream, both directly and through Torana:

```bash
go test ./internal/proxy -run '^$' -bench BenchmarkHTTPDataPlane \
  -benchmem -benchtime=2s -count=5
```

Use paired `direct` and `torana` rows at the same concurrency. Their difference
is Torana's local request parsing, provider adaptation, routing, reverse proxy,
response parsing, usage accounting, and response rendering cost. It excludes
network/provider latency and deliberately loads no plugins; the plugin
benchmarks below isolate those costs.

Report medians and dispersion from all five samples, the exact revision,
machine, OS, Go version, GOMAXPROCS, payload, and concurrency. Do not turn this
microbenchmark into a universal “requests supported” number: production
capacity also depends on upstream latency, streaming event rate, enabled
plugins, limits, logging, and deployment resources.

“Cost” here means local CPU time and allocations. Convert CPU time to an
infrastructure estimate only with an explicitly priced deployment shape and
measured utilization; Torana itself does not add per-token model charges.

## Production-shaped process benchmark

The microbenchmark above deliberately removes provider latency. Before making
a release or capacity statement, run the separate-process harness instead:

```bash
./scripts/benchmark-production.sh results.jsonl
```

It builds the real `torana` binary, starts it as its own process with a separate
OpenAI-compatible upstream, and drives both the direct and proxied paths from
an external concurrent client. The controlled upstream waits 100 ms before
responding; its streaming response then emits 100 text events 10 ms apart. The
matrix covers non-streaming concurrency 1, 8 and 32 and streaming concurrency
1 and 8. Every stream must retain at least 100 data events and terminate with
`[DONE]`. Output is JSON Lines containing the revision and runtime metadata,
request count, errors, throughput, p50/p95/p99/max latency, stream event count,
and Torana process CPU time plus start/peak RSS. Direct rows sample the idle
Torana process, which makes their CPU field an explicit measurement floor;
proxied rows record the CPU consumed while Torana handled that row.

The defaults run each measured row for 10 seconds after a 2-second warmup.
Override them without editing the harness:

```bash
TORANA_BENCH_DURATION=30s TORANA_BENCH_WARMUP=5s \
  TORANA_BENCH_PORT=28080 TORANA_BENCH_UPSTREAM_PORT=28081 \
  ./scripts/benchmark-production.sh results.jsonl
```

This harness still uses a controlled loopback provider. Its latency and event
cadence are production-shaped, not production evidence. Run on an otherwise
idle Linux machine (`/proc` supplies RSS), retain every raw JSONL row, and state
the machine and configuration beside any summary. Zero request errors and the
stream-integrity checks are validity requirements, not performance results.

The current post-rewrite run and its raw rows are in
[the 2026-08-18 report](BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md). Keep older
runs as historical comparisons; do not splice their best rows into a newer
result.

## Plugin pipeline

`internal/plugin/bench_test.go`. Run them with:

```bash
make testdata
go test ./internal/plugin -run '^$' -bench . -benchmem
```

These exist to answer design questions with numbers. The first one they were
built to answer is recorded below.

## Should the host verify what each plugin changed?

**Yes, using exact structural comparison. It costs 2.6–10% of pipeline time,
worst case 1.5 ms absolute, which is well under 0.1% of end-to-end request
latency.**

Today `RunBeforeRequest` converts once, marshals once, then chains raw bytes
from plugin to plugin without looking inside (`discovery.go:942-967`). Enforcing
write grants means, per plugin, decoding that plugin's output and establishing
which grantable sections it changed.

**This is enforcement, and the plugin author is the threat model.** A method that
merely usually notices a change is not a candidate. Two safe methods are
measured, in `writegrant_prototype_test.go`. Coverage is not asserted by a
hand-written mutation list — that only demonstrates the cases someone thought
of. `TestEveryProtoFieldHasAGrantSection` walks the protobuf descriptor and
fails if any field belongs to no grant section, and
`TestEveryGovernedFieldIsDetected` mutates every governed field through
reflection — on `ChatRequest` **and** on the nested `Message`, `ToolCall` and
`ToolDef` — requiring both methods to notice. Adding a field to the contract in
v2 without deciding which grant governs it, or assigning one a section but
forgetting it in the fingerprint, will fail the suite.

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

> An earlier revision priced a cheaper prototype that folded a per-role
> accumulator over unframed field concatenation. It was **not** enforcement-safe:
> it missed a cross-role reorder entirely, and collided when a field boundary
> moved. A later revision was still incomplete — it ignored
> `provider_extensions_json`, `safety_settings_json`, `torana_meta_json` and
> `ToolDef.strict`, compared floats with `==` (so `-0.0` and `+0.0` were
> identical), and framed optional fields without their identity, letting
> `{max_tokens: 0, temperature: nil}` and `{max_tokens: nil, temperature: 0}`
> collide. All are now regression tests, and the reflection inventory exists so
> the next omission fails a test rather than shipping.

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

Each measurement plays a **complete request** — whatever a plugin opens it gets
to close — and ends it with `EndRequest`. Stream plugins keep request-scoped
state, so a benchmark that reuses one request ID forever measures unbounded
buffer growth, and one that invents a fresh ID per event without ending it leaks
a buffer per event. Neither is a per-event cost.

| plugins | sequence | per event | allocations |
|--------:|---|---:|---:|
| 1 | text delta | 58 µs | 37 KB |
| 1 | tool call (start, 2 deltas, end) | 67 µs | 212 KB |
| 2 | text delta | 128 µs | 73 KB |
| 2 | tool call | 213 µs | 473 KB |

Two plugins is **not** double one: 2.2× for text deltas and 3.2× for tool calls,
because the second fixture buffers fragments and does real work per event.

Sustained, one plugin, text deltas only — `BenchmarkStreamedResponse`:

| events | time | allocations |
|---:|---:|---:|
| 100 | 4.4 ms | 3.7 MB |
| 1000 | 45.9 ms | **36.7 MB** |

That works out to 45 µs/event sustained against 58 µs per single dispatch. The
first runs are measurably slower than the steady state, so the benchmarks warm
before timing — but **the cause of that gap is not established here.** It is not
instantiation: the pipeline creates and pools an instance while loading the
plugin, before any timing starts. An earlier revision of this document asserted
otherwise, and asserted **77 µs** on the strength of an unwarmed run.

Where the two disagree, the sustained figure is the one to use.

**Latency is fine; allocation is not.** 58 µs added to time-to-next-token is
imperceptible. But 37 KB allocated to process a six-byte text delta is a ~6000×
amplification, and ~34,000 allocations per thousand events is real GC pressure —
which shows up under concurrency, and every benchmark here is serial.

Two design consequences:

1. **Do not extend write-grant verification to the stream path** without
   measuring it there. On the request path it costs 2.6–10%; on the stream path
   the same work is multiplied by the event count.
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
