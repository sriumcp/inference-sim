# Epoch 3 (proposed): derive `n` instead of declaring it

**Status:** design note, NOT implemented. Deliberately deferred to its own epoch.

## The problem epoch 2 papers over

Epoch 2 freezes the horizon at `n = 6000` because
`HORIZON-IS-THE-APPARATUS.md` measured that R_t's super-vs-sub separation grows
2.3x -> 14.6x from n=500 to n=8000, and that at n=6000 the cliff-adjacent pair
(0.9x vs 1.1x) separates 3.8x across all five seeds.

That is a defensible APPARATUS choice for a comparison -- every row sees the same
horizon, so the comparison is fair. But it is an author-chosen constant, and it is
the wrong shape for a DEPLOYED detector: a real detector does not get to run a
6000-request calibration campaign before answering. It must say "I do not know yet"
early and "I am confident now" later, from the data it has.

## The idea: anytime-valid confidence, not a fixed n

The principled name is a **confidence sequence** -- an interval that is valid
SIMULTANEOUSLY at every n, so it can be inspected continuously without the
error-inflation that repeatedly peeking at a fixed-n interval causes. (Adjacent
literature: always-valid p-values, e-values / test martingales, mixture-SPRT.
nousko's own guide already names confidence sequences as one of three acceptable
inferential accounting rules for an adaptive branch, alongside POSI and data
splitting -- so this is in-vocabulary for the kind, not a new mechanism.)

Applied here, the detector would report, alongside its statistic:

- a lower and upper anytime-valid bound on R_t (or on the peak-growth ratio),
- an explicit INDETERMINATE verdict while the interval straddles the threshold,
- STABLE / OVERLOADED only once the interval lies wholly on one side.

`n` then stops being a knob: the detector reaches confidence when the evidence
supports it. The `Result` struct already carries a `Confidence` field and the
existing detectors already populate it with a crude ramp
(`min(1, arrivals/20)`), so the reporting surface exists; what is missing is that
the number means nothing statistically today.

## Why burstiness sets the required n -- and is measurable

The user's intuition is correct and quantifiable. For a counting process the
relevant quantity is the **index of dispersion** `I = Var(N_T)/E[N_T]`: `I = 1` for
Poisson, `I > 1` for bursty/over-dispersed arrivals (and BLIS can generate exactly
this range -- its arrival processes are poisson, gamma, weibull, constant, and the
spike/diurnal/servegen schedules are strongly over-dispersed by construction).

A confidence-sequence width scales like `sqrt(I * log log n / n)` in the
self-normalized (Howard-style) form, so:

- doubling the dispersion index roughly doubles the variance term and therefore
  requires ~2x the observations for the same interval width;
- burstier arrivals => wider interval at equal n => larger n before the interval
  clears the threshold. Exactly the user's prediction, and it falls out of the
  bound rather than being asserted.

The empirical half is cheap: `I` is estimable online from the same arrival
timestamps the detector already consumes (bucket counts over a window, then
variance/mean), so the detector can report both its dispersion estimate and the
implied evidence requirement. That makes the claim FALSIFIABLE -- a run at gamma or
spike arrivals should need measurably more observations than a Poisson run at the
same mean rate for the same interval width.

## Why this is epoch 3 and not an epoch-2 edit

1. **It changes the instrument.** A detector that emits INDETERMINATE has a
   different verdict alphabet, which changes the §3.5 rung rule (what does
   "fired for >= 50% of the run" mean when some records are neither?) and the T4
   flip count. Changing that mid-epoch would leave rows measured on two different
   instruments with one pre-registration -- the exact failure the epoch-boundary
   rule exists to prevent.
2. **It needs epoch 2's answer as an input.** Which statistic and which walk win
   at a FIXED horizon is the question to settle before asking how fast each one
   earns confidence. Bolting a stopping rule onto a statistic that has not yet been
   shown to discriminate would confound "cannot decide yet" with "cannot decide".
3. **It deserves its own factors.** The bound family (Howard-style self-normalized
   vs a mixture-SPRT), the confidence level, the dispersion-estimation window, and
   the INDETERMINATE policy are all free choices in exactly the way the horizon
   was -- i.e. a response surface, not a formula. Under nousko's graded-complexity
   tiers this is a tier-4 question (robustness across arrival regimes), which the
   discipline defers until the simpler ones are ruled out.

## Sketch of the epoch-3 design

- **Response:** observations-to-confident-verdict at matched FPR (minimize), with
  the epoch-2 metamorphic passes retained as constraints so speed cannot be bought
  by getting the answer wrong.
- **Factors:** bound family; confidence level; dispersion-window length;
  INDETERMINATE handling in the rung rule.
- **Ladder extension:** the existing rate ladder crossed with an ARRIVAL-PROCESS
  axis (constant / poisson / gamma / weibull / spike), which is what makes the
  burstiness prediction testable rather than merely stated.
- **Held-out:** a burstiness level never used in fitting.
- **Pre-registered prediction (so it can fail):** required observations grow
  monotonically with the measured dispersion index, approximately linearly in `I`.
  Refuted if a spike schedule reaches confidence in no more observations than
  Poisson at the same mean rate.
