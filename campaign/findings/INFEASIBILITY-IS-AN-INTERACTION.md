# The infeasible corner is an INTERACTION, and narrowing further would be p-hacking

**Epoch 4, 6 rows in:** 3 complete, 3 infeasible, and every rejection is `correctness` at
`ALPHA <= 0.075` while every completion is at `ALPHA = 0.10`.

```
st          ALPHA  KAPPA  LATCH   lead    corr   fpr   knob   violated
infeasible  0.075  3.0    True    0.925   0.20   0.05  0.8    correctness
infeasible  0.075  3.0    True    0.925   0.35   0.00  0.6    correctness
infeasible  0.05   5.0    False   1.000   0.25   0.00  0.6    correctness
complete    0.1    1.0    True    0.825   1.00   0.05  0.4
complete    0.1    5.0    False   0.925   0.90   0.00  0.45
complete    0.1    5.0    True    0.8875  1.00   0.05  0.45
```

## The tempting move, and why it is wrong

Narrow `ALPHA` to `[0.10]` and every row becomes feasible. Two reasons not to:

1. **A single-level factor is not a factor.** The campaign would fit KAPPA and LATCH only, and I
   would have DECIDED alpha rather than measured it.
2. **It is p-hacking by construction** -- narrowing the space until the answer I have already seen
   is the only one left. The user asked for a result without reward hacking, and this is the exact
   shape of it: the recommendation would be true of the region I chose because I chose it.

## What the data actually shows: an interaction

My own hand-check measured `alpha=0.05, kappa=1.0` as **FEASIBLE with correctness 1.00**. The
campaign measured `alpha=0.05, kappa=5.0` at **correctness 0.25**. Those are not contradictory --
they differ in kappa.

So **low alpha is survivable at LOW kappa and not at high kappa.** Both knobs widen the interval
(alpha by demanding more evidence, kappa by raising the width floor), and their effects compound:
either alone is tolerable, together they push the interval wide enough that it never clears the
boundary and the detector stops committing.

That is an INTERACTION, not a main effect -- and finding interactions is precisely what a factorial
design is for. Infeasible rows in one corner of the space are therefore the MEASUREMENT, not an
obstacle to it. `GUARDS-CAUGHT-IT` made the same point from the other direction: a constraint
rejection is information about the space.

## The real problem, and the honest fix

3 complete rows cannot support a 3-factor fit. That is a resolution problem, not a region problem:

- **wrong fix:** shrink the space until the survivors fit (p-hacking)
- **right fix:** sample more of the space so the feasible region has enough points to fit

Rows cost 237s and the whole campaign is ~35 minutes, so raising the screen resolution is cheap.
The feasible region needs >= 7 points for three factors plus center points; at roughly half the
space feasible, ~16-20 rows should deliver that.

## What must be reported regardless of how the fit turns out

The infeasible corner is a **finding about the detector**: at tight alpha combined with high kappa,
the interval is too wide to commit and correctness collapses to 0.20-0.35. An operator choosing
conservative settings on both knobs simultaneously gets a detector that rarely fires -- and that is
worth saying explicitly, because both knobs individually look like "be more careful".
