# WASM guest host-memory attribution — 2026-08-18

This report follows the direct linear-memory and repeated-call profiles. It
separates guest linear memory, live Go-host heap, reserved host heap, and
process RSS at each plugin lifecycle boundary.

## Method

The Go and Rust guests, exact source revision, artifact digests, pool size, and
16 KiB coding-agent request are the same as
[`BENCHMARK_WASM_LINEAR_MEMORY_2026-08-18.md`](./BENCHMARK_WASM_LINEAR_MEMORY_2026-08-18.md).
Each guest runs in a fresh process. The test forces host GC and returns unused
host pages before reading Linux RSS and `runtime.MemStats` at these boundaries:

1. baseline after reading the guest artifact;
2. empty wazero runtime;
3. compiled module plus the validation/prewarm instance;
4. grants installed and that instance discarded, leaving the compiled module;
5. one newly prewarmed instance;
6. four acquired instances; and
7. one real request through each instance.

The artifact bytes are read before the baseline, so the 9.38 MiB Go file is
not misattributed to compilation. `HeapAlloc` includes linear-memory backing
allocated by wazero. RSS remains a process-level Linux observation and may not
rise until declared pages are touched.

Raw stage records are retained in
[`benchmark-wasm-host-memory-2026-08-18.jsonl`](./benchmark-wasm-host-memory-2026-08-18.jsonl).

## Result

| Attribution | standard Go | Rust |
|---|---:|---:|
| compiled module, RSS delta (after-grant − runtime) | +45.1 MiB | +1.64 MiB |
| compiled module, live-heap delta | +6.78 MiB | +0.06 MiB |
| compiled/runtime RSS not explained by live heap | ~38.3 MiB | ~1.58 MiB |
| first retained instance, live-heap delta | +9.72 MiB | +1.07 MiB |
| each of next three instances, live-heap delta | +9.72 MiB | +1.07 MiB |
| each of next three instances, RSS delta | +10.38 MiB | below 0.04 MiB |
| four real calls, live-heap delta | +7.29 MiB | +0.16 MiB |
| four real calls, linear-memory delta | +2.00 MiB | 0 |

The Go instance's 7 MiB initial linear memory explains most, but not all, of
its 9.72 MiB live-host-heap increment. Roughly 2.7 MiB per instance belongs to
the remaining wazero instance/runtime representation at this boundary. The
compiled Go module is the other large fixed owner: most of its ~45 MiB RSS
increment is not live Go heap and therefore cannot be recovered by ordinary
host GC while the compiled handle remains loaded.

Rust's additional declared pages are initially mostly non-resident: live heap
tracks the 1.0625 MiB linear allocation per instance, while RSS barely moves as
the pool grows. After real calls it still does not grow linear memory.

The Go call-stage delta includes 2 MiB of measured linear growth plus retained
per-instance execution state. It should not be labelled a leak: the independent
10,000-call probe shows the guest reaches 12 MiB per instance by 1,000 calls and
then remains flat.

## Decision

The evidence narrows the viable improvements:

- Recompilation is already avoided inside a loaded plugin; the fixed compiled
  handle is expensive but shared by all instances.
- Lowering the memory limit does not remove the standard-Go runtime's committed
  initial pages and previously produced no material RSS improvement.
- Reducing the global pool trades away measured throughput. If idle retirement
  is pursued, it must retain a ready instance, retire only burst-created idle
  instances after evidence-backed quiescence, and pass the existing throughput,
  stream-integrity, approval, hot-reload, and lifecycle suites.
- Rust remains the practical memory-sensitive authoring path today.

These numbers are attribution on one Linux host, not universal deployment
capacity claims.
