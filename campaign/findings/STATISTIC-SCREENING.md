# Pre-campaign screening: which reflected-walk statistics carry signal

**Measured 2026-08-24 on the frozen apparatus** (llama-3.1-8b-instruct / H100 /
TP=1 / trained-physics, r_nominal = 20 rps, 600 requests, seed 42, threshold set
to 1e6 so the raw statistic is observable without the verdict interfering).

This is APPARATUS SCREENING, not a result: it establishes which factor levels are
runnable at all, the same way `screen_levels` exists so a campaign need not spend
rows on known-pathological extremes. It does not choose a winner — the campaign
does that.

## The five statistics across the rate ladder

| statistic | 0.3x | 0.6x | 1.0x | 1.5x | 2.0x | ratio | verdict |
|---|---|---|---|---|---|---|---|
| `peak_over_elapsed` (R_t = Peak/t) | 0.338 | 1.435 | 4.666 | 9.17 | 11.71 | **35x** | strong, monotone |
| `peak_decay_rate` (Peak/sqrt t) | 3.49 | 10.98 | 30.39 | 56.89 | 71.77 | **21x** | strong, monotone |
| `excursion_rate` | 106.4 | 58.5 | 42.4 | 38.5 | 37.6 | 2.8x (inverse) | moderate, monotone |
| `excursion_scaling` | 0 | 0 | 0 | 0 | 0 | — | **DEAD** |
| `idle_fraction` | 1 | 1 | 1 | 1 | 1 | — | **DEAD** |

## Why two are dead, and why that is a real finding

Diagnostic at 0.3x nominal: `excursions = 1`, `idle_fraction = 0`.

**In-flight never returns to zero during an LLM serving run.** The walk leaves
zero at the first arrival and returns only when the run drains at the end. Tested
down to 0.05x nominal (1 rps): still exactly ONE excursion and zero idle time,
because each request's E2E (~4s) far exceeds the inter-arrival gap even at 1 rps.

So the reflecting boundary is **never visited** in this regime. Statistics that
depend on visiting it — excursion count, the T ~ M^b scaling exponent, and
boundary occupancy — have no data to work with. This is a property of continuous
batching (the engine holds many requests concurrently by design), not a defect.

Consequence for the theory: the reflected-random-walk framing is right about the
DRIFT regimes but the excursion-based half of it does not transfer to LLM serving,
because the queue is effectively always occupied. The `max(0,·)` reflection still
matters for a WORK residual (which does return to zero when the backlog clears);
it does not for a CONCURRENCY walk.

`excursion_rate` survives only because it degenerates gracefully: with one
excursion it becomes ~ 1/(1/elapsed) = elapsed, i.e. it is measuring run length,
not excursions. That makes its apparent monotonicity an artifact — it is retained
as a declared level so the campaign REFUTES it explicitly rather than omitting it
silently, but it is expected to lose.

## What goes into the campaign

- `peak_over_elapsed` and `peak_decay_rate`: the real candidates. Both are
  Peak-based, need NO capacity estimate, and so are immune to the identifiability
  trilemma (see ESTIMATOR-IMPOSSIBILITY.md).
- `excursion_rate`: declared, expected to lose, kept for falsifiability.
- `excursion_scaling` and `idle_fraction`: EXCLUDED from the factor levels, with
  this document as the recorded reason. A factor level that provably cannot vary
  is not a level; including it would spend rows measuring a constant and dilute
  the screen.
- `source` (in_flight vs work_backlog) stays a live factor, but the open question
  it raised is now ANSWERED, and the answer is no.

## Addendum: does `work_backlog` revive the reflection? No.

The hypothesis was that unserved WORK might return to zero even when concurrency
does not, restoring the reflecting boundary the excursion statistics need.
Measured on the same apparatus:

| rate | excursions | idle_fraction | peak | R_t |
|---|---|---|---|---|
| 0.3x | 1 | 0.0000 | 22,814 | 302.7 |
| 1.0x | 0 | 0.0000 | 117,352 | 3,556.4 |
| 2.0x | 0 | 0.0000 | 193,839 | 6,705.8 |

Still 1 or 0 excursions and zero boundary occupancy: the work walk never returns
to zero either, for the same structural reason -- requests always overlap, so
there is always unserved work resident. **The reflecting boundary is unreachable
in LLM serving regardless of which quantity is treated as the walk.** That closes
the excursion family entirely for this domain, not just for the concurrency walk.

What `work_backlog` DOES buy is a stronger Peak signal: R_t spans 22x across
0.3x-2.0x (302.7 -> 6,705.8), monotone, versus in_flight's 35x on a much smaller
absolute scale. Both are live campaign levels; which wins on lead time at matched
FPR is the measurement the campaign makes, not one this screening pre-judges. Note
the two sources need thresholds orders of magnitude apart, which is exactly why the
per-row FPR calibration (rather than a shared declared threshold) is what makes
them comparable at all.
