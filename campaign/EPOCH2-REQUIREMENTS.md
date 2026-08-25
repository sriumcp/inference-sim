# What epoch 2 must change, and why each item is an EPOCH boundary rather than an edit

**Written during epoch 1's re-launch pre-flight, while the evidence is fresh.** Every item
below is either apparatus or inside the compiled policy, so none of them can be applied to a
measuring epoch -- that is the rule three prior epochs died to.

Epoch 1 status: STOPPED before any row scored (3 of 19 rows died on `KeyError: 'anytime'`,
level-independently). Its policy hash was `97557929819c`. The instrument defects are fixed and
the epoch is being re-launched, so the next MEASURING epoch inherits a working adapter.

## 1. `latch` vs `free_running` should be a FACTOR (highest value)

**Evidence:** the build chose to LATCH (`anytime.go:103-118`) -- "once either side commits, the
verdict is held until the interval commits the other way". Defensible: it keeps the calibration
knob live, and an unmovable detector cannot be placed on a matched FPR at all (#1614's point).

**But a latching detector's T4 flip count is structurally near-zero, so T4 measures THE LATCH
rather than the statistic.** The guard is not gamed; it simply stops discriminating. Reporting
a zero flip count from a latched detector as a T4 pass would be the honest-arrival version of
`GUARDS-CAUGHT-IT.md`'s useless-detector signature.

Making it a factor is the only way to PRICE the latch instead of assuming it, and it directly
answers the production question the user raised (stop-on-fire vs continue): the free-running arm
measures "is it saturated RIGHT NOW?", the latched arm "did this run saturate?". A deployment
then chooses knowingly.

Note `INDET` (`strict` / `lean_stable`) is a different axis -- it governs what to report while
UNDECIDED, not whether a COMMITTED verdict may be revoked.

## 2. Concurrency: raise the OUTER width only

**Measured under real screen load** (`findings/CONCURRENCY-MEASURED.md`): declared 3x2=6,
observed exactly 6; our six processes total 267.9% CPU (~2.7 of 10 cores), each getting ~45% of
a core with none starved. Load 10.86 is mostly co-tenant system agents (CrowdStrike 44.5%,
WindowServer 42.9%, Lakeside 33%, airportd 32%), not us.

**Recommendation: `max_parallel: 4`, `--adapter-width 2` (~8 procs).** Measured adapter fan-out
is IDENTICAL at widths 1 and 2 (2 procs) and 4 at width 4, so the inner width does not scale
between 1 and 2 -- raising it buys nothing. Do NOT go to 4x4=16: the remaining cores are
contested rather than idle, and 16 approaches the 24-process configuration that produced load
122 and destroyed an epoch through level-correlated bias.

`max_parallel` is inside the compiled policy, hence an epoch boundary.

## 3. Arm oracle 2(c) properly

Epoch 1 ran with the oracle NOT armed: the control configuration could not be measured before
`anytime.go` existed, so nothing in the machinery checked the mechanism is inert at its OFF
setting -- and as `verify` put it, "a mechanism that shifts the metric at its OFF setting would
confound every effect this epoch measures".

It was satisfied BY HAND (`probes/verify_control_inert.sh`: byte-identical stdout against a
binary built from PR #1620's head, on a default run and with each of the four incumbents
selected). That is sound evidence but it is outside the machinery.

**To arm it:** make `run_command` able to execute the control configuration BEFORE the build
runs -- i.e. have `--incumbent-only` be the control level's behaviour rather than a separate
flag, so `{'WRAPPED': 'composite'}` is runnable pre-build. Then the oracle measures it
automatically. Requires a fresh campaign per the warning's own instruction.

## 4. Reconsider `run_timeout_sec` against the measured row cost

Measured: **372.5s per row** at full resolution, width 2 (from `--smoke`). The declared ceiling
is 3000s -- an 8x margin. The guide warns a generous ceiling is not free: a hung target burns
the WHOLE ceiling on every affected row before failing it. With 372s measured, **1200s** is
still a 3.2x margin and fails a hung row 2.5x sooner.

## 5. Carry forward, unchanged

- The frozen burstiness ladder and its four MEASURED cliffs (constant 96, poisson 88,
  gamma_cv2 88, gamma_cv4 80; held-out weibull_cv3 96). `r_nominal` is a property of the
  (workload, hardware) PAIR -- never inherit it across a workload change.
- Drive the CS width from the MEASURED dispersion index, never a declared CV: capacity is not
  monotone in CV across arrival families (weibull CV=3 ties `constant`) while measured I IS
  monotone (0.002 / 1.08 / 2.08 / 8.49 / 9.40).
- Do not use the `poisson` vs `gamma_cv2` pair as a fine-grained burstiness contrast: their
  cliffs tie at 88 and on the continuous growth statistic the ordering inverts. Use `constant`
  vs `gamma_cv4`.
- The structural floor: no completion-aware statistic can honestly commit before the first
  completion (measured index 55/79/85 on super rungs). Any config that appears to beat it is
  reading arrival rate, not saturation.
- Run to completion; never halt the MEASUREMENT at first fire
  (`findings/RUN-TO-COMPLETION-NOT-STOP-ON-FIRE.md`).

## 6. Launch discipline that must not regress

- Run `nous run` from the NOUSKO repo with an absolute campaign path. `prompts.methodology_layer`
  resolves relative to it, and a missing preamble is only a WARNING (`build.py:545`) -- so
  launching elsewhere silently strips the campaign-beats-target-docs hierarchy and voids
  auto-approve precondition 3.
- Stage `at.yaml` from its template first: a `config_patch` never creates structure, and all
  five factors patch into that file.
- Rebuild `blis` before measuring. `assert_blis_fresh()` now enforces this, after a 2h-stale
  binary made every anytime row fail with a message that read like a missing struct field.
- Run BOTH `--smoke` AND `--liveness`. `--smoke` alone has now missed one campaign-killing
  defect in this work (the anytime knob grid, which `--incumbent-only` never reached).
