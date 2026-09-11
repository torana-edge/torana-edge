# Benchmark archive

Raw measurement data and the per-scenario reports it backs. This is evidence,
not documentation — it is kept so a published number can be traced to the run
that produced it, and so a later run can be compared against an earlier one.

Nothing here is written for a first-time reader. Two files are, and they stayed
in `docs/`:

- [How to run the benchmarks](../docs/BENCHMARKS.md) — the harness, the
  scenarios, and how to read the output.
- [Production results, 2026-08-18](../docs/BENCHMARK_PRODUCTION_RESULTS_2026-08-18.md)
  — the current headline result, the one the README links.

## What is here

| Kind | Files |
|---|---|
| Raw runs | `benchmark-*.jsonl` — one JSON object per request |
| Run summaries | `benchmark-*-summary.jsonl` — cumulative counters for a run |
| Scenario reports | `BENCHMARK_*_RESULTS_*.md`, `BENCHMARK_WASM_*.md` |

Every file is dated in its name. A report and its data share the same date and
scenario, so `BENCHMARK_SATURATION_RESULTS_2026-08-18.md` reads
`benchmark-saturation-2026-08-18.jsonl`.

Superseded runs are kept rather than deleted: `BENCHMARK_PRODUCTION_RESULTS_2026-08-08.md`
is what the 08-18 production run is an improvement over, and deleting it would
leave the improvement unverifiable.

New runs belong here, not in `docs/`. `docs/` held 31 raw `.jsonl` files and 13
dated reports against 13 actual user documents, which made the documentation
directory mostly an archive that happened to also contain the quickstart.
