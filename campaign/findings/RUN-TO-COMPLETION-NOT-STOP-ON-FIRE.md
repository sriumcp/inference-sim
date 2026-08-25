# Let the detector run to completion; do NOT stop it at first fire

**Raised by the user: should the anytime detector stop as soon as it fires (as production
would) or continue, so its metamorphic behaviour can also be measured?**

Answer: **run to completion, and measure BOTH on the same trace.** Production would indeed
stop or alert on first fire, but stopping the MEASUREMENT there would destroy the campaign's
ability to falsify the detector.

## The two quantities answer different questions

| quantity | what it is a property of | measured how |
|---|---|---|
| `obs_to_confident_verdict` (speed) | the FIRST commitment | index of the first fired record |
| T1 / T4 / FPR (correctness) | the WHOLE trace | every record after the first |

Speed is a prefix property; correctness is a suffix property. A run that halts at the first
fire has no suffix, so:

- **T4 (temporal consistency) becomes unfalsifiable.** Flip count is transitions from fired
  to not-fired. Stop at the first fire and the count is **zero by construction**, for every
  configuration, good or bad.
- **The FPR budget becomes unmeasurable on the calibration band.** FPR is "did it fire on
  known-stable traffic", which requires observing that it *keeps* not firing.

`GUARDS-CAUGHT-IT.md` names this exact shape: §3.4's "most useless detector imaginable"
passes T1/T2/T3 trivially and scores "a flawless zero flips on T4". Stopping at first fire
hands every configuration that flawless zero for free. Two of the campaign's four inherited
anti-gaming constraints would go inert.

## And it costs nothing to keep both

Stopping-time is a PREFIX of the full trace, so running to completion loses no speed
information: the first-fire index is identical either way. Halting early is strictly less
information for zero saving (the run is already paid for -- the adapter caps at the frozen
observation_cap regardless).

So the campaign measures the full trace, and `obs_to_confident_verdict` reads the first-fire
index out of it. Production stopping behaviour is a DEPLOYMENT choice that this measurement
informs rather than one the measurement must imitate.

## The real issue the question surfaced: latching makes T4 near-vacuous anyway

The build chose to LATCH (`anytime.go:103-118`): "once either side commits, the verdict is
held until the interval commits the other way", making this "a post-hoc verdict on a finished
run ('did this run saturate?') ... rather than a live 'is it saturated right now?'".

That is defensible -- it is the same question peak-rate's all-time high-water mark answers,
and it keeps the calibration knob live (a wrapper verdict coming only from its own CS would
have an INERT knob, and an unmovable detector cannot be placed on a matched FPR at all,
which is #1614's whole point).

But it has a consequence the build did not state: **a latching detector's flip count is
structurally near-zero, so T4 measures THE LATCH rather than the statistic.** The guard is
not gamed here -- it is simply not discriminating, which is the same end state arrived at
honestly.

Two things follow, and both belong in the next epoch rather than a mid-epoch edit:

1. **Report T4 alongside the latch policy, never as independent evidence.** A zero flip count
   from a latching detector is not the same finding as a zero flip count from a free-running
   one. The current epoch's T4 numbers should be reported as "latched, so uninformative"
   rather than as a pass.
2. **A `latch` policy is a candidate FACTOR for epoch 2** (`latch` vs `free_running`), which
   is the only way to learn what the latch is worth. With it as a factor, T4 becomes
   informative on the free-running arm and the comparison prices the latch instead of
   assuming it. That also directly serves the user's production question: the free-running arm
   measures "is it saturated right now?", the latched arm "did this run saturate?", and a
   deployment can then choose knowingly.

`INDET` already distinguishes `strict` from `lean_stable`, but that governs what to report
while UNDECIDED -- an orthogonal axis to whether a COMMITTED verdict may be revoked.
