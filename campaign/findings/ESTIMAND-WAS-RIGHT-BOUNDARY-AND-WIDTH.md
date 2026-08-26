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
