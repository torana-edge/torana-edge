# Issue 166 DeepSeek dogfood results

On 21 July 2026, Torana ran a paired OMP dogfood to test whether safe
tool-output compaction reduces provider-reported input tokens and actual API
cost. This is a workload result, not a general savings claim.

## Method

- Provider: DeepSeek V4 Pro through `https://api.deepseek.com/beta`.
- Offload candidate: DeepSeek V4 Flash through the same beta endpoint.
- Matrix: five repository tasks, five repetitions, and three randomized arms
  per repetition: control, deterministic, and model-gated (75 sessions).
- Guardrails: 25-request cap, 150 seconds without progress, repeated semantic
  tool-intent detection, and a $15 calculated-spend ceiling.
- Cost: calculated from provider-reported input, output, and cache-hit tokens
  using the prices active for the run. Cache misses, cache hits, outputs, and
  any offload usage were charged separately.
- Safety: mutation outputs, errors, diffs, and failed commands remained exact.
  Source reads were exact in the corrected matrix.

The test prices were $0.435/M input-cache-miss, $0.003625/M input-cache-hit,
and $0.87/M output tokens for V4 Pro; V4 Flash was $0.14/M, $0.0028/M, and
$0.28/M respectively. Prices are historical test inputs and must not be
treated as current defaults.

## Corrected matrix

| Arm | Runs complete | Median cost | Median input tokens | Cache-hit ratio | Compaction |
| --- | ---: | ---: | ---: | ---: | --- |
| Control | 23/25 | $0.02482 | 450,810 | 92.23% | none |
| Deterministic | 24/25 | $0.02670 | 479,087 | 91.68% | 34 transformations, 365 reuses |
| Model-gated | 24/25 | $0.02247 | 444,625 | 92.75% | 6 deterministic transformations, 79 reuses |

The few request-cap terminations also occurred in control and in treatment
runs with zero transformations, so they were baseline agent variance rather
than a compaction-induced loop in the corrected matrix.

Paired deterministic results had a median cost reduction of $0.000524 per
session (about 2.1%), but a bootstrap 95% interval from -$0.003714 to
+$0.003725 crossed zero. Mean savings were slightly negative. Deterministic
compaction removed 4.81 MB from repeated history, yet transformed-only runs
had a median cost change of -$0.000262 (a small increase). The median paired
input-token reduction was 29,117, with an interval from -57,989 to +60,161.

Model-gated results had a median paired reduction of $0.000478 (about 1.9%),
with a 95% interval from -$0.000998 to +$0.004973. Only four runs transformed
anything, all through the deterministic recoverable-output policy, so this is
not evidence for model-summary savings. The economic preflight correctly made
zero Flash calls: with six expected replays and DeepSeek's very low cache-hit
price, a rewritten prompt prefix could not repay its cache miss.

All 15 safety-task answers preserved and explained the exactness boundary.
Their automated keyword scores understated quality when answers used different
phrasing, so these outputs were manually reviewed.

## Negative pilot findings

An earlier unsafe calibration was excluded from savings estimates. It exposed
two important failures:

1. Delayed source-read markers caused OMP to reread different ranges until a
   request cap. A same-argument guard was insufficient. `source` therefore
   fails closed to `exact` pending the recovery/economics work in issue #178.
2. An uncached optimistic model candidate was labeled as cache reuse, so its
   preflight omitted the prefix-rewrite charge. Torana paid for Flash summaries
   that the final gate rejected. Uncached candidates are now labeled as
   transformations during preflight, which prevents that wasted call.

The run also found that DeepSeek cache hits were not visible through the
standard OpenAI cache-detail field. Torana now recognizes DeepSeek's cache
usage fields in streaming, JSON, and offload responses, and `/stats` reports
offload token usage.

## Conclusion

This dogfood does not support advertising a universal cost-saving percentage.
It demonstrates that Torana can remove large repeated payloads safely for
explicitly recoverable tools, but bytes removed do not directly translate to
bill savings when cached input is extremely cheap or when compaction changes
agent behavior. Developers should validate with paired runs on their own tool
mix and provider prices, using provider-reported tokens and including offload
cost. The economic gate's decision to do nothing is a successful outcome when
compaction cannot prove positive savings.
