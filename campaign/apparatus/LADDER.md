# Frozen apparatus: workload, hardware, and ladders

**Frozen 2026-08-24, before any campaign design/build stage ran.** Per
`../agentic-strategy-evolution/docs/optimization-campaign-guide.md` ("An apparatus
change is an epoch boundary, not an edit"), the workload generator and its data
are under the measurement freeze. Nothing in this file may change while an epoch
is measuring; changing it ends the epoch and starts a new one.

## Hardware and model (realistic, from committed BLIS configs)

| Setting | Value | Source |
|---|---|---|
| Model | `meta-llama/llama-3.1-8b-instruct` | `defaults.yaml`, real HF config in `model_configs/llama-3.1-8b-instruct/` |
| GPU | H100 | `defaults.yaml` default for this model |
| Tensor parallelism | 1 | `defaults.yaml` |
| Latency model | `trained-physics` (default) | learned coefficients, not a guess |

Nothing here is invented: the model/GPU/TP triple is the committed default for
this model, and the latency coefficients are the trained ones BLIS ships.

## Measured nominal capacity

Determined by the §1.1 ground-truth definition — a regime is super-capacity iff
**backlog diverges**, which shows up as mean E2E that keeps GROWING with the
horizon instead of stabilizing. Measured at seed 42, comparing n=800 vs n=3200:

| rate (rps) | E2E n=800 | E2E n=3200 | growth | regime |
|---|---|---|---|---|
| 18 | 8763 | 9699 | +11% | sub |
| 19 | 9234 | 10641 | +15% | sub |
| **20** | 9703 | 11758 | +21% | **sub (knee)** |
| **21** | 10164 | 14235 | +40% | **SUPER** |
| 22 | 10616 | 16954 | +60% | super |
| 26 | 12388 | 26706 | +116% | super |

Output throughput independently asymptotes at ~18-19 rps (rate 30 -> 17.97,
rate 50 -> 18.28), corroborating the same knee.

**`r_nominal = 20 rps`.** This is the cliff the ladders are anchored to.

## T1 — Rate ladder (the primary ladder)

Rungs as multiples of `r_nominal = 20`:

| Band | Multipliers | Rates (rps) | Role |
|---|---|---|---|
| Calibration (§3.4) | 0.3, 0.4, 0.5, 0.6 | 6, 8, 10, 12 | FPR budget measured HERE only |
| Gray zone (§3.4) | 0.7, 0.8, 0.9, 0.95 | 14, 16, 18, 19 | charged to no budget |
| Cliff | 1.0 | 20 | the knee itself |
| Super-capacity | 1.1, 1.25, 1.5, 2.0 | 22, 25, 30, 40 | T1 must fire; T4 scored here |

## T2-IN — Prompt-size ladder

Rate held at 0.6x nominal (12 rps, inside the calibration band so the BASE rung
is known-stable); prompt scaled, output fixed. A detector with no prefill term
in its statistic is structurally blind to this ladder.

Multipliers on the base prompt length: 1x, 2x, 4x, 8x, 16x.

## T2-OUT — Output-size ladder

Rate held at 0.6x nominal (12 rps); output scaled, prompt fixed.
Multipliers: 1x, 2x, 4x, 8x.

## T3 — Capacity ladder

Workload held BYTE-IDENTICAL; server capacity reduced. Capacity is reduced by
KV-cache pressure (`--total-kv-blocks`), NOT by inflating per-request service
time: per `metamorphic_tests.md` §1.2 (the factor-B trap), inflating residency
while completions still stream raises CONCURRENCY without reducing capacity. A
real collapse must reduce COMPLETED TOKENS PER UNIT TIME.

## T4 — Temporal consistency

Scored ONLY on super-capacity-mean traces (>= 1.1x nominal), per §5.2. Bursts
hold the MEAN constant and inject variance; a burst followed by a lull is NOT a
load reduction. Flip count = transitions from fired to not-fired.

## Seeds

Five seeds: 42, 43, 44, 45, 46. Per-rung verdict = majority vote (>= 3 of 5),
per §3.5. Run -> verdict per seed: FIRED iff the detector is fired for >= 50% of
the run after discarding one warm-up burst-lull cycle.

## What is NOT in scope for epoch 1

Agentic multi-turn with context accumulation. Per the user's decision (option c)
it is epoch 2: context growth makes offered load non-stationary WITHIN a rung,
which breaks the stationary-mean premise T4's scoring rule depends on. Treating
it as a separate epoch with a re-registered policy is the correct shape.

## Addendum: measured horizon dependence per statistic (checklist item 3)

The horizon must be measured for EACH statistic being scored, not assumed from one.
Measured at seed 42, `in_flight`, threshold set high so the raw statistic is visible:

**`peak_over_elapsed`** -- super vs WORST sub rung, whole ladder:

| n | separation |
|---|---|
| 500 | 2.3x |
| 2000 | 4.7x |
| 8000 | 14.6x |

Cliff-adjacent pair (0.9x sub vs 1.1x super), all 5 seeds: **3.8x gap at n=6000**
(0.58-0.62 vs 2.18-2.36), +/-8% seed spread.

**`peak_ratio_stability`** (H=2.0) -- 0.6x sub vs 1.5x super:

| n | sub | super | separation |
|---|---|---|---|
| 1500 | 0.6437 | 0.8179 | 1.27x |
| 3000 | 0.6625 | 0.8630 | 1.30x |
| 6000 | 0.6708 | 0.8951 | **1.33x** |

Correctly ORDERED at every horizon (super > sub, no inversion) and improving slowly
with n, but far weaker than `peak_over_elapsed` -- 1.33x on an easy pair versus 3.8x
on the hard cliff-adjacent pair. It stays a factor level so the campaign RANKS it
rather than the author excluding it; the expectation on this evidence is that it
loses.

**n = 6000 is the declared horizon for both**, and it is on the plateau rather than
the slope for each: `peak_over_elapsed` gains 14.6x/2.3x = 6.3x from n=500 to 8000
with most of it realized by 6000, and `peak_ratio_stability`'s curve is nearly flat
(1.27 -> 1.33) so a longer horizon buys it almost nothing.
