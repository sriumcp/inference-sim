# The defensible claim: the CS beats static rules on TRANSFER, not on raw speed

**Measured before building anything, on simulated log-log paths where the true growth exponent
is known.** All rules calibrated to the SAME false-alarm budget on critical (g=0.5) paths first,
per §3.4 -- an uncalibrated comparison is not a comparison.

## Speed at matched FPR: honest mixed result

| rule | mean n to decide (g=1.0) | missed |
|---|---|---|
| prequential CS (alpha 0.05, kappa 5) | **33.2** | 0 |
| static level threshold | **20.0** | 0 |
| static fixed-horizon OLS (n=200) | 200.0 | 0 |

- **CS beats fixed-horizon 6x** (33 vs 200). Real and structural: a fixed-horizon rule must wait
  for its declared n; the CS stops when the evidence is decisive. This is the anytime-validity
  win, and it is what the whole construction is for.
- **CS LOSES to static-level on speed** (33 vs 20), and that must be reported, not spun.

## But static-level's speed is an ARTIFACT, and its FPR does not transfer

**The artifact:** static-level fired at mean n = **20.0**, which is EXACTLY its warmup constant.
It fires at the first index it is permitted to. Identical in kind to peak-rate's
`MinObservations + 1 = 21` from epoch 4 -- a gate constant reported as a detection speed.

**The transfer failure**, which is the substantive finding. Both rules calibrated at noise 0.30
and base-backlog shift 0, then measured WITHOUT retuning:

| condition | CS FPR | static-level FPR |
|---|---|---|
| noise 0.30 (calibration point) | 0.000 | 0.015 |
| noise 0.60 | **0.000** | **0.935** |
| noise 1.00 | **0.050** | **1.000** |
| base backlog +0.5 | **0.000** | **0.745** |
| base backlog +1.0 | **0.000** | **1.000** |

**The static rule collapses to near-total false alarm the moment the deployment differs from
where it was tuned.** The CS holds its budget across a 3.3x noise range and a full unit of
intercept shift.

The mechanism is not subtle: a level rule compares a statistic to an ABSOLUTE threshold, so its
operating point is a property of the deployment. The CS compares an EXPONENT to the criticality
value 0.5, which is a property of the queueing physics -- dimensionless, and identical on every
deployment.

## So the defensible claim is precise, and narrower than "faster"

> **At a matched false-alarm rate, the confidence-sequence detector decides 6x sooner than a
> fixed-horizon rule, and unlike a calibrated level threshold it retains its false-alarm
> guarantee when the noise scale changes by 3x or the base load shifts -- because its decision
> boundary is the criticality exponent 0.5 rather than a deployment-specific magnitude.**

That is a claim about CALIBRATION-FREEDOM and TRANSFER, evidenced at matched FPR, with the one
place it loses stated plainly. It is not "beats everything on speed", which would be false.

## Why this is not reward hacking

Three checks, because the user asked specifically:

1. **The comparison is at matched FPR**, calibrated on boundary paths before any speed number was
   read. The level rule's threshold was searched upward until its FPR fit the budget -- it was
   given its best honest setting, not a strawman.
2. **The losing result is reported.** Static-level is faster at the calibration point, and its
   speed advantage is explained (a warmup artifact) rather than hidden.
3. **The transfer test was not chosen after seeing the answer.** It follows directly from the
   design's stated rationale -- "a shape, not a level, so it needs no per-deployment tuning"
   (§3 of the design doc). The test is the pre-existing claim, measured.

## What still must be shown on the real simulator

These are simulated log-log paths with a KNOWN exponent, which isolates the estimator. They do
NOT establish behaviour on BLIS traffic, where the exponent is not known, arrivals are not
Gaussian, and the incumbents are composite / peak-rate / threshold / backlog-drift rather than
textbook rules. That is what the campaign is for, and the transfer result above is the
hypothesis it should test -- across the burstiness ladder and the held-out weibull level, which
epoch 6 already showed does not behave like the fitted four.
