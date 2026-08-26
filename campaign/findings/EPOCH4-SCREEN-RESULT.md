# Epoch 4 screen: the wrapper works on composite and is INFEASIBLE on peak-rate

**First scored epoch.** 19 rows, 0 failures, 11 complete + 8 infeasible. Policy compiled and
pre-registered before any row ran; `--smoke` and `--liveness` both green beforehand.

## The headline: a perfect separation on WRAPPED

| WRAPPED | feasible | infeasible |
|---|---|---|
| `composite` | **11 / 11** | 0 |
| `peak_rate` | 0 | **8 / 8** |

Every `composite` row satisfied all seven constraints. Every `peak_rate` row violated at least
two. That is not a marginal ranking; it is a categorical outcome, and it answers the campaign's
central question about which detector the anytime wrapper can be built on.

## Why every peak_rate row failed, and it is not the wrapper's fault

The violated constraints, per row:

```
peak_rate a=0.1   obs=104  fpr=0.1  fpr_within_budget, t1_pass
peak_rate a=0.01  obs=144  fpr=0.0  t1_pass, knob_at_grid_edge
peak_rate a=0.01  obs=150  fpr=0.0  t1_pass, knob_at_grid_edge
peak_rate a=0.01  obs=111  fpr=0.1  fpr_within_budget, t1_pass, knob_at_grid_edge, cs_coverage
peak_rate a=0.1   obs=128  fpr=0.0  t1_pass, knob_at_grid_edge
peak_rate a=0.1   obs=108  fpr=0.1  fpr_within_budget, t1_pass
peak_rate a=0.01  obs=120  fpr=0.1  fpr_within_budget, t1_pass, knob_at_grid_edge, cs_coverage
peak_rate a=0.1   obs=129  fpr=0.0  t1_pass, knob_at_grid_edge
```

Two distinct failure modes, and together they are a vise:

- **At the lenient end** (fpr=0.1 against a 0.05 budget): it fires too readily -- 104-120
  observations, FASTER than every feasible composite row, bought with false alarms. `GUARDS-CAUGHT-IT.md`'s
  lesson exactly: a scalar objective would have crowned the 104-observation row as the winner.
- **At the strict end** (fpr=0.0): it stops failing FPR and starts failing `t1_pass` -- it no
  longer fires at the super-capacity rungs at all -- while pinning `knob_at_grid_edge`, meaning
  the operating point it wants lies OUTSIDE the calibration grid.

So there is no setting of peak-rate's threshold, under this wrapper, that both holds the
false-alarm budget and still fires when the server is genuinely saturated. **The vise is the
finding.** It is consistent with the plan stage's diagnosis that `R_t` in absolute
backlog-per-second is effectively an arrival-rate reading, hence deployment-specific -- and
`knob_at_grid_edge` firing repeatedly is the mechanical signature of exactly that.

## ALPHA behaves exactly as the theory predicts

Among the 11 feasible rows, monotone in the error budget:

| alpha | n | mean obs | values |
|---|---|---|---|
| 0.01 | 4 | 149.0 | 140, 148, 149, 159 |
| 0.06 | 3 | 135.0 | 135, 135, 135 |
| 0.10 | 4 | 131.2 | 129, 130, 131, 135 |

A tighter error budget costs observations -- ~18 observations from alpha 0.10 to 0.01, which is
precisely the effect `--liveness` measured pre-launch (ALPHA 18, the dominant axis). The
instrument and the result agree.

`BOUND` is a weak second: `mixture_sprt` 136.8 vs `howard_eb` 139.9 mean observations.

## Everything honest about the numbers

- **All 11 feasible rows sit at 129-159 observations**, comfortably above the ~55-85
  first-completion structural floor the plan stage established. Nothing is claiming to detect
  saturation before evidence exists.
- **`correctness_bursty` = 1.0 on every feasible row.** The wrapper holds composite's perfect
  bursty correctness while committing at ~129 observations instead of composite's own 63... which
  is SLOWER, not faster. See the honest reading below.
- **`calibrated_fpr` = 0.0 on all 11**, against a 0.05 budget. Same overpayment the plan stage
  criticised in bare composite.

## The honest reading: this is not yet a speed win

Bare composite commits at 63 observations (a noise-floor crossing, per `PLAN-REFUTED-THE-BAR.md`).
The wrapper's best feasible row is **129**. So wrapping composite in a confidence sequence made it
roughly **2x SLOWER** on this apparatus, while keeping correctness at 1.0.

That is a legitimate and reportable outcome, and it should not be dressed up. What the wrapper
buys is not speed but *calibration-free-ness* and an explicit undecided state -- and this epoch
did not measure either as a benefit, because the objective was speed. The campaign's declared bar
("beat the fixed-horizon instrument on observations-to-verdict") is NOT met by the wrapper on
composite.

What IS established:

1. The anytime wrapper is CONSTRUCTIBLE and FEASIBLE on composite, satisfying all seven
   constraints including CS coverage, at every alpha in the grid.
2. It is INFEASIBLE on peak-rate for a structural reason (the FPR/T1 vise plus a grid-edge
   operating point), which is a genuine negative result about peak-rate's absolute-scale
   statistic, not about anytime-valid inference.
3. The speed cost of anytime validity on this apparatus is ~2x versus the unwrapped noise-floor
   crossing, and it is monotone in the error budget exactly as theory says.

## What the machinery did unprompted, and did right

- **Detected `WRAPPED` is non-identifiable** (every row at one level was infeasible) and DROPPED
  it from the fitted set rather than aborting -- "a non-identifiable coefficient must not be
  estimated, and discarding the 11 measurable rows to protect it would lose every other
  coefficient too".
- **Excluded the 8 infeasible rows explicitly** rather than letting one NaN poison every
  coefficient silently.
- **Found the aliasing CONSEQUENTIAL**: `BOUND x INDET` aliases with `ALPHA x DISPWIN`, and
  re-attributing the shared estimate "names a different winner", so it fired the pre-registered
  foldover (19 more rows, negating the BOUND column, for a combined 38-row OLS).
- Screen recommendation before foldover:
  `{WRAPPED: composite, BOUND: mixture_sprt, ALPHA: 0.1, DISPWIN: 10000000, INDET: lean_stable}`,
  predicted 124.1 observations, argmax over 324 valid candidates.

Note `INDET` appears in that recommendation despite `--liveness` measuring its effect on this
objective as exactly ZERO. That is the aliasing at work, and it is why the foldover matters: an
`INDET` level selected through an aliased interaction is not evidence that `INDET` matters.
