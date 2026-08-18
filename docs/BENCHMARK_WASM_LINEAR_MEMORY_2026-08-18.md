# WASM guest linear-memory profile — 2026-08-18

This report follows up the process-level plugin-chain evidence in
[`BENCHMARK_PLUGIN_CHAIN_RESULTS_2026-08-18.md`](./BENCHMARK_PLUGIN_CHAIN_RESULTS_2026-08-18.md).
It separates the guest's directly observable WASM linear memory from the
compiled/runtime state included in process RSS.

## Reproduction

The current-v2 SDK logger examples were built from merged SDK main commit
`692f4a0` with the release commands:

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -buildvcs=false \
  -o /tmp/torana-wasm-guests/go-logger.wasm ./examples/go-logger
cargo build --release --target wasm32-wasip1 \
  --manifest-path examples/rust-logger/Cargo.toml
```

The resulting SHA-256 digests were
`122674add245c5a7c1369655a8fb3814d772774f09bcd09617e32cccd744b90d`
for Go and
`4c0767b6d6ebab957a9f2f0cbda939e196339e722c27117874883c65419cf3c6`
for Rust.

Each guest implements the same before-request behavior: decode a typed request,
emit one granted log call, and pass the request unchanged. The input reproduces
the retained 16 KiB coding-agent shape: system/user/assistant/tool turns, a
historical 16 KiB tool result, a tool call, and a map-valued tool schema. The
committed, bundle-gated `TestGuestLinearMemoryProfile` fills a four-instance
pool, reads each instance's linear-memory size, drives exactly one real hook
call through each individual instance, and reads the size again. Go and Rust
were run in separate fresh test processes. The Linux RSS stages are
informational; the linear-memory sizes are the portable WASM contract
measurement.

`TestOfficialPluginLinearMemoryProfile` then applies the portable probe to all
nine official bundles built from plugin main commit `769e8c8`. It installs each
manifest's exact grants and drives a real before-request hook on each of four
deterministically targeted instances.

Raw machine-readable records are retained in
[`benchmark-wasm-linear-memory-2026-08-18.jsonl`](./benchmark-wasm-linear-memory-2026-08-18.jsonl).

## Result

| Guest | Bundle | Linear memory / instance after init | Growth after one call | Four-instance linear total | Fresh-process RSS after four calls |
|---|---:|---:|---:|---:|---:|
| standard Go | 9.38 MiB | 7.00 MiB | +0.50 MiB | 30.00 MiB | 126.55 MiB |
| Rust | 131.2 KiB | 1.0625 MiB | 0 | 4.25 MiB | 23.76 MiB |

The result narrows the earlier approximately 230 MiB full-proxy RSS gap:

- After the representative call, linear pages explain 25.75 MiB of the
  pool-four difference.
- The 16 KiB call grows each standard-Go instance by 512 KiB and grows no Rust
  instance. Standard Go retains some request-processing heap in its enlarged
  linear memory even though the plugin passes the request unchanged.
- In the isolated harness, standard Go retained about 45.4 MiB above the bare
  runtime after compilation/validation and grant installation, before the
  retained pool was rebuilt. Rust retained about 1.3 MiB at the same stage.
- Growing the retained Go pool from one to four instances added about 28.7 MiB
  RSS in this run, roughly 9.6 MiB per additional instance. The Rust increment
  was below one MiB and within RSS measurement noise because declared linear
  pages are not necessarily resident until touched.

All nine official plugins declare exactly 7.00 MiB after initialization. Eight
grow to 7.50 MiB after the agent-shaped call; `pii` grows to 8.50 MiB, making it
the one post-hook linear-memory outlier. Bundle sizes range from 9.41 MiB
(`otel`) to 9.72 MiB (`compactor`). The fixed standard-Go runtime/toolchain is
therefore the common initialization cost, while PII's scan path owns a measured
additional 1.00 MiB per warm instance beyond the other official plugins.

`LoadPlugin` must instantiate once to validate the hook bitmap. The subsequent
initial `SetGrants` recycles that instance because stdout/stderr wiring depends
on `env.log`; the probe therefore records load, post-grant, true one-instance
prewarm, and full-pool stages separately. This is a lifecycle fact, not yet an
optimization recommendation: changing it requires preserving initialization,
approval, logging, and hot-reload guarantees.

## Decision

Do not lower the global pool or memory ceiling based on this result. The prior
pool-two experiment materially reduced throughput, while the memory ceiling did
not materially reduce RSS. The evidence instead supports:

1. documenting Rust as the current memory-sensitive authoring path;
2. profiling PII's extra post-hook page growth without weakening its complete
   structured scan or fail-closed behavior;
3. attributing compiled-module and per-instance host allocations before testing
   idle retirement; and
4. retaining the official four-plugin workload and stream-integrity suite as
   the acceptance gate for any runtime-policy change.

This is a single-machine engineering measurement, not a universal capacity or
language-performance claim.
