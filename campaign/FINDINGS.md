# Anytime-valid saturation detection: campaign findings

**Campaign:** `anytime-valid-detector-e4`, `kind: optimization`, auto-approved.
**Outcome:** the epoch ended on a **SEMANTIC EXCEPTION**, `certified: False`, with
`accounting: "none: semantic exception ends the epoch, no inference is drawn"`.
**So there is no certified optimum.** Everything below is either a measured observation or an
explicitly-labelled uncertified estimate.

Read `epoch_end-1.json` as authoritative, not the LLM gate narration: the narration described a
normal advance to a refine stage, while the machine record says the epoch ENDED and no inference
was drawn.

---

## 1. What was measured: 41 rows, 0 crashes

| block | rows | complete | infeasible |
|---|---|---|---|
| screen (iter-2) | 19 | 11 | 8 |
| foldover (iter-3) | 19 | 11 | 8 |
| relations gate | 14 tests | 14/14 matched, 0 failures | — |

Every row ran. `infeasible` is information, not failure: those configurations produced
trustworthy numbers and violated a declared constraint.

## 2. THE MAIN RESULT: a perfect separation, and it is a negative one for peak-rate

| WRAPPED | feasible | infeasible |
|---|---|---|
| `composite` | **11/11** (22/22 across both blocks) | 0 |
| `peak_rate` | 0 | **8/8** (16/16) |

**peak-rate cannot be made feasible under this wrapper, and it fails in a vise:**

- *Lenient end:* fires in 104-120 observations -- FASTER than every feasible composite row -- but
  at FPR 0.10 against a 0.05 budget. `fpr_within_budget_num` and `t1_pass_num` reject it
  independently. A scalar objective would have crowned the 104-observation row as the winner.
- *Strict end:* holds FPR 0.00 but stops firing at the super-capacity rungs at all (`t1_pass`
  fails) while pinning `knob_at_grid_edge` -- its wanted operating point lies OUTSIDE the
  calibration grid.

No threshold setting both holds the false-alarm budget and still fires when the server is
genuinely saturated. This corroborates the `plan` stage's independent diagnosis that `R_t` in
absolute backlog-per-second is effectively an arrival-rate reading and therefore
deployment-specific; the repeated `knob_at_grid_edge` violation is that diagnosis's mechanical
signature.

**This is the campaign's most defensible finding**, because it is categorical (16/16) and
because two independent constraints produced it.

## 3. THE WRAPPER IS SLOWER THAN WHAT IT WRAPS

Bare composite commits at **63** observations. The wrapper's best feasible row is **129**.

**So anytime-valid inference cost ~2x in stopping time on this apparatus, and the campaign's
declared bar -- "beat the fixed-horizon instrument on observations-to-verdict" -- is NOT met.**

`correctness_bursty` was 1.0 on every feasible row and `calibrated_fpr` was 0.0 on all of them,
so nothing was lost in correctness; the cost was purely in speed. What the wrapper buys
(calibration-free operation, an explicit undecided state) this objective did not measure as a
benefit. Reported as a negative result, not reframed.

Caveat that makes this fairer to the wrapper than it looks: composite's 63 is itself an artifact
(the index where `8/sqrt(arrivals)` crosses 1.0), and peak-rate's oft-quoted 21 is
`MinObservations+1` -- see `PLAN-REFUTED-THE-BAR.md`. The honest floor is the first-completion
index (55-85 on super rungs). Against THAT floor, 129 is roughly 1.6-2.3x, not 2x a real detector.

## 4. ALPHA is the dominant knob, exactly as theory predicts

Feasible rows, mean observations-to-verdict:

| alpha | n | mean obs |
|---|---|---|
| 0.01 | 4 | 149.0 |
| 0.06 | 3 | 135.0 |
| 0.10 | 4 | 131.2 |

Monotone, ~18 observations across the range. `--liveness` measured this pre-launch as ALPHA 18 --
the instrument and the result agree, which is the strongest available evidence that the apparatus
measures what it claims. Screening main effects: **ALPHA -8.875**, BOUND -3.375, DISPWIN -2.875,
INDET -0.125 (ALPHA ~2.6x the next factor, ~70x INDET).

`BOUND` is a weak second: `mixture_sprt` 136.8 vs `howard_eb` 139.9 mean observations.

## 5. THE BURSTINESS PREDICTION WAS NEVER TESTED -- an instrument gap, and it is mine

The campaign's pre-registered central prediction was: *observations-to-confident-verdict grows
monotonically with the MEASURED dispersion index `I`.*

The dispersion index was measured per level, and it IS monotone:

| level | measured I |
|---|---|
| constant | 0.004 |
| poisson | 0.791 |
| gamma_cv2 | 2.019 |
| weibull_cv3 (held out) | 4.924 |
| gamma_cv4 | 8.822 |

**But the adapter emits `obs_to_verdict` for POISSON ONLY** (`score_anytime.py:928-929` emit
`obs_to_verdict_poisson` and `obs_to_verdict_weibull_heldout`; the held-out value was absent from
every completed row, and `constant` / `gamma_cv2` / `gamma_cv4` are never emitted at all). So the
response the prediction is about exists at one point, and **the prediction is neither confirmed
nor refuted -- it is unmeasured.**

That is a defect in my Task 3 adapter: I wired the `regimes` block for a smooth-vs-bursty
conjunction but never emitted the per-level series the monotonicity claim requires. It is the
single most important thing to fix before the next epoch, because it is the campaign's actual
research question.

## 6. WHY THE EPOCH ENDED: the foldover could not be formed

