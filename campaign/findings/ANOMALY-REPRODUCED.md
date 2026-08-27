# The 1-in-5 failure REPRODUCED exactly, so it is a real property -- and I was wrong to downgrade it

**Epoch 5 confirm, independent run, fresh caches, `elapsed_sec` instrumentation added:**

```
15 replicates -> 12 complete, 3 infeasible
infeasible knobs: [0.7]
  ALPHA=0.100  corr=0.45  knob=0.7  fpr=0.05
  ALPHA=0.090  corr=0.45  knob=0.7  fpr=0.05
  ALPHA=0.095  corr=0.45  knob=0.7  fpr=0.05
```

**Byte-identical to epoch 4**: same 3-of-15, same shared replicate slot, same boundary 0.7, same
correctness 0.45, same FPR 0.05 across all three finalists.

## What that changes

I downgraded this to an "open anomaly" after failing to reproduce the mechanism by hand, on the
grounds that a claim resting on 3 of 15 replicates with no explanation should not be reported as an
established weakness. **The downgrade was wrong** -- not because the reasoning was bad, but because I
tested reproducibility the wrong way: I tried to recreate it from outside the harness at seed offsets
0, 1 and 2, and it lives at an offset the confirm stage uses and I did not sample.

Reproduced independently, it is a **real and deterministic property of the detector**, and it should
be reported as one.

## The mechanism, now unambiguous

The boundary calibrates to **0.7** on one workload draw. Measured overload gamma is ~0.468 (five
seeds, sd 0.052), so at a boundary of 0.7 **the interval can never clear it** -- the detector cannot
fire on super-capacity rungs at all, and correctness collapses to 0.45.

Why calibration lands at 0.7 on that draw is the remaining question, and it is now answerable rather
than speculative: the simulator is deterministic, so the draw can be located and its calibration walk
read directly. That hunt is running.

Note what is NOT the cause -- three hypotheses I refuted and which remain refuted:

1. early-admission transient (trimming 50% of early samples barely moves gamma)
2. elapsed-time as the regressor (it separates BETTER than the index, and gamma's seed sd is 0.06
   against a 0.34 signal)
3. an over-tight calibration rule (`fpr <= 0.05` correctly accepts one firing rung of twenty)

## Why this is a publishable finding rather than an embarrassment

It is a **quantified failure mode of anytime-valid saturation detection**, with a clean mechanism:

> On roughly one workload draw in five, the false-alarm calibration selects an exponent boundary
> above the achievable overload exponent, at which point the detector is provably blind. The
> guarantee is intact -- it never exceeds its false-alarm budget -- but the operating point is
> useless.

That is exactly the kind of property a paper must state, and it also names the fix precisely:
**the calibration must be constrained to boundaries that remain below the measured overload
exponent.** A boundary that cannot be cleared is not a conservative choice; it is an inadmissible
one, and the calibration walk currently has no way to know that.

## Consequence for the campaign

Both epochs died at confirm for the same structural reason: all three finalists lose the SAME
replicate slot, so the paired comparison under common random numbers has no pair at that slot and
`nan_response` ends the epoch. With the cause now identified as configuration-independent, two fixes
follow, and they are independent of each other:

1. **Detector-side:** reject calibration boundaries above the achievable overload exponent -- the
   real fix, and the one that makes the detector production-safe.
2. **Campaign-side:** more confirm replicates, so one lost slot cannot void the comparison.

The first is the one that matters. The second only stops the symptom.
