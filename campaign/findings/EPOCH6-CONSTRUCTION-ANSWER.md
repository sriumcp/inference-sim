# Epoch 6: which CONSTRUCTION is best, and the trade-off inside it

**Partial result, recorded at 12 of 20 screen rows.** First epoch where the question is
answerable at all: per-level stopping times and lead time both emit, the contract is
type-stable, and 12/12 rows are COMPLETE with zero infeasible.

## The construction ranking

Main effects over the 12 feasible rows:

| factor | level | mean obs (speed) | mean lead_mult (earliness) |
|---|---|---|---|
| **BOUND** | `mixture_sprt` | **136.3** | **0.800** |
| | `howard_eb` | 141.0 | 0.860 |
| **ALPHA** | 0.10 | **130.7** | 0.843 |
| | 0.01 | 148.8 | **0.800** |
| **DISPWIN** | 10 s | **134.8** | -- |
| | 0.5 s | 141.7 | -- |
| **INDET** | `lean_stable` | 137.4 | -- |
| | `strict` | 138.9 | -- |

## Finding 1: mixture-SPRT dominates Howard, on BOTH axes

`mixture_sprt` is simultaneously FASTER (136.3 vs 141.0 observations) and EARLIER-WARNING
(0.800 vs 0.860 of the cliff). No trade -- it is better on both, so for this setting the
bound-family question has an unambiguous answer.

Mechanistically plausible: a mixture-SPRT integrates over a prior on the effect size rather
than paying a uniform-over-time union bound, so it is tighter when the true effect is near the
threshold -- which is exactly the regime a saturation detector lives in.

## Finding 2: ALPHA trades speed against earliness, in OPPOSITE directions

This is the substantive result, and a single scalar objective would have hidden it:

- alpha = 0.10 commits **18 observations sooner** (130.7 vs 148.8)
- alpha = 0.01 warns **0.043 of the cliff earlier** (0.800 vs 0.843)

A looser error budget lets the interval clear the threshold sooner in absolute terms, but a
tighter budget makes the detector willing to commit at a LOWER LOAD, because the evidence it
demands is stronger and therefore more persuasive further from the cliff.

So "best" depends on the operational question:
- **"Tell me fast once it's saturated"** -> alpha 0.10.
- **"Tell me before it saturates"** -> alpha 0.01.

For capacity planning the second is usually worth more, which inverts the naive reading of the
speed objective. This is precisely why lead time had to be a scored regime rather than a note.

## Finding 3: the dispersion window prefers LONG (10 s over 0.5 s)

134.8 vs 141.7 mean observations. Consistent with the theory: the index of dispersion I is a
variance-over-mean estimate, so a short window gives a noisy I, and because the width scales
with `max(1, I)` the noise can only ever WIDEN the interval -- a noisy I is a systematically
wider one. A longer window buys a steadier I and hence a tighter interval.

## Finding 4: INDET remains nearly inert on speed, as measured pre-launch

137.4 vs 138.9 -- a 1.5-observation difference, the smallest of any factor, consistent with
`--liveness` measuring its effect as exactly 0. Structural: an undecided verdict reports
`Level=Stable` under BOTH policies, so the first-fired index cannot differ. It should move
`t1_pass` / `correctness_bursty` instead, which is the epoch-7 change.

## Caveats, stated plainly

1. **12 of 20 rows.** These are partial main effects from an unbalanced subset (n=5 vs 7 per
   level), not the fitted model. The final fit may reorder the weak factors.
2. **No significance yet.** center_points 4 was declared so a pure-error estimate exists in
   the final fit, but these means carry no confidence intervals. The ALPHA effect (18
   observations) is large relative to the others; the INDET effect (1.5) is not.
3. **Every row has calibrated_fpr = 0.0** against a 0.05 budget -- the same overpayment the
   plan stage criticised in bare composite. The detector is systematically more conservative
   than its budget requires, which is part of why it is slower than what it wraps.
4. **The held-out level is already behaving differently**: weibull sits at lead 0.9 /
   gray_frac 0.33 while all four fitted levels sit at 0.7-0.9 / 0.67. A construction tuned on
   the fitted ladder should NOT be assumed to transfer, and confirm will measure exactly that.