`epoch_end-1.json`: `reason: foldover: {"nan_response": true}`.

The chain, from the log:

> *"iter-2's screen block has 8 rows with no usable measurement, so the combined fit cannot be
> formed over the design whose aliasing this block was spent to resolve."*

The screen found `BOUND x INDET` aliased with `ALPHA x DISPWIN`, and re-attributing the shared
estimate **named a different winner** -- so the aliasing was CONSEQUENTIAL and the policy fired
its pre-registered foldover. But the 8 infeasible `peak_rate` rows are the same 8 in both blocks
(foldover reproduced them exactly: 8 infeasible / 11 complete), so negating the `BOUND` column
cannot repair a design with a hole at one `WRAPPED` level. The foldover spent 19 rows on a
resolution it could not achieve, and the policy correctly ended the epoch rather than fit a model
over the hole.

**The lesson: a factor whose levels are categorically infeasible does not merely lose its own
coefficient -- it can break a later stage's ability to resolve aliasing among the OTHER factors.**

The machinery handled this well and unprompted: it detected `WRAPPED` as non-identifiable and
dropped it from the fitted set rather than aborting ("a non-identifiable coefficient must not be
estimated, and discarding the 11 measurable rows to protect it would lose every other coefficient
too"), excluded the infeasible rows explicitly rather than NaN-poisoning every coefficient, and
ended the epoch on a NAMED policy branch instead of inventing one.

## 7. Uncertified estimate, recorded as such

Final recommendation, basis `measured`, **`certified=False`**, `R_model=None`, `R_terminal=None`:

```
WRAPPED=composite  BOUND=mixture_sprt  ALPHA=0.1  DISPWIN=10000000  INDET=strict
predicted obs_to_confident_verdict = 127.5   (epsilon = 6.37)
```

Two reasons not to act on it:

1. There were **no replicated center points**, so there is no pure-error estimate and no
   confidence interval on any effect. All four main effects are point estimates with `se=null`.
   The campaign's own gate says all four hypotheses are `PARTIALLY_CONFIRMED` -- directionally
   measured, statistically unverified.
2. `INDET` appears in the recommendation despite `--liveness` measuring its effect on this
   objective as **exactly zero**, and despite the mechanism making that zero structural (an
   undecided verdict reports `Level=Stable` under BOTH policies, so the first-fired index is
   identical -- see `LIVENESS-EFFECT-TABLE.md`). Its presence is the unresolved aliasing, not
   evidence that `INDET` matters.

## 8. Verified constraints and isolation

- **All seven anti-gaming constraints functioned.** Two of them independently rejected the
  fastest configuration on the board (peak-rate at 104 observations, FPR 0.10).
- **Oracle 2(c) was NOT armed** by the machinery (the control config could not be measured before
  `anytime.go` existed). Satisfied BY HAND: `probes/verify_control_inert.sh` shows byte-identical
  stdout against a binary built from the apparatus base commit, on a default run and with each of
  the four incumbents selected. So "the mechanism shifts the metric at its OFF setting" is ruled
  out by measurement.
- **Isolation held throughout.** Zero lines changed in `peak_rate.go`, `composite.go`,
  `backlog_drift.go` versus the apparatus base commit `6959f64a`; every one of this branch's
  commits touches only `campaign/` plus the three build-authored files. PR #1620 advanced twice
  during the campaign (`6959f64a -> f5740cd1`) and was never written to.

## 9. Epoch ledger -- four epochs, four distinct apparatus defects, zero wrong numbers

| epoch | policy | ended because |
|---|---|---|
| e1 | `97557929819c` | adapter's anytime knob grid was a COMMENT describing a function nobody wrote; 3/19 rows died on `KeyError` |
| e2 | -- | `plan` was re-running non-idempotently; would have measured a mechanism against a different design doc |
| e3 | `1dee5d7d79cc` | both `design_space` invariants malformed (a literal `"baseline"` placeholder, and a check pinned to a MUTABLE branch that advanced mid-run); all 12 rows rejected |
| e4 | `f0cc79376ba9` | **scored**; semantic exception at foldover (`nan_response`) |

None of the four produced a WRONG number. Each was caught by a guard doing its job -- including
two of my own guards firing correctly on my own errors, and the `--liveness`/`--smoke` pair
catching three defects before launch.

## 10. What to change for the next epoch

Ordered by value. Full detail in `EPOCH2-REQUIREMENTS.md`.

1. **Emit `obs_to_verdict_<level>` for all five burstiness levels.** Without this the campaign's
   central research question is unmeasurable. Highest priority by a wide margin.
2. **Add replicated center points** so effects get a pure-error estimate and real confidence
   intervals. Without them no effect can be called significant.
3. **Drop `peak_rate` from `WRAPPED`, or give it a scale-free threshold.** 16/16 infeasibility is
   a settled answer; keeping the level as-is costs 8 rows per block AND breaks the foldover.
4. **Score `INDET` against a response it can move** (`t1_pass` / `correctness_bursty`, or an
   explicit `undecided_fraction`). Its zero effect on the current primary is structural.
5. **Make `latch` vs `free_running` a factor** -- the latch currently makes T4 near-vacuous, and
   this is also the honest way to answer the production stop-on-fire question.
6. **Measure the noise floor over the workload seed**, not replicates of one seed on a
   deterministic simulator (`sd=0` collapsed liveness's significance rule to `|effect| >= 0`).
7. **Raise `max_parallel` to 4, keep `--adapter-width 2`**; cut `run_timeout_sec` from 3000s
   toward 1200s now that a full-resolution row measures ~420s.
