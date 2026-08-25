# The candidate ties composite, and the tie survives a sharper metric

**This is the honest headline, and it does not favour the new detector.**

## First: the rung rule was a ceiling, and I nearly reported a scoring artifact

`randomwalk` and `composite` scored byte-identically on ALL 11 reported fields at full
resolution -- same regret (9.0), same rungs fired, same T2-IN/T2-OUT patterns, same
FPR, same lead time. An exact match across 11 independent fields is suspicious, so I
checked whether the detectors actually differ or whether the scoring had collapsed.

They differ. On the same 2000-request trace at 0.9x nominal:

| detector | first fires at event | fired fraction |
|---|---|---|
| randomwalk | 21 | 0.995 |
| composite | 3 | 0.978 |

So the identical scores were a **ceiling effect in my metric**, not equivalence. §3.5
asks only whether a detector was fired for >= 50% of a run; both sit at 97-99% on
every rung they fire at, so the test saturates and lead time quantizes to the nearest
ladder rung. §1a lists LEAD TIME and TIME-TO-DETECTION as separate metrics and my
adapter measured neither directly -- only WHICH RUNG fires, a coarse proxy for the
first and no proxy at all for the second.

Fixed by adding the continuous quantity: microseconds from the first scored record to
the first fired record, median over the super-capacity traces, folded into regret at a
weight that breaks ties without ever outweighing a failed test.

## With the sharper metric, the tie is real

| detector | T1 | T2-IN | T2-OUT | T4 | FPR | median detection delay | regret |
|---|---|---|---|---|---|---|---|
| **randomwalk** | PASS | PASS | PASS | PASS | 0.0 | **0.0 ms** | **10.0** |
| composite | PASS | PASS | PASS | PASS | 0.0 | **0.0 ms** | **10.0** |
| threshold | PASS | PASS | PASS | PASS | 0.0 | 6598 ms | 10.66 |
| backlog-drift | FAIL | PASS | FAIL | FAIL | 0.25 | — | 340.0 |

The metric now discriminates -- it separates `threshold` by 6.6 seconds, which the
rung rule could not see. And with it, `randomwalk` and `composite` are still
equivalent: identical on every test, identical FPR, identical detection delay.

## What this means for the campaign's claim

**The drafts' bar is not met by the author-chosen configuration.**
`detection_strategies.md` §3b: *"merely matching the §2b level/symptom detectors is
not enough"*. At regret 10.0 with an identical detection delay, `randomwalk` matches
`composite`. It does not beat it.

That is a legitimate outcome to report, and reporting it is the point of having
pre-registered the objective before measuring.

## What IS established

1. **The reflected-random-walk idea works.** `R_t = Peak_t/t`, at n=6000, produces a
   clean step function at the capacity cliff (silent 0.3x-0.7x, fired 0.9x-2.0x),
   passes all four metamorphic tests, and holds a 0.0 false-alarm rate against a 0.05
   budget with its knob strictly interior. That was not known before this work; the
   statistic was PROPOSED in the drafts with no implementation.
2. **The horizon, not the statistic, was the obstacle.** Separation grows 2.3x -> 14.6x
   from n=500 to n=8000. Every earlier negative result was an n=500 artifact.
3. **`backlog-drift` is genuinely broken** -- regret 340, fails T1/T2-OUT/T4, and
   cannot hold the false-alarm budget (FPR 0.25) even at maximum sensitivity. The
   misimplementation diagnosed by reading the code at the start of this work is
   confirmed empirically: an OLS slope over ALL buckets rather than a trailing window,
   a dimensionally-wrong 1/sqrt(arrivals) noise floor, and degeneracy at rho ~= 1 where
   backlog grows like sqrt(t) so a linear fit tends to zero.
4. **`threshold` is slower by 6.6 s**, which only the continuous metric reveals.

## What remains open

Whether ANY of the 1200 points beats `composite` -- a different `SRC`, `WARM`,
`HORIZON`, or `CONSEC` may. The author-chosen point does not, and finding out is
exactly what the campaign is for. That is the epoch-4 question.
