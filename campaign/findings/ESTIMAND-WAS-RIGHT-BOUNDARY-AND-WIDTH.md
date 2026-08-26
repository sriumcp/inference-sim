# I mis-diagnosed the blindness twice. The estimand is right; the geometry is the problem.

**Measured on real BLIS traces at six load levels around the measured cliff (88 rps).** Two of
my own diagnoses were wrong and the data corrected both.

## Diagnosis 1 (WRONG): "the cumulative exponent stops discriminating on a capped queue"

I reasoned that a real server pins at its concurrency ceiling, so an overloaded run's Peak goes
flat and the cumulative log-log slope decays toward the same place a healthy run ends up. I
proposed replacing it with a trailing-window exponent or with ceiling occupancy.

**Measured, and the opposite is true:**

| rate | x cliff | cumulative gamma | trailing gamma | ceiling occupancy |
|---|---|---|---|---|
| 26 | 0.30 | 0.115 | 0.000 | 0.000 |
| 53 | 0.60 | 0.214 | 0.000 | 0.006 |
| 79 | 0.90 | 0.336 | 0.013 | 0.135 |
| 88 | 1.00 | 0.401 | 0.002 | 0.104 |
| 106 | 1.20 | 0.510 | 0.000 | 0.196 |
| 176 | 2.00 | 0.691 | 0.000 | 0.000 |

Separation, worst super-capacity value minus best sub-capacity value:

- **cumulative gamma: +0.174 -- SEPARATES**, and is monotone across the whole range
- trailing gamma: -0.013 -- OVERLAPS (peak is pinned, so there are no new records in the tail)
- ceiling occupancy: -0.135 -- OVERLAPS (0.000 at 2x the cliff, WORSE than at 0.9x)

**Both of my proposed replacements are worse than what is already there.** Had I acted on the
diagnosis instead of measuring it, I would have broken a working estimand. The cumulative
exponent is the right object.

## Diagnosis 2 (PARTLY WRONG): "the boundary is mis-set at 0.5"

Correct as far as it goes: theory puts criticality at gamma = 0.5 for an UNBOUNDED walk, but the
measured cliff here lands at gamma = 0.401, BELOW 0.5. So a detector testing gamma > 0.5 is
testing the wrong threshold on this apparatus.

But re-calibrating it does not rescue the detector. Sweeping the boundary:

```
boundary=0.30:  . . . . . O        rates:  26  53  79  88 106 176
boundary=0.35:  . . . . . O        (cliff = 88)
boundary=0.40:  . . . . . O
boundary=0.45:  . . . . . O
```

Every boundary gives the same verdicts. It fires at 2x the cliff and is silent below -- an
improvement over never firing -- but it misses 1.2x at EVERY boundary, and the boundary barely
matters. So the boundary is not the binding constraint.

## The actual constraint: the interval is as wide as the signal

At n = 1600, boundary 0.40:

| rate | gamma | interval | width |
|---|---|---|---|
| 106 (1.2x) | 0.357 | [0.276, 0.439] | 0.163 |
| 176 (2.0x) | 0.409 | [0.321, 0.498] | 0.177 |

**The sub-to-super signal gap is ~0.17 and the interval width is ~0.17.** The detector is
correctly reporting that it cannot separate 1.2x from sub-capacity at this sample size. That is
the guarantee working, not a defect -- an interval narrower than that on this evidence would be
the coverage violation the whole construction exists to avoid.

Three consequences, and they are the real design constraints:

1. **A confidence-sequence detector on this statistic needs more observations than a point-
   estimate rule** to reach the same verdict. That is the price of the guarantee, and it is
   the honest reason this detector is slower than composite (129 vs 63 observations in the
   earlier epoch) rather than anything fixable by tuning.
2. **The width is driven by the exponent's own estimation noise**, so the lever that matters is
   the SIGNAL, not the threshold: anything that makes gamma's separation larger (a longer run, a
   less noisy peak series) buys more than moving the boundary.
3. **The near-cliff region may be genuinely undecidable at these horizons**, which is a finding
   rather than a failure. A detector that says "undecided" at 1.2x load and commits at 2.0x is
   more honest than one that guesses at 1.2x -- and epoch 4 measured that guessing is exactly
   what peak-rate does, firing at 0.3x the cliff.

## What the campaign must now measure

- **BOUNDARY as a factor** (0.30-0.50), calibrated on the FPR band only (0.3-0.6x cliff) and
  FROZEN before the ladder is scored -- the DS1 invariant already enforced for every other
  detector knob. That keeps it calibration, not p-hacking.
- **KAPPA as a factor** (1.0-5.0), since it directly sets the width floor and therefore the
  near-cliff resolution. Smaller kappa narrows the interval at the cost of small-n coverage;
  the trade is exactly what a campaign is for.
