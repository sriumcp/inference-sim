# The `plan` stage refuted both incumbent numbers I set as the bar — and found a tautology
# in the statistic I specified

**Source: `mechanism_plan.json`, epoch `anytime-valid-detector-e1`, verified independently
before acceptance.** This is the `plan` stage doing exactly what the guide's measured
comparison predicted: pricing the mechanism BEFORE authoring it, and refuting the author's
premises in the process.

## 1. Both "incumbent speed" numbers are GATE ARTIFACTS, not measurements

I put these into `factor_nomination` as the bar to beat:

```
peak-rate       21 observations      <- reported as "3x faster than composite"
composite       63 observations
```

Both are artifacts of a constant, and neither measures detection speed.

**peak-rate's 21 = `MinObservations(20) + 1`.** Verified in the target: `peak_rate.go:128`
sets `defaultPeakRateMinObservations = 20`, and `ready()` (line 322) gates on
`observations >= MinObservations`. So the detector fires at the FIRST INDEX ITS OWN
READINESS GATE PERMITS. The plan's decisive evidence: it scores 21 on the 1.5x SUPER rung
**and 21 on the 0.4x STABLE rung** — identical. Only `fired_fraction` distinguishes them
(0.991 vs 0.087). A number identical in a saturated and a healthy regime is not measuring
saturation.

Its statistic at that index (`R_t` = 241 super vs 64 stable, in absolute backlog/sec) is
effectively an arrival-rate reading — which is why it needs per-deployment calibration and
reaches only 0.875 correctness on the bursty ladder.

**composite's 63 = where `8/sqrt(arrivals)` crosses 1.0.** `noise_floor` is 1.0000 at index
63 and 0.9923 at 64, with `rate_deficit == 1.0` throughout. Its FPR of 0.00 against a 0.05
budget is OVERPAYMENT, and the same sensitivity that bought the FPR inflates time-to-fire
QUADRATICALLY (the floor goes as `1/sqrt(n)`, so halving it costs 4x the observations).

**RETRACTED:** my earlier report that "peak-rate commits at 21 vs composite's 63 — a 3x
speed separation" and that this was "the first real speed discrimination since the void
tie". It is not a speed discrimination. It compares a gate constant against a noise-floor
crossing. The clipped-metric fix (`DETECTION-DELAY-IS-CLIPPED.md`) removed one artifact and
this one replaced it, one layer down.

## 2. `gamma_t == 1` is TAUTOLOGICAL before the first completion

The sharper finding, and it is about the statistic **I** chose in the design (§3.1.1):

> Before the first `Completion` event, `in_flight == arrivals` identically, so `Peak == t`
> and `gamma_t = dlogPeak/dlogt == 1` under EVERY regime, healthy or overloaded.

Verified directly: regressing `log Peak` on `log t` over a pre-completion phase where
`Peak_t = t` gives **gamma = 1.000000**, identically, regardless of load.

So in the opening phase the statistic I selected carries **zero information** — and it reads
`1`, which is exactly the OVERLOADED value. A CS on it would not merely be uninformative
early; it would be confidently wrong in the direction of a false alarm.

**This is the deeper reason my "beat 21" bar was unmeetable.** Measured first-completion
indices:

| rung | constant | poisson | gamma_cv4 |
|---|---|---|---|
| super (1.1x) | 55 | 79 | 85 |
| stable (0.4x) | 19 | 28 | 31 |

**No completion-aware statistic can honestly land below ~55 on the super rungs**, which is
where the objective is measured. A design that beat 21 would be reading arrivals, not
saturation — i.e. it would be reproducing peak-rate's artifact.

## Consequence: what "best possible detector" means here

The bar is NOT "commit in under 21 observations". It is:

1. commit as close to the structural floor (~55-85, regime-dependent) as the evidence
   permits, and
2. hold correctness on the bursty ladder at or above 0.9, where peak-rate manages 0.875, and
3. not overpay FPR the way composite does (0.00 against a 0.05 budget), since that
   overpayment is what makes it quadratically slow.

The wrapper's honest claim is therefore about **stopping when the evidence is decisive
instead of at a fixed horizon**, and about being CALIBRATION-FREE where peak-rate's
`threshold` is deployment-specific — not about beating a gate constant.

## Method note

The plan states it MEASURED these rather than inferring them, and every claim above was
re-verified here from the target source and from arithmetic before being accepted. That
matters given this campaign's history: five prior defects were all measurements that could
not fail in the direction being tested, and an unverified plan claim would have been the
sixth. Two of the plan's numbers I could confirm exactly (the `MinObservations` constant,
the gamma tautology); the first-completion indices and the noise-floor crossing I take as
reported, flagged here as plan-sourced rather than independently reproduced.
