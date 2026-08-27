# The seed sensitivity: three hypotheses of mine, all refuted, and what is actually true

**Investigating why one confirm replicate in five defeated every configuration (correctness 0.45,
knob 0.7).** I formed three explanations and measurement killed all three. Recording them because
the refutations are more useful than the hypotheses were, and because two of them I had already
half-committed to in earlier writeups.

## Hypothesis 1: early admission contaminates the fit with transient high-gamma

The reasoning: admission is the first Completion, and immediately after it the peak still tracks
`t`, so gamma reads near 1. Seed 45 admits at n=24 while seed 44 admits at n=50, so seed 45
captures more of the transient and its fitted slope is pulled up.

**Refuted.** Refitting the recorded traces while discarding 0%, 10%, 25% and 50% of post-admission
samples moves gamma from 0.137/0.183 to 0.059/0.000 -- the transient contributes, but the
un-trimmed values were ALREADY far below the 0.40 boundary. The transient cannot explain a reported
0.405.

## Hypothesis 2: elapsed-time as the regressor makes gamma seed-sensitive

The reasoning: the detector regresses `log(peak)` on `log(elapsed seconds)` (`anytime.go:194`), and
elapsed time is non-uniform under bursty arrivals, so clustering inflates the slope.

**Refuted, and the opposite is true.** Measured across five seeds:

| regressor | healthy gamma | overload gamma | separation | sd |
|---|---|---|---|---|
| `elapsed_sec` | 0.132 | 0.468 | **0.336** | 0.059 |
| observation index | 0.118 | 0.425 | 0.307 | 0.050 |

Elapsed time separates BETTER (0.336 vs 0.307), so the current choice is correct rather than
defective. And the seed spread is 0.05-0.06 against a 0.34 signal -- **gamma is seed-stable**. There
is no pathology in the estimate.

## Hypothesis 3: the calibration walk overshoots on an unlucky draw

The reasoning: the walk climbs until false alarms stop, so one unlucky rung pushes the boundary
past where the signal lives -- explaining knob 0.7, at which the overload gamma of ~0.47 sits BELOW
the boundary and the detector cannot fire at all.

**Refuted.** Reproducing the walk with the harness's own code at three seed offsets:

```
seed_offset=0 (seeds 42..46):    boundary 0.4 at fpr 0.05
seed_offset=1 (seeds 1042..):    boundary 0.4 at fpr 0.05
seed_offset=2 (seeds 2042..):    boundary 0.5 at fpr 0.00
```

Calibration lands at 0.4-0.5 on every draw and never climbs to 0.7. The acceptance rule is also
correct on inspection: one firing rung of twenty is exactly 0.05, which `fpr <= target_fpr` accepts
rather than rejecting.

## What is actually established

1. **The estimator is sound and seed-stable.** gamma separates healthy from overloaded by 0.34 with
   a seed sd of 0.06, and elapsed-time is the better of the two candidate regressors.
2. **The calibration rule is sound.** It selects 0.4-0.5 consistently and accepts an at-budget
   false-alarm rate rather than over-tightening.
3. **The 1-in-5 confirm failure is therefore NOT explained by any of these**, and I no longer have a
   mechanism for it. What I know: those rows report knob 0.7, which no calibration I can reproduce
   selects. The confirm stage seeds replicates by its own scheme, which differs from the offsets I
   tested, so the remaining candidate is a seed regime I have not sampled.

## What I changed, and what I deliberately did not

**Changed:** `elapsed_sec` is now emitted in `Signals`. Its absence is why hypothesis 1 reached a
wrong conclusion -- I refitted on the observation index because the actual regressor was not in the
trace, got 0.14-0.18 against a reported 0.405, and mistook a units mismatch for a defect. A trace
that cannot be re-fitted to audit its own reported quantity is under-instrumented, and that is a
real gap this exposed.

**Deliberately NOT changed:** the admission rule, the regressor, and the calibration rule. All three
were candidate fixes and all three are measurably fine. Changing a sound mechanism on an unrefuted
hypothesis is how a detector acquires unexplained complexity -- and I came close three times.

## The honest status of the robustness question

I reported earlier that "the detector's correctness is not robust across workload draws." That
claim rests on 3 of 15 confirm replicates, and I cannot now reproduce the mechanism. It should be
downgraded from a finding to an **open anomaly** until either the seed regime is identified or the
next epoch reproduces it. Stating it as an established weakness would be as wrong as suppressing it.
