# peak_over_elapsed passes the full metamorphic suite at n=6000

**Measured on the frozen apparatus, full suite: 5 seeds x 11 rungs, n=6000,
FPR-calibrated first per §3.4.** This is the result the horizon finding predicted,
now measured end-to-end rather than inferred from a statistic probe.

## The verdict

```
rungs fired: 0.3 F  0.4 F  0.5 F  0.6 F  0.7 F | 0.9 T  1.0 T  1.1 T  1.25 T  1.5 T  2.0 T
```

A clean step function at the capacity cliff: silent across every calibration rung,
fired at every super-capacity rung, transition at 0.9x nominal.

| metric | value | budget / meaning |
|---|---|---|
| `t1_pass` | **True** | fires at every super-capacity rung |
| `t4_pass` / `t4_max_flips` | **True / 0** | no flip-flop under burst-lull traffic |
| `calibrated_fpr` | **0.0** | budget 0.05 -- zero false alarms over 20 stable runs |
| `fires_on_all_rungs_num` | 0 | NOT gaming: genuinely silent below the cliff |
| `knob_at_grid_edge_num` | 0 | knob interior, so the FPR is real, not a grid artifact |
| `fpr_within_budget` | True | admissible |
| `lead_time_mult` | **0.1** | fires 0.1 nominal-multiples BEFORE the cliff |
| `frozen_knob` | 0.5 | the calibrated threshold |
| `regret` | **9.0** | all four constraints satisfied |

## Why this is not the gamed configuration

Epoch 2's row 27 also reported `t1_pass: True` and `t4_pass: True` with zero flips
and a LONGER lead time (0.7). It was infeasible: `calibrated_fpr = 1.0` and it fired
at all 11 rungs -- an always-saturated detector (see GUARDS-CAUGHT-IT.md).

The difference is the whole point of the §3.4 discipline. This configuration fires
at 6 of 11 rungs, is silent on the 5 that define the false-alarm budget, and reaches
FPR 0.0 with its knob strictly inside the grid. Its lead time is SHORTER than the
gamed row's precisely because it is real.

## Against the incumbents

| detector | regret | t1_pass | notes |
|---|---|---|---|
| **peak_over_elapsed** | **9.0** | True | full suite, all guards satisfied |
| composite | 10.0 | True | quick mode (1 seed, 4 rungs) -- the only incumbent that passes |
| threshold | 110.0 | False | quick mode, 2-test suite -- SUPERSEDED, see below |
| backlog-drift | 110.0 | False | INFEASIBLE, and knob pinned at its grid edge |

**SUPERSEDED for threshold.** On the 4-test suite with a calibrated knob, threshold
PASSES all four tests at regret 10.0 -- it is not infeasible. See
`T2-CORRECTION-INCUMBENTS-NOT-BLIND.md`. backlog-drift remains broken (regret 340).

Reported honestly: composite's 10.0 was measured in the weaker quick mode, so the
9.0-vs-10.0 gap is indicative, not a like-for-like comparison. What IS like-for-like
is that threshold and backlog-drift are infeasible under any mode -- they cannot
fire above the cliff even at maximum sensitivity.

## What this settles, and what it does not

**Settles:** the reflected-random-walk idea works for LLM-serving saturation
detection. `R_t = Peak_t/t`, given a long enough horizon, separates sub- from
super-capacity cleanly and passes T1 and T4 at a matched false-alarm rate. The
earlier "no Peak-based configuration discriminates" was an n=500 artifact and is
retracted; the horizon was the defect, not the statistic.

**Does not settle:** whether this is the BEST configuration. It is one point --
`SRC=in_flight`, `WARM=0`, `CONSEC=3`, `MINOBS=20`, `HORIZON=2.0` -- chosen by an
author, not searched. Epoch 3's job is the response surface: whether
`work_backlog` buys more lead time, whether a warm-up sharpens the transition,
whether `peak_ratio_stability` at a better horizon beats the plain level. T2-IN and
T2-OUT (prompt- and output-size ladders) are also not yet in the adapter's scored
suite -- only T1 and T4 are -- so "passes the suite" here means T1 + T4 + the FPR
discipline, and the size ladders remain future work.
