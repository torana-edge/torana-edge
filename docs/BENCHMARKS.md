# Plugin pipeline benchmarks

Raw run data and the per-scenario reports live in [`benchmarks/`](../benchmarks/README.md).

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

The retained 2026-08-18 post-rewrite run and its raw rows are in
[the 2026-08-18 report](BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md). Keep older
runs as historical comparisons; do not splice their best rows into a newer
result.

For a separate CPU-saturation and payload-scaling matrix, use the built-in
profile rather than editing the provider-shaped defaults:

```bash
TORANA_BENCH_PROFILE=saturation \
  ./scripts/benchmark-production.sh saturation.jsonl
```

That profile removes the artificial upstream delay, disables streaming, and
pairs direct/Torana rows for 1, 16, and 128 KiB requests at concurrency 1, 8,
32, and 128. It measures all three local processes competing on one machine;
it is useful for finding scaling knees and payload amplification, not for a
universal capacity claim. Metadata in the JSONL records the exact profile,
upstream settings, payload sizes, and concurrency matrix. Every setting can be
overridden with `TORANA_BENCH_FIRST_BYTE`, `TORANA_BENCH_EVENT_DELAY`,
`TORANA_BENCH_EVENTS`, `TORANA_BENCH_RESPONSE_BYTES`,
`TORANA_BENCH_PAYLOAD_BYTES`, `TORANA_BENCH_NONSTREAM_CONCURRENCY`,
`TORANA_BENCH_STREAM_CONCURRENCY`, and `TORANA_BENCH_RUN_STREAM`.

`TORANA_BENCH_REQUEST_SHAPE=agent` replaces the single user message with a
coding-agent-shaped conversation: system/user turns, one historical tool call
and result, a later assistant/user turn, and a map-valued tool schema. The
configured payload size applies to the tool result. Use that shape when the
plugins under test operate on tools or conversation history; the default
`plain` shape remains byte-compatible with earlier runs.

The first retained saturation run is in
[the 2026-08-18 saturation report](../benchmarks/BENCHMARK_SATURATION_RESULTS_2026-08-18.md),
with every raw row kept alongside it.

The first retained official-plugin chain run is in
[the 2026-08-18 plugin-chain report](../benchmarks/BENCHMARK_PLUGIN_CHAIN_RESULTS_2026-08-18.md).
It uses the `agent` shape with `schema_translator`, `intent`,
`keyword_compactor`, and `otel`, and includes a five-minute stability run plus
pool-size and memory-limit comparisons.

## Plugin pipeline

`internal/plugin/bench_test.go`. Run them with:

```bash
make testdata
go test ./internal/plugin -run '^$' -bench . -benchmem
```

These isolate plugin dispatch and validation costs. Compare them with the
full HTTP path and [public plugin-chain measurements](../benchmarks/BENCHMARK_PLUGIN_CHAIN_RESULTS_2026-08-18.md)
before making an end-to-end claim.

## Near-limit request bodies

The production harness also has a separate `large` profile for whole-process
memory scaling. It runs 1, 4, and 8 MiB agent payloads at concurrency 1, 4,
and 8 with a zero-delay controlled upstream and no streaming rows:

```bash
TORANA_BENCH_PROFILE=large \
  ./scripts/benchmark-production.sh large.jsonl
```

The largest generated request remains below the configured 10 MiB request-body
limit after JSON framing. Treat RSS as whole-process evidence: one Torana
process is reused across rows, so retained heap from an earlier row can raise a
later row's starting point. Use separate invocations when an experiment needs a
fresh process per row.

The retained first run is documented in
[`BENCHMARK_LARGE_REQUEST_RESULTS_2026-08-18.md`](../benchmarks/BENCHMARK_LARGE_REQUEST_RESULTS_2026-08-18.md).

### Heap and GC attribution

Set `TORANA_BENCH_PROFILE_DIR` to compile a benchmark-only Torana binary and
capture forced-GC runtime counters plus before/after heap and cumulative-allocation
profiles around every Torana row:

```bash
TORANA_BENCH_PROFILE=large \
TORANA_BENCH_REQUEST_SHAPE=agent \
TORANA_BENCH_DURATION=3s \
TORANA_BENCH_WARMUP=0s \
TORANA_BENCH_PROFILE_DIR="$PWD/large-profiles" \
  ./scripts/benchmark-production.sh large-profile.jsonl
```

Use a zero warmup when dividing the reported allocation delta by the measured
request count; otherwise the delta correctly includes warmup work that is not
part of that count. `summary.jsonl` records cumulative allocated bytes,
allocations, GC count and pause time, and forced-GC heap state for each Torana
row. The directory also retains Go `heap` and `allocs` profiles for call-site
attribution.

The profiling HTTP surface exists only under the
`torana_benchmark_profile` build tag and accepts only a literal loopback listen
address. It is absent from release binaries; do not add a production pprof
listener merely to collect benchmark evidence.

Historical raw heap/GC records remain in `benchmarks/`. For user-facing
sizing evidence, see the [large-request report](../benchmarks/BENCHMARK_LARGE_REQUEST_RESULTS_2026-08-18.md).
Measure the current revision before applying an older optimization conclusion.
