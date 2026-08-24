# The anti-reward-hack constraints caught a gamed configuration on the first rows

**Epoch 2, iter-2 screen, first three rows returned.** Recorded because it is
direct evidence that the campaign's objective cannot be gamed -- which is the
whole basis for trusting whatever it eventually recommends.

## The seductive row

Row 27: `STAT=excursion_rate, SRC=work_backlog, HORIZON=10.0, WARM=30000`.

| metric | value | how it reads on a naive objective |
|---|---|---|
| `regret` | **3.0** | best on the board (next best measured: 260) |
| `t1_pass` | True | passes the rate ladder |
| `t4_pass` / `t4_max_flips` | True / **0** | perfect temporal consistency |
| `lead_time_mult` | **0.7** | "detects" 0.7 nominal-multiples before the cliff |
| `first_fire_mult` | 0.3 | fires very early |

On a single scalar response this configuration WINS OUTRIGHT.

## Why it is worthless

`t1_rungs` shows `True` at **every rung measured** -- 0.3x, 0.4x, 0.5x, 0.6x, 0.7x,
0.9x, 1.0x, 1.1x, 1.25x, 1.5x, 2.0x. It fires unconditionally. Therefore:

- it "passes" T1 trivially (it fires everywhere, so of course it fires above the
  cliff);
- it scores ZERO T4 flips because it never stops firing, so there is nothing to
  flip back from;
- its "lead time" is an artifact of firing at the bottom of the ladder.

`calibrated_fpr = 1.0` against a `target_fpr = 0.05`: it fires on 100% of
known-stable traffic. This is exactly the failure `metamorphic_tests.md` §3.4
names -- "a detector that returns saturated unconditionally passes T1, T2, and T3
trivially, and scores a flawless zero flips on T4. It would top the scorecard while
being the single most useless detector imaginable."

## What caught it

Two of the four declared constraints, independently:

- `fpr_within_budget_num >= 1.0` -> **violated** (`fpr_within_budget: False`)
- `fires_on_all_rungs_num <= 0.0` -> **violated** (`fires_on_all_rungs_num: 1.0`)

Result: `failure_kind: constraint_violated`, `status: infeasible`. Per the kind's
semantics the row is EXCLUDED FROM FITTING but RETAINED in `runs.jsonl` as real
data about the space -- which is the correct treatment: "a config that turns a
feature off entirely and thereby wins trivially is exactly the case this catches."

Row 22 (`excursion_rate`, `in_flight`) is the same story with the same numbers,
confirming it is the STATISTIC that is degenerate, not one odd corner.

## It also confirms a prediction

`STATISTIC-SCREENING.md` predicted `excursion_rate` would lose because with ~1
excursion per run it degenerates into measuring elapsed time rather than
excursions, and it was deliberately RETAINED as a level so the campaign would
refute it explicitly rather than have the author quietly omit it. Measured
standalone at n=6000 it returns ~1007 and ~976 -- essentially the run duration in
seconds. The campaign has now refuted it on its own evidence, which is worth more
than the exclusion would have been.

## The honest contrast

Row 15 (`peak_ratio_stability`, `work_backlog`, H=10.0, W=30000) is a genuine
failure rather than a gamed one: `t1_pass=0` (never fires at super-capacity) and
5 T4 flips, `regret=260`. Both kinds of failure are being detected and separated.
