# Epoch 6: the construction answer, and a PARTIALLY REFUTED prediction

**The first epoch to run the whole pipeline.** screen -> foldover -> refine -> confirm (2 rounds)
-> report. 50 rows, **all 50 complete, zero failures, zero infeasible**. No semantic exception:
there is no `epoch_end` file because the epoch ended by reaching `report`, not by dying.

**Verdict: `certified=False`**, and the reason is unusual enough to state carefully.

---

## 1. The answer to "what is the best construction?"

Final recommendation, basis `terminal_best`:

```
BOUND   = mixture_sprt        <- dominates on BOTH speed and earliness
ALPHA   = 0.1                 <- fastest; but see the trade in section 2
DISPWIN = 10 s (10000000 us)  <- long window beats short
INDET   = lean_stable         <- effectively arbitrary (see section 4)
```

Main effects over the completed rows, stable to within ~2 observations as the row count grew
from 12 to 17 to 50 (so these are not small-sample noise):

| factor | level | mean obs | mean lead |
|---|---|---|---|
| **BOUND** | `mixture_sprt` | **136.8** | **0.800** |
| | `howard_eb` | 142.6 | 0.860 |
| **ALPHA** | 0.01 | 149.0 | **0.800** |
| | 0.06 | 135.0 | -- |
| | 0.10 | **131.2** | 0.843 |
| **DISPWIN** | 10 s | **137.2** | -- |
| | 5 s | 135.0 | -- |
| | 0.5 s | 143.0 | -- |
| **INDET** | `strict` | 139.6 | -- |
| | `lean_stable` | 140.1 | -- |

**mixture-SPRT dominates Howard on both axes** -- ~6 observations faster AND warning 0.06 of the
cliff earlier. No trade, so this part of the construction question has a clean answer. Plausible
mechanically: a mixture-SPRT integrates over a prior on the effect size instead of paying a
uniform-over-time union bound, so it is tighter near the threshold, which is exactly where a
saturation detector operates.

## 2. The real design tension: ALPHA trades speed AGAINST earliness

A single scalar objective would have hidden this, and it is the most actionable finding:

- alpha = 0.10 commits **18 observations sooner** (131.2 vs 149.0)
- alpha = 0.01 warns at a **lower load** (lead 0.800 vs 0.843 of the cliff)

A looser budget clears the threshold sooner in absolute terms. A TIGHTER budget makes the
detector willing to commit further from the cliff, because the evidence it demands is stronger
and therefore persuades earlier. So the operational question decides:

| you want | pick |
|---|---|
| "tell me fast once it is saturated" | alpha = 0.10 |
| "tell me BEFORE it saturates" | alpha = 0.01 |

For capacity planning the second is usually worth more, which **inverts the naive reading of the
speed objective**. The campaign's own recommendation (alpha 0.1) optimizes the declared primary
metric; it is not automatically the right deployment choice.

## 3. THE BURSTINESS PREDICTION IS PARTIALLY REFUTED

Pre-registered: *observations-to-verdict grows monotonically with the measured dispersion index
`I`.* Measured over 50 rows:

| level | measured I | mean obs | mean lead |
|---|---|---|---|
| constant | 0.002 | **135.7** | 0.716 |
| poisson | 0.430 | **121.7** | 0.808 |
| gamma_cv2 | 1.433 | 141.9 | 0.732 |
| gamma_cv4 | 5.977 | 147.2 | 0.834 |

**Strict monotonicity: FALSE.** `constant` (I~0.002) needs MORE evidence than `poisson` (I=0.43)
-- 135.7 vs 121.7 -- so the ordering breaks at the least-bursty end.

**But the TREND is positive**: OLS of obs on log(I) gives **+1.09 observations per e-fold of I**,
and the three genuinely stochastic levels are correctly ordered
(121.7 -> 141.9 -> 147.2 as I goes 0.43 -> 1.43 -> 5.98).

So the honest verdict, stated as the pre-registration requires:

- **The directional claim SURVIVES**: burstier arrivals do require more evidence, across the
  stochastic range.
