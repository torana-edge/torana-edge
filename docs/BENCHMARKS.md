# Plugin pipeline benchmarks

`internal/plugin/bench_test.go`. Run them with:

```bash
make testdata
go test ./internal/plugin -run '^$' -bench . -benchmem
```

These exist to answer design questions with numbers. The first one they were
built to answer is recorded below.

## Should the host verify what each plugin changed?

**Yes. It costs 2–4% of pipeline time, which is ~0.03% of end-to-end request
latency.**

Today `RunBeforeRequest` converts once, marshals once, then chains raw bytes
from plugin to plugin without looking inside (`discovery.go:942-967`). Enforcing
write grants means unmarshalling each plugin's output so the host can
fingerprint its sections and compare them against the previously accepted
request — so the added cost is one `proto.Unmarshal` per plugin.

Measured on an AMD Ryzen 7 4800H, Go 1.26, `-benchtime=100x` (pipeline) and
`2000x` (serialization):

| plugins | msgs | pipeline | verification | overhead |
|--------:|-----:|---------:|-------------:|---------:|
| 1 | 1 | 0.33 ms | 0.006 ms | 1.7% |
| 1 | 100 | 4.30 ms | 0.132 ms | 3.1% |
| 3 | 20 | 2.99 ms | 0.100 ms | 3.4% |
| 3 | 100 | 8.96 ms | 0.396 ms | 4.4% |
| 5 | 20 | 4.93 ms | 0.167 ms | 3.4% |
| 5 | 100 | 16.67 ms | 0.660 ms | 4.0% |

Worst case is 0.66 ms of verification on a request whose upstream LLM call takes
2–30 seconds. Fully-granted plugins skip the check entirely via the fast path,
so most pipelines pay less than this.

## The WASM boundary dominates everything else

The more useful finding, and the one that should shape future optimisation work:

| | 1 msg | 100 msgs |
|---|---|---|
| Marginal cost per extra plugin | ~230 µs | ~3.1 ms |
| Host-side serialization for that plugin | ~8 µs | ~200 µs |

Each plugin crossing costs roughly 230 µs even for a single-message request that
does almost nothing — that is instantiation, memory copy and guest-side decode,
not encoding. Host-side serialization is 3–6% of the marginal cost of adding a
plugin.

Two consequences:

1. **Optimising host-side encoding is not worth doing.** `pbconv` is the most
   expensive host-side step (it runs `json.Marshal` per message, per tool call
   and per tool — `pbconv.go:33-84`), and at 100 messages it is 69 µs against a
   16.7 ms pipeline.
2. **Plugin count is the thing that costs.** Five plugins are ~4× one plugin.
   Anything that reduces crossings — merging hooks, skipping plugins that
   declare no interest in a request — is worth far more than encoding work.

## What the numbers do not cover

- Streaming. `RunOnStreamChunk` is called per SSE event, so its per-call cost
  matters far more than the request path's; it is not benchmarked yet.
- Host calls made from inside a hook (`torana_offload_completion` reaches a
  local model and takes hundreds of milliseconds). Real plugin work dwarfs
  everything measured here.
- Concurrency. The pool is 4 (`wasm.Runtime`), so a sixth concurrent request
  waits. `BenchmarkRunBeforeRequest` is serial and says nothing about that.
