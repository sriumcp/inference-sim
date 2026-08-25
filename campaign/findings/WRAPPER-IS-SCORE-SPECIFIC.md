# The anytime wrapper cannot be score-agnostic: a ratchet and a fluctuating score need
# different CS targets

**Raised by the user asking whether the mechanism is an "anytime" derivative of
composite, of peak-rate, or both.** The answer is both -- but the first design
("confidence sequence over the wrapped detector's Score") is sound for only one of them,
and would report false confidence for the other.

## The asymmetry, verified in the code

| | `anytime(composite)` | `anytime(peak_rate)` |
|---|---|---|
| Score | rate deficit + quartile-filtered latency trend | `R_t = Peak_t / t` |
| Numerator | running counts over a window | `peak`, a RUNNING MAXIMUM (`peak_rate.go:219`, `if p.inFlight > p.peak`) |
| Within-run behavior | rises and falls freely | RATCHET: after the peak is set, `R_t` can only decay in `t` |
| Sample structure | roughly stationary inside a regime | strongly negatively autocorrelated, non-stationary BY CONSTRUCTION |

`PEAK-STATISTIC-DIAGNOSIS.md` already measured the ratchet: `R_t` fell 31.4 -> 9.2 with
**one** STABLE->OVERLOADED transition in 1200 records.

## Why that breaks a naive CS

A confidence sequence bounds a FIXED parameter -- typically a mean -- uniformly over
time. Its self-normalized (Howard-style) form assumes increments are not systematically
drifting. `R_t`'s expectation drifts downward by construction even when the load is
constant, so a CS over raw `R_t`:

- chases a moving target, and
- narrows around whatever the recent decay happens to be,

reporting high confidence about a quantity that is not the thing being tested. That is
worse than a wide interval: it is a **confident wrong answer**, which is the exact
failure mode an anytime-valid method is adopted to prevent.

For composite the same construction is fine -- its score has no built-in drift, so
"is the mean above the floor?" is a well-posed fixed-parameter question.

## The fix: put the CS on the stationary quantity

The PR's own theory names what is stable in each regime:

```
rho > 1  ->  R_t -> a positive CONSTANT      (HOLDS)
rho = 1  ->  R_t -> 0  as 1/sqrt(t)
rho < 1  ->  R_t -> 0  as 1/t               (DECAYS)
```

The regimes differ in the DECAY EXPONENT, not in the level -- which is the
level-vs-trend distinction `metamorphic_tests.md` 1.0 calls the most common detector
defect, and the same error `PEAK-STATISTIC-DIAGNOSIS.md` caught when `R_t`-as-a-level
put all three regimes in an overlapping band at t=100.

So the wrapper's CS target for peak-rate is a log-log growth EXPONENT, and it should be
parameterized on **Peak, not on R_t** (raised by the user, verified in
`campaign/probes/verify_exponent_equivalence.py`):

```
gamma_t = d log Peak_t / d log t   ~ 1    overloaded
                                   ~ 0.5  critical
                                   ~ 0    healthy

beta_t  = d log R_t   / d log t   =  gamma_t - 1      (exactly; offset measured 1.000000
                                                       in all three regimes)
```

Equivalent up to an additive constant, so for a POINT ESTIMATE the choice is cosmetic.
For the CONFIDENCE SEQUENCE it is not:

1. `Peak_t` is the raw observable the detector already tracks (`p.peak`). Forming `R_t`
   first injects the deterministic `-log t` into every sample before the regression -- no
   added information, and on an increment-based CS a zero-variance term with nonzero
   leverage. Estimate the free parameter; subtract the known constant afterward.
2. `gamma_t in [0, 1]` by construction (`Peak_t` non-decreasing => `>= 0`; in-flight
   cannot outgrow arrivals => `<= 1`). A bounded parameter is where empirical-Bernstein /
   Howard bounds are tightest and the interval can be clipped to the support for free.
   `beta_t in [-1, 0]` has the same width but carries an offset to remember at every
   comparison, where a sign error lands a full regime away.

**Free soundness check:** `gamma_t < 0` is impossible, so an interval dipping below zero
is a bug indicator rather than a verdict -- a diagnostic the `beta` parameterization
hides. The verdict becomes "is `gamma_t` confidently above 0.5?" (Peak outgrowing
`sqrt(t)`) instead of "is `R_t` confidently above a threshold?".

**Effective sample size caveat:** `Peak_t` only moves when a new maximum is set, so the
exponent's effective sample size is the number of RECORD-SETTING events, not the record
count. That is the quantity the CS width must be driven by.

## Consequence for the design

`WRAPPED` stops being a plain factor level and becomes a factor WITH A PER-LEVEL
MECHANISM:

| level | CS target | verdict test |
|---|---|---|
| `composite` | mean score | above the sensitivity-scaled noise floor? |
| `peak_rate` | log-log decay slope `beta_t` | above the criticality exponent? |

One wrapper, one confidence-sequence implementation, two adapters -- and which adapter
applies is dictated by the score's structure, not chosen. This must be settled BEFORE
the apparatus freezes: a wrapper that used the naive target for peak-rate would produce
a fifth author-built measurement that cannot fail in the direction being tested
(see DETECTION-DELAY-IS-CLIPPED.md for the other four).

## The pre-registered check that makes this falsifiable

The ratchet claim is measurable, so it should be asserted rather than trusted:

- **Ratchet check:** on a fixed-load run, raw `R_t` is non-increasing after its peak
  while `beta_t` has no systematic trend. If raw `R_t` shows no drift, the asymmetry
  above is wrong and the naive target was fine -- report that and simplify.
- **Soundness check:** on KNOWN-STABLE (calibration-band) traffic, the CS must not
  exclude the healthy exponent more often than `alpha`. A CS whose coverage fails here
  is invalid regardless of how fast it concludes.
