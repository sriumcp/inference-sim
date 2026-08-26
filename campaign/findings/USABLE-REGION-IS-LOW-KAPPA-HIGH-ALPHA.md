# Epoch 3: the factor space is mostly unusable, and the boundary of the usable region IS the result

**Epoch 3 halted:** 8 of 12 rows infeasible, leaving 4 -- too few to fit three factors, so the model
matrix was rank-deficient and no coefficient was estimable. The circuit breaker stopped after three
identical iterations rather than spend the budget reproducing one defect.

Two of my declarations were wrong, and the data says so precisely.

## Defect 1: `correctness >= 1.0` demanded perfection

I set the floor at 1.0 after a QUICK-mode probe showed correctness 1.000 everywhere, reading it as a
bar every admissible configuration clears. On the FULL five-level ladder correctness spans 0.0 to
1.0, so demanding perfection turned a graded response into a pass/fail gate.

It discarded the most informative point in the design: a row at correctness **0.95** that held its
FPR budget AND posted the **best lead time on the board (0.8375)**. A constraint should exclude
configurations that are UNUSABLE, not ones that are imperfect. Now 0.9 -- the threshold the `regimes`
block already applies to this same quantity.

## Defect 2: the boundary grid's ceiling was too low

5 of 12 rows pinned the calibration knob at the old grid top (0.60), so the operating point they
wanted lay OUTSIDE the grid and their reported FPR was an artifact of my range rather than a
calibration. `knob_at_grid_edge` was measuring my choice instead of a genuine failure to calibrate.

**A hypothesis of mine the data refuted:** I suspected the required boundary was coupled to `kappa`
(a wider interval needing a higher boundary for the same FPR), which would have meant no single grid
could serve the factor range. Measured, the ceiling is hit at EVERY kappa level -- 1.0, 5.5 and 10.0
each chose 0.60 on some row. It is simply a ceiling that is too low. Extended to 0.90, the last
meaningful step before the exponent's support (0,1) makes the boundary unreachable.

## The substantive finding: correctness is monotone in BOTH factors, in opposite directions

| kappa | correctness across rows |
|---|---|
| 1.0 | 0.50, 0.85, 1.00, 1.00 |
| 5.5 | 0.05, 0.45, 1.00, 1.00 |
| 10.0 | 0.00, 0.05, 0.80, 0.95 |

| alpha | correctness across rows |
|---|---|
| 0.01 | 0.00, 0.05, 0.85, 1.00 |
| 0.06 | 0.05, 0.45, 1.00, 1.00 |
| 0.10 | 0.50, 0.80, 0.95, 1.00 |

**Correctness degrades with kappa and improves with alpha**, and both directions are mechanically
necessary rather than empirical accidents:

- `kappa` is the prior quadratic variation, so it sets the interval's WIDTH FLOOR. A wider interval
  straddles the boundary longer, commits less often, and therefore misses super-capacity rungs.
- `alpha` is the coverage budget. A LOOSER budget (0.10) narrows the interval, so it commits sooner
  and catches more rungs; a tighter budget (0.01) demands more evidence and commits less.

So the usable region is **low kappa, loose alpha** -- and my declared levels reach well past it in
both directions. `KAPPA=10` with `ALPHA=0.01` is the corner where the detector essentially never
commits (correctness 0.00), which is a real and reportable property rather than a bug.

## Why this is a result and not merely a design error

The campaign was asked which configuration is most statistically robust. The answer emerging is a
CONSTRAINT SURFACE rather than a point: **the detector is usable only where the interval is narrow
enough to commit, and the two knobs that widen it (high kappa, tight alpha) trade correctness for
coverage guarantees.** That trade is the honest characterization of an anytime-valid detector -- the
guarantee is not free, and this measures its price in missed detections.

The narrowing that follows is therefore justified by measurement rather than convenience: KAPPA to
[1.0, 3.0, 5.5] and ALPHA to [0.05, 0.10] keeps the region where the detector is admissible at all,
which is where a configuration recommendation is meaningful. Reporting a recommendation from a region
where 8 of 12 points are infeasible would be reporting the shape of my own constraints.
