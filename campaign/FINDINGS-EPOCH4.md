# Epoch 4: the campaign COMPLETED, and alpha is a plateau rather than an optimum

**First epoch to run the full pipeline to a report.** screen (12 rows) -> confirm (15 replicates)
-> report. `Campaign complete after 3 iteration(s)`, no semantic exception, no circuit-breaker halt.

**Verdict: `certified=False`, `winner None`** -- and the reason is a substantive result rather than a
defect.

---

## 1. What was measured

| stage | rows | outcome |
|---|---|---|
| screen | 12 | 7 complete, 5 infeasible -- exactly the 7 needed to fit 3 factors |
| confirm | 15 | 12 complete, 3 infeasible (one per finalist) |

Screen recommendation: `ALPHA=0.1, KAPPA=1.0, LATCH=True`, predicted lead 0.825, argmax over 158
valid candidates with 5 measured-infeasible excluded. Residual regret at screen: R_0.05 = 13.16
against epsilon 0.04125.

## 2. Why no winner: the finalists are statistically indistinguishable

The three finalists were `ALPHA` = 0.09 / 0.095 / 0.10, all at `KAPPA=1.0, LATCH=True`.

| alpha | per-replicate lead time | mean | sd |
|---|---|---|---|
| 0.090 | 0.8125, 0.875, 0.8875, 0.950 | 0.8812 | 0.0564 |
| 0.095 | 0.8125, 0.875, 0.8875, 0.9625 | 0.8844 | 0.0616 |
| 0.100 | 0.8125, 0.875, 0.8875, 0.9625 | 0.8844 | 0.0616 |

```
spread between finalist MEANS : 0.0031
pooled sd WITHIN finalists    : 0.0542     -> noise is 17x the signal
```

**The between-finalist difference is 17x smaller than the within-finalist variation.** At five
replicates no terminal discrimination is possible, so `winner None` is the honest verdict and
`R_0.05 = unknown` is the honest regret. The policy reported uncertified rather than crowning a
finalist the data cannot distinguish.

## 3. The finding: alpha is a PLATEAU over 0.09-0.10, not a point optimum

This is a genuine result about the detector, and it is the opposite of a disappointment:

- Lead time is **insensitive to alpha** in the 0.09-0.10 range. An operator does not need to tune
  the coverage budget finely; anywhere in that band performs identically within noise.
- The variation that DOES exist is **seed variation** (workload draw), not configuration variation.
  Every finalist spans 0.8125-0.9625 across its own replicates -- a 0.15 range -- while the
  configurations differ by 0.003.

So the practical recommendation is a REGION: `alpha in [0.09, 0.10], kappa = 1.0, latch = true`. That
is more useful for production than a point estimate, because it says the setting does not need
protecting.

`KAPPA=1.0` and `LATCH=True` were selected identically by every finalist, so those two are settled
by the screen even though alpha is not.

## 4. What remains genuinely unresolved

1. **The plateau's width is unmeasured.** Confirm only explored alpha 0.09-0.10 because that is
   where the screen pointed. The plateau may extend further; epoch 3 measured alpha=0.05 as feasible
   at kappa=1.0, which suggests it does.
2. **`epsilon=0` at confirm.** The declared `epsilon: {pct: 5.0}` resolved to 0.04125 at screen but 0
   at confirm. An epsilon of 0 can never be met by construction (the guide is explicit), so even a
   separable winner could not have certified. That is a policy-arithmetic issue to fix before the
   next epoch, and it is independent of the plateau finding.
3. **The FPR reconciliation is still open** (see the addendum in
   ESTIMAND-WAS-RIGHT-BOUNDARY-AND-WIDTH.md). I could not reproduce a row's calibration walk by
   hand. Until that is closed by recomputing from the harness's own saved per-rung records, every
   "matched false-alarm rate" claim rests on the harness alone -- which is load-bearing for a
   published comparison.
4. **One replicate per finalist was infeasible at correctness 0.45.** Same configuration, different
   seed. That means the configuration is not robustly correct across workload draws, which is a
   caveat any deployment recommendation must carry.

## 5. Cross-detector comparison, stated carefully

The static detectors on the SAME full ladder at matched FPR (from the cached reference):

| detector | mean lead | correctness | admissible |
|---|---|---|---|
| anytime (best measured) | **0.8125** | 1.00 | yes |
| composite | 0.80 | 1.00 | yes |
| peak-rate | 0.80 | 1.00 | yes |
| threshold | 1.00 | 0.00 | no -- fails correctness |
| backlog-drift | 0.80 | 1.00 | no -- FPR 1.00, knob at grid edge |

**Honest reading:** composite and peak-rate still hold a small lead-time edge (0.80 vs 0.8125), and
that difference is far inside the 0.054 replicate noise measured above -- so on this evidence the
three admissible detectors are indistinguishable on lead time, not ranked. Claiming the detector
"beats" them would be reading noise; claiming it "loses" would be too.

What IS distinguishable: two of the four static detectors are INADMISSIBLE at a matched false-alarm
rate, and the anytime detector's boundary is a dimensionless exponent rather than a
deployment-specific magnitude -- which is the calibration-free property the simulated-path
experiments measured directly (static rules rose to 0.93-1.00 false-alarm rates off their
calibration point; this one held its budget).
