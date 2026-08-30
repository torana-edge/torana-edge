# WASM burst-instance idle retirement — 2026-08-18

This experiment applies the ownership evidence from
[`BENCHMARK_WASM_HOST_MEMORY_2026-08-18.md`](./BENCHMARK_WASM_HOST_MEMORY_2026-08-18.md)
without lowering Torana's concurrency ceiling.

## Policy

- The pool remains a hard maximum of four concurrent instances per plugin.
- One ready instance is always retained.
- Instances created for a concurrent burst are retired after at least one
  quiet minute (the sweep runs every half-timeout).
- New bursts regrow the pool from the already-compiled module.
- Operators can choose another timeout or explicitly disable retirement.

Grant installation, idle sweeping, calls, unload, and close share the existing
call-admission boundary. An instance built under an old grant/logging policy
cannot be requeued after a grant update. The sweeper examines a bounded channel
snapshot, so continuous traffic cannot make one sweep run indefinitely.

## Measurement

The current standard-Go logger from the preceding reports ran in a
four-instance pool. Each exact instance received the same 16 KiB coding-agent
request. A 50 ms timeout accelerated the policy transition for the test; the
production default is 60 seconds and executes the identical retirement path.
The three-instance regrowth was driven concurrently.

| Measurement | Result |
|---|---:|
| linear memory before idle | 30.0 MiB |
| linear memory after retirement | 7.5 MiB |
| exact linear memory released | 22.5 MiB |
| retained RSS reduction, three runs | 34.7–35.0 MiB |
| concurrent three-instance regrowth, three runs | 46.4–51.5 ms |
| median concurrent regrowth | 47.3 ms |

The retained instance serves the first request immediately. The measured
regrowth cost applies only to simultaneous requests beyond that instance after
the quiet timeout. Instantiation was previously serialized across the whole
operation and took about 89 ms for the same three instances; narrowing the
unique-name lock lets wazero instantiate them concurrently.

Raw records are retained in
[`benchmark-wasm-idle-retirement-2026-08-18.jsonl`](./benchmark-wasm-idle-retirement-2026-08-18.jsonl).

## Safety and decision

Deterministic and race-enabled tests pin all of these boundaries:

- all-stale pools retain exactly the newest instance;
- recent and active instances are not retired;
- disabled and closed plugins are unchanged;
- a retired pool regrows to its full concurrency ceiling on demand;
- grant installation quiesces calls and sweeping before draining old-policy
  instances;
- concurrent traffic plus millisecond sweeps produces no call failures or data
  races; and
- runtime close stops the worker before releasing plugin resources.

This changes idle retention, not capability enforcement, request semantics, or
the concurrency ceiling. The full plugin/proxy and stream-integrity gates remain
required before merge. The numbers are single-host engineering evidence, not a
universal latency or capacity promise.
