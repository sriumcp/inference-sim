# Finding: R_t needs a LONG horizon, and the campaign was running it at the worst one

**Status:** apparatus defect in epoch 1, found before the epoch produced a
recommendation. Epoch 1 stopped deliberately; epoch 2 re-registers with the fix.

## The question

`R_t = Peak_t / t` is an ASYMPTOTIC result (`t -> infinity`). Every measurement
here is a finite run. So: does the discrimination actually appear given a long
enough run, and how long is long enough?

## Measured: yes, and the separation GROWS with the horizon

Fixed rate, seed 42, varying run length (`--num-requests`):

| n | R_t @ 0.5x | R_t @ 0.9x | R_t @ 1.5x | super / worst-sub |
|---|---|---|---|---|
| 500 | 1.147 | 4.109 | 9.618 | **2.3x** |
| 1000 | 0.640 | 2.882 | 8.179 | 2.8x |
| 2000 | 0.342 | 1.630 | 7.677 | 4.7x |
| 4000 | 0.174 | 0.854 | 7.230 | 8.5x |
| 8000 | 0.088 | 0.466 | 6.820 | **14.6x** |

The mechanism is exactly the predicted one -- visible in the peak trajectory:

| load | peak at n=500 -> 8000 | R_t decay |
|---|---|---|
| 0.5x (sub) | 66 -> 71 (saturates) | 13.0x |
| 0.9x (sub) | 162 -> 212 (slows) | 8.8x |
| 1.5x (SUPER) | 329 -> **2743** (grows ~linearly) | 1.4x |

Sub-capacity: Peak stops growing, so `R_t -> 0`. Super-capacity: Peak grows
linearly in `t`, so `R_t` holds near a constant. That IS the theory
(near-constant when overloaded, falling when underloaded), and it needs enough `t`
for the sub-capacity peak to stop growing.

## The cliff-adjacent pair, across all 5 seeds

The pair that actually matters is 0.9x (sub) vs 1.1x (super) -- one rung either
side of the measured cliff:

| n | 0.9x (sub) | 1.1x (super) | gap | seed spread |
|---|---|---|---|---|
| 500 | 4.109 .. 4.311 | 5.854 .. 6.289 | ~1.4x, bands nearly touch | +/-9% |
| 6000 | 0.576 .. 0.621 | 2.182 .. 2.355 | **3.8x, clean** | +/-8% |

Seed spread is horizon-independent (~8-9%), so the long horizon buys separation
without costing stability. At n=500 a threshold can barely be placed; at n=6000
there is room for one.

## Consequence: horizon is APPARATUS, and epoch 1 had it wrong

Epoch 1 ran `--num-requests 500` -- the single worst point on that curve. Every
Peak-based level in the design was being scored where its statistic is least
separable, which is a defect in the INSTRUMENT, not in the candidate.

This also revises the diagnosis in `PEAK-STATISTIC-DIAGNOSIS.md`. That document is
still correct that R_t's LEVEL is horizon-sensitive and that `peak` is a
within-run ratchet. What it got wrong is the conclusion: it read
"no Peak-based configuration discriminates" from short-horizon data, when the
correct statement is "no Peak-based configuration discriminates AT n=500." At
n=6000 `peak_over_elapsed` separates the cliff-adjacent pair by 3.8x across every
seed.

The confound noted there is real and now explained rather than merely flagged:
comparing final R_t across runs of DIFFERENT durations is invalid, because R_t is
horizon-dependent by construction. The fix is not to abandon the level -- it is to
hold the horizon FIXED and LONG across every row, which is what an apparatus
declaration is for.

## Epoch 2 changes (apparatus only)

- `--num-requests 500 -> 6000` in `run_command`, and it becomes a declared,
  frozen apparatus constant (an epoch boundary, never an edit).
- `run_timeout_sec` raised to match the longer runs.
- The ladder, seeds, model, GPU, and cliff are unchanged, so epoch 1's rows are
  not silently pooled with epoch 2's: a new `policy.sha256` marks the boundary.

An apparatus change is an epoch boundary, not an edit -- so epoch 1 was stopped
rather than patched mid-measurement. It never reached a recommendation, so nothing
is being discarded except rows measured on the wrong instrument.
