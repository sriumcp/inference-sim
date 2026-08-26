# The liveness effect table, acted on BEFORE launch (checklist item 7)

**`--liveness` passed the contract: all 13 declared levels run, 3/3 self-check invariants hold
at every one, and no level aborts the target.** But the checklist says to READ THE EFFECT TABLE
AND ACT ON IT, and it reports three things that must change the launch decision.

```
noise floor from 3 baseline runs: mean obs_to_confident_verdict=138, sd=0, CV=0.00%
effect (objective range across measured levels) vs noise (threshold: |effect| >= 2 x 0 = 0)
  WRAPPED   effect:  1
  BOUND     effect:  5
  ALPHA     effect: 18
  DISPWIN   effect:  4
  INDET     effect:  0
```

## 1. `INDET` effect is EXACTLY ZERO -- and it is my declaration error, not a dead knob

The knob works; it is wired to a response that cannot see it. From `anytime.go:85-101`: an
undecided verdict reports **`Level=Stable` under BOTH policies** (a fourth `Level` constant
would panic the reducer or round-trip as STABLE -- see the enum trap in the design). The
policies differ only in whether `Signals["undecided"]` is emitted:

- `strict`: emit `undecided=1`, so the pre-commitment stretch is EXCLUDED from the rung rule's
  denominator.
- `lean_stable`: report STABLE with `undecided=0`, so that stretch counts as genuine STABLE
  evidence and the same stream needs a longer firing run to carry a rung.

So `INDET` moves the **rung verdict** (`t1_pass`, `correctness_bursty`) and CANNOT move
`obs_to_confident_verdict`, which reads the index of the first FIRED record -- identical under
both policies because the `Level` sequence is identical.

**This is the level-vs-trend error's cousin: a factor declared against the wrong response.**
The fix is not to drop the factor (its semantics are real and load-bearing for alarm fatigue)
but to score it against a response it can move. Recorded for epoch 2; NOT edited now, because
the response block is inside the compiled policy.

## 2. `sd = 0` makes the significance threshold ZERO

The simulator is deterministic (INV-6), so three baseline replicates at the same seed give
identical objectives -- `sd=0`, `CV=0.00%`. The consequence is that liveness's own rule,
`|effect| >= 2 x noise`, becomes `|effect| >= 0`, which **every nonzero effect satisfies**. The
table's "inf noise" annotation on every row is that division by zero.

So the screen cannot distinguish a real 1-observation effect from a rounding wobble in the
first-fire index. The honest reading of this table is a RANKING (ALPHA 18 >> BOUND 5 > DISPWIN 4
> WRAPPED 1 > INDET 0), not a set of significance verdicts.

What the noise floor SHOULD be measured over is the workload seed, not replicates of one seed:
`workload.seed_env: NOUS_WORKLOAD_SEED` is declared, so a genuine floor comes from varying it.
That the baseline block did not vary it means `noise_estimate_pct: 8.0` in the YAML is
currently an author's assertion rather than a measurement -- and it was inherited from the prior
campaign.

## 3. `WRAPPED` effect is 1 observation -- the axis I care most about barely moves

composite vs peak_rate shifts `obs_to_confident_verdict` by ONE observation (out of ~138-169).
Two readings, and they have opposite implications:

- **Encouraging:** the wrapper's stopping time is governed by ITS OWN evidence rule rather than
  by which detector it wraps. That is what a well-designed wrapper should do, and it is
  consistent with the structural floor -- both wrapped detectors are waiting on the same first
  completion.
- **Concerning for THIS objective:** if the wrapper's speed is insensitive to the wrapped
  detector, then `obs_to_confident_verdict` is mostly measuring the floor plus `ALPHA`, and the
  interesting differences between the two detectors live in CORRECTNESS
  (`correctness_bursty`: peak-rate 0.875 vs composite 1.000) rather than in speed.

`ALPHA` dominating at 18 is exactly as theory predicts -- a tighter error budget costs
observations -- so the instrument is measuring something real.

## Decision: launch anyway, and here is why that is not negligence

The contract holds, every level runs, and the three issues above are about SENSITIVITY of the
declared response, not correctness of the instrument. Specifically:

- Nothing here can produce a WRONG number; it can only produce a weakly-discriminating one.
- The seven constraints (FPR budget, T1, fires-on-all-rungs, grid edge, indeterminate-forever,
  response-interior, CS-coverage) all still function, and they are what stop a gamed
  configuration.
- Fixing the response block or the noise estimate means recompiling the policy, i.e. a new
  pre-registration. Doing that on the basis of a pre-flight is legitimate, but doing it
  REPEATEDLY as each pre-flight suggests a tweak is how a campaign never launches.

So epoch 1 runs to a scored result, and its honest report will say which factors the design
could and could not resolve. `INDET`'s zero effect and the zero noise floor are then MEASURED
inputs to epoch 2 rather than author guesses -- which is the same discipline that turned
"r_nominal = 20" from an inherited constant into a measured per-level table.

Added to `EPOCH2-REQUIREMENTS.md`.