- **The strict monotonicity claim FAILS**, at the deterministic end.

Why `constant` is anomalous, and it is mechanically explicable rather than noise: the width
scales on `max(1, I)`, which is **floored at 1** deliberately -- narrowing below the i.i.d. bound
on under-dispersed arrivals would break coverage, since the martingale argument does not license
it. So every level with `I < 1` (constant at 0.002, poisson at 0.43) gets the SAME width
treatment, and the floor erases the distinction the prediction expected to see. `constant`'s
extra cost then comes from somewhere else entirely: with perfectly regular arrivals the peak
grows in lockstep with time, so the growth exponent sits near its own boundary for longer.

**This is a genuine scientific result: the prediction was falsifiable, it was tested, and it
half-failed for a reason traceable to a deliberate design decision in the bound.**

## 4. INDET is inert, as measured before launch

139.6 vs 140.1 -- a 0.5-observation gap, converging toward the exactly-zero that `--liveness`
measured pre-launch. Structural, not noise: an undecided verdict reports `Level=Stable` under
BOTH policies, so the first-fired index cannot differ. Its appearance in the recommendation is
arbitrary among equals. It should be scored against `t1_pass` / `correctness_bursty`, which it
CAN move -- the epoch-7 change.

## 5. Why certification was withheld -- and why it is not a failure

From the campaign's own report:

> *"every finalist returned 129.0 on all 5 replicates, so the t-interval for challengers f1 and
> f2 would collapse onto its point estimate"*

Verified: all five confirm replicates returned **exactly 129.0**. Zero variance. The simulator is
deterministic (INV-6), and the confirm block replicated the same workload seed, so there is no
spread for a t-interval to be computed over. `R_0.05` is therefore `unknown` rather than large,
and the policy correctly reported `uncertified` at its registered round cap instead of inventing
a guarantee.

**So the practical answer is in hand and exactly reproducible; the statistical guarantee is not.**
The fix is the one already recorded in `EPOCH2-REQUIREMENTS.md`: vary `NOUS_WORKLOAD_SEED` across
replicates. `workload.seed_env` is declared but the confirm block replicated one seed, which is
the same defect that made `--liveness` report `sd=0` and `inf noise`.

## 6. Held-out transfer: a warning worth acting on

`weibull_cv3` (held out, never fitted) behaves unlike all four fitted levels: lead **0.9** and
gray-fired-fraction **0.33**, against 0.7-0.85 and ~0.67 for the fitted ladder. Its stopping time
(135) is fine, but its EARLY-WARNING margin is materially worse.

Combined with epoch 2's finding that capacity is not monotone in CV across arrival FAMILIES
(weibull CV=3 ties `constant`), this says: **a construction tuned on the gamma family should not
be assumed to transfer to weibull arrivals.** The held-out level earned its place.

## 7. What this campaign established, in order of confidence

1. **`mixture_sprt` > `howard_eb`**, on both speed and earliness, stable across 50 rows. High
   confidence, no trade.
2. **The wrapper gives a real ~20-30% early-warning margin** -- silent across the entire 0.3-0.6
   calibration band (FPR 0.0 vs a 0.05 budget) while firing from 0.7-0.8x the cliff. High
   confidence; the calibration band is the control.
3. **ALPHA trades speed against earliness**, in opposite directions. Medium-high confidence: the
   effect is 18 observations, far the largest, and monotone across three levels.
4. **A long dispersion window beats a short one** (137.2 vs 143.0). Medium confidence -- the
   5 s level (135.0) is slightly better than 10 s, so the surface is flat near the top.
5. **Burstier traffic needs more evidence** across the stochastic range (+1.09 obs per e-fold of
   I), but NOT monotonically once the `max(1, I)` floor binds. Medium confidence, and explicitly
   a partial refutation of the pre-registered claim.
6. **The wrapper costs ~2x the stopping time of bare composite** (129 vs 63) while holding bursty
   correctness at 1.0. It buys calibration-freedom, an explicit undecided state, and early
   warning -- not speed.
