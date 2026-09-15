# Performance evidence

These reports describe measured revisions and workloads, not a guarantee for
today's build or your machine. Start with proxy-only overhead, then include the
plugins and payload sizes you actually use.

| Report | What it tells you |
| --- | --- |
| [Proxy-only process benchmark](../docs/BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md) | Direct vs Torana latency, CPU and RSS with a controlled provider; no plugins |
| [Official plugin chain](BENCHMARK_PLUGIN_CHAIN_RESULTS_2026-08-18.md) | End-to-end cost with four plugins, including memory and stability |
| [Saturation](BENCHMARK_SATURATION_RESULTS_2026-08-18.md) | Payload and concurrency scaling on one machine |
| [Large requests](BENCHMARK_LARGE_REQUEST_RESULTS_2026-08-18.md) | Near-limit request memory costs |
| [WASM memory](BENCHMARK_WASM_LINEAR_MEMORY_2026-08-18.md) | Go/Rust guest footprint, repeated-call growth and host overhead |

All reports are dated 18 August 2026 and name their source revisions.
The plugin-chain result is an important counterbalance to the proxy-only
headline; do not quote one as if it measured the other.

[Run the benchmarks](../docs/BENCHMARKS.md) to evaluate another revision.
Raw `benchmark-*.jsonl` records, including older baseline and optimization
runs, remain public here so comparisons can be reproduced. Some historical
reports are available in Git history at their original revision; raw evidence
has not been removed.

Retain every row, report errors and stream-integrity results, and distinguish
linear memory, live heap and process RSS. Do not select the best rows from
different runs and present them as one measurement.