- **Horizon**, because consequence 1 says this detector's verdict is horizon-limited in a way the
  static rules are not. If it separates 1.2x at n = 6000 but not at n = 1600, that is the
  headline number for a deployment decision.
- **The four static detectors as reference rows**, on identical traffic, at matched FPR. Note
  what the load sweep already shows: composite fires BACKLOGGED at 0.3x the cliff and peak-rate
  fires OVERLOADED at 0.3x -- both false alarms on healthy traffic, where anytime is silent.
  The comparison is not "who fires soonest" but "who is right".

---

## Addendum: what epoch 3's first row teaches (and a caution about hand-reproduction)

The first scored row -- `ALPHA=0.06, KAPPA=5.5, LATCH=true` -- came back **infeasible**, and it is
real data rather than a defect:

```
mean_lead_mult 0.925   correctness 0.45   calibrated_fpr 0.20   frozen_knob 0.60 (grid top)
```

Three constraints rejected it independently: `fpr_within_budget`, `knob_at_grid_edge`, and
`correctness`. The calibration walk climbed to the top of the boundary grid (0.60) and still could
not hold the 0.05 budget, which is exactly what `knob_at_grid_edge` exists to catch -- the
operating point that configuration wants lies OUTSIDE the grid, so its reported FPR is a grid
artifact rather than a calibration.

That is a genuine property of that corner. `KAPPA=5.5` widens the interval (it is the prior
variation, so it sets the width floor), and a wider interval commits later and less often -- so
correctness falls to 0.45 while the boundary has to climb to compensate, which then costs
false alarms. The trade is visible in one row.

**The caution, which cost me several detours.** I tried to reproduce the FPR 0.20 by hand and got
STABLE at every calibration rung, at several seeds, at several boundaries. The two numbers are not
comparable: the campaign's FPR is the §3.5 MAJORITY-VOTE RUNG RULE over 5 seeds x 2 levels x 4
calibration multipliers, while `blis run`'s stdout label is the last-window plurality verdict of a
single run. They answer different questions, and a mismatch between them is not evidence of
anything.

The general rule: **an aggregate scored by the harness cannot be checked against a single run's
headline label.** To verify a harness aggregate, recompute it from the same records the harness
used -- or trust the harness and check its inputs instead. Reproducing it from a different
quantity produces a contradiction that looks like a bug and is not one.

## OPEN QUESTION: I could not reproduce the row's calibration walk, and I am flagging it rather
## than explaining it away

Epoch 3's first rows recorded this calibration walk at `ALPHA=0.06, KAPPA=5.5`:

```
knob 0.25 -> fpr 0.75      knob 0.45 -> fpr 0.35
knob 0.30 -> fpr 0.65      knob 0.50 -> fpr 0.20
knob 0.35 -> fpr 0.50      knob 0.60 -> fpr 0.20
knob 0.40 -> fpr 0.40
```

Using the harness's OWN `rung_fired()` and `write_cfg()`, with the same alpha/kappa/latch, the same
`num_requests=800`, the same full 4-multiplier calibration band, all five levels, and both seed
offsets, I measured **0 false alarms out of 20 rungs at boundary 0.60** -- against the row's 0.20.

I verified the obvious candidates and none explains it: `cfg_resolved` shows the factors patched
correctly (alpha 0.06, kappa 5.5); `write_cfg` produces a config identical to the one I tested; the
env-var ordering in `main()` is correct (they are set before any `write_cfg` call); and `sigma0` is
absent in both, so both fall back to the same Go default.

**What I am NOT doing: concluding the row is wrong.** The row ran through the campaign's own code
path with its own environment, and my probe is the reconstruction -- so the probe is the more likely
suspect. `NOUS_RUN_DIR`, working directory, and the reloaded-module seed offset all differ between
the two, and any of them could matter.

**Why this is recorded instead of chased further.** I have spent a dozen probes on it and the
campaign is meanwhile producing usable rows. The discrepancy does not threaten the epoch: the rows
are internally consistent (the walk is monotone in the knob, and the constraints reject the corner
for three independent reasons), and `infeasible` rows are retained as real data either way.

**What it DOES threaten, and what to do about it.** If I cannot reproduce a row's calibration by
hand, then I cannot independently verify any FPR the campaign reports -- which matters for a
publishable claim, since "calibrated to a matched false-alarm rate" is load-bearing for every
comparison. Before publishing any number from this epoch, the reconciliation has to be closed:
recompute one row's FPR from its OWN saved per-rung records rather than by re-running, which removes
every environmental difference from the comparison. That means the adapter should persist per-rung
verdicts, not just the aggregate -- an instrumentation gap this exposed.

This is the third time in this work that hand-reproduction of a harness aggregate has misled me. The
lesson is now explicit: **verify an aggregate from the harness's own saved records, never by
re-running the harness.**
