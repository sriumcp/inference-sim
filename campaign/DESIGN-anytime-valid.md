# Anytime-valid saturation detection: derive the horizon instead of declaring it

**Status:** design, approved in conversation 2026-08-25. Not yet implemented.
**Location:** `campaign/DESIGN-anytime-valid.md` (docs/superpowers is gitignored in BLIS).
**Isolation:** worktree `.worktrees/anytime-campaign`, branch `campaign/anytime-valid`,
based read-only on `feat/peak-rate-detector` (#1620 @ 6959f64a). No commit lands on
`main`, `feat/peak-rate-detector`, `feat/swd-owd-saturation`, or nousko's `nousko`.

## 1. The problem

Every BLIS saturation detector answers with a horizon someone chose. The existing
campaign froze `n = 6000` because `HORIZON-IS-THE-APPARATUS.md` measured R_t's
super-vs-sub separation growing 2.3x -> 14.6x from n=500 to n=8000. That is a defensible
APPARATUS choice for a fair comparison, but it is the wrong shape for a deployed
detector: a real detector does not get to run a 6000-request calibration before
answering. It must say "I do not know yet" early and "I am confident now" later, from the
data it has.

Both detectors #1620 ships require several hand-set parameters -- composite's
`sensitivity`; peak-rate's `threshold`, `min_observations`, `warmup_us`, `consecutive_k`,
`overload_multiple`. Each is a horizon-or-sensitivity choice made in advance of the
traffic it will meet.

## 2. The claim

A confidence sequence -- an interval valid SIMULTANEOUSLY at every n, so it may be
inspected continuously without the error inflation that repeated peeking at a fixed-n
interval causes -- lets the detector start CONSERVATIVE and earn confidence from
evidence. And because a CS's width scales with the variance of what it observes, the
required horizon follows the traffic's BURSTINESS rather than an author's guess.

For a counting process the relevant quantity is the index of dispersion
`I = Var(N_T)/E[N_T]` (`I = 1` Poisson, `I > 1` over-dispersed). A Howard-style
self-normalized width scales like `sqrt(I * log log n / n)`, so doubling dispersion
requires roughly twice the observations for the same width. **Burstier traffic therefore
needs a longer horizon before the interval clears the threshold -- and that falls out of
the bound rather than being asserted.**

Adjacent literature: always-valid p-values, e-values / test martingales, mixture-SPRT.
nousko's guide already names confidence sequences as an acceptable inferential accounting
rule for an adaptive branch, so this is in-vocabulary for `kind: optimization`.

## 3. Mechanism: a wrapper, and it is score-specific

New file `sim/saturation/anytime.go` implementing the existing 4-method `Detector`
interface (`Name`/`Observe`/`Detect`/`Reset`). It holds a wrapped detector, forwards
`Observe`/`Reset`, and independently accumulates the same `Event` stream:

```
Observe(e):  wrapped.Observe(e)
             dispersion.Update(e.Timestamp)     // online I = Var(N_T)/E[N_T]
             evidence.Update(target(wrapped))

Detect():    lo, hi := CS(evidence, alpha, I)
             lo > crit  -> wrapped's fired level
             hi < crit  -> STABLE
             else       -> INDETERMINATE
```

**Zero lines change in `peak_rate.go`, `composite.go`, or `backlog_drift.go`.** #1620's
diff and its byte-identity tests stay valid. Absent an `anytime:` config block the
wrapper never constructs, so output is byte-identical (INV-6).

It reads `Event`, never `Signals`: commit `3a61aa3b` establishes `Signals` as an OUTPUT
surface (it serializes into `--saturation-report`, so an unconditional new key is a
report-format change). `Event` carries `Timestamp`, `InputTokens`, `OutputTokens` --
sufficient for both the dispersion estimate and the wrapped score.

### 3.1 The CS target differs per wrapped detector

Established in `campaign/findings/WRAPPER-IS-SCORE-SPECIFIC.md`, and NOT a free choice:

| level | score structure | CS target | verdict test |
|---|---|---|---|
| `composite` | freely fluctuating, roughly stationary | mean score | above the sensitivity-scaled noise floor? |
| `peak_rate` | `R_t = Peak_t/t`, RATCHET numerator (`peak_rate.go:219`) | log-log decay slope `beta_t = d log R_t / d log t` | above the criticality exponent -0.5? |

`R_t`'s expectation drifts downward by construction even at constant load, so a CS over
raw `R_t` would narrow around a moving target -- a confident wrong answer, the exact
failure an anytime-valid method is adopted to prevent. The PR's own theory says the
regimes differ in the DECAY EXPONENT (`rho>1` -> constant; `rho=1` -> `1/sqrt(t)`;
`rho<1` -> `1/t`), not in the level, so `beta_t` is the stationary quantity. One CS
implementation, two adapters.

## 4. The verdict alphabet, and the rules it forces

`INDETERMINATE` breaks two existing scoring rules. Both resolutions are PRE-REGISTERED
here, before any row runs -- epoch 3 died from wiring tests in after launch.

- **Rung verdict (§3.5).** FIRED iff fired >= 50% of DECIDED records; INDETERMINATE is
  excluded from the denominator, not counted as stable. A minimum-decided floor stops
  "one decided record, it fired" from passing.
- **T4 flips.** `fired -> INDETERMINATE -> fired` is NOT a flip (honest uncertainty);
  `fired -> STABLE -> fired` IS. The conservative choice: the alternative lets a
  detector launder flapping through INDETERMINATE.
- **New guard `indeterminate_forever <= 0`.** A detector answering INDETERMINATE always
  has a perfect FPR and zero flips -- `GUARDS-CAUGHT-IT.md`'s trap in new clothing. It
  must decide on every super-capacity rung.

## 5. Apparatus: the burstiness ladder (new, and must be measured)

Reachable only via `--workload-spec` (`blis run` has no arrival-process flag), so four
committed spec files crossed with the existing rate ladder:

| level | process | CV | expected I |
|---|---|---|---|
| smooth | constant | -- | ~0 |
| baseline | poisson | 1.0 | 1 |
| bursty | gamma | 2.0 | ~4 |
| very bursty | gamma | 4.0 | ~16 |
| **held out** | **weibull** | **3.0** | **~9**, never used in fitting |

**`r_nominal = 20` was measured under Poisson and almost certainly moves per level** --
bursty traffic saturates earlier. Per `LAUNCH-CHECKLIST.md` item 2 each level's cliff
must be measured before launch: a ladder that never tips makes its tests vacuous, and a
base rung that already tips makes them unfalsifiable. Four cliff measurements, not one.

**Pre-registered prediction:** observations-to-confident-verdict grows monotonically in
the MEASURED dispersion index, approximately linearly in `I`.
**Refuted if** gamma CV=4 reaches confidence in no more observations than Poisson at the
same mean rate.

## 6. Objective

Primary: **observations-to-confident-verdict**, minimize. The metamorphic passes and the
FPR budget stay HARD constraints so speed can never be bought by being wrong.

```
constraints:
  fpr_within_budget      >= 1
  t1_pass                >= 1
  fires_on_all_rungs     <= 0
  knob_at_grid_edge      <= 0
  indeterminate_forever  <= 0     # NEW (section 4)

regimes:                          # the trade, made checkable
  smooth: obs_to_verdict_poisson <= 0.5 * incumbent_obs_to_verdict
  bursty: correctness_bursty     >= 0.9
```

`obs_to_verdict_poisson`'s bound is DERIVED, not chosen: the smooth-regime target is
half the best incumbent's observations-to-verdict on the same ladder, measured during the
pre-launch cliff sweep (§5) and written into the YAML as a literal before the policy hash
is compiled. A pre-registered constraint cannot carry a symbol -- the number must exist
at compile time or the registration is not binding. If no incumbent decides at all on a
rung, that rung's smooth-regime check is reported as inapplicable rather than passed.

The bar: beat the fixed-horizon n=6000 instrument on observations-to-verdict at matched
FPR with every metamorphic pass held. Merely matching is insufficient
(`detection_strategies.md` §3b).

### 6.1 The speed metric had to be repaired first

`campaign/findings/DETECTION-DELAY-IS-CLIPPED.md`: the previous `detection_delay_us`
measured from the first record AFTER the 10% warm-up, so any detector already fired when
scoring opened returned `max(0, t0-t0) == 0`. Every first-fire in the first 200 of 2000
records reported exactly 0 us. The reported "0.0 ms tie" between composite and randomwalk
is void -- both fire inside that dead zone (events 3 and 21), and on the only unclipped
evidence composite is ~7x FASTER, not equal.

Fixed before this apparatus freezes: measure from `records[0]`, return the event INDEX
(uncippable by a time origin), flag `clipped`, keep the warm-up's transient rejection.
Verified strictly monotone; the collapsed pair now separates 3000 vs 21000 us.

**A speed objective on a clipped metric would have had a guaranteed floor of 0 for
exactly the detectors that matter.**

## 7. Factors (screen, resolution V)

| factor | levels | why it is free |
|---|---|---|
| `WRAPPED` | composite, peak_rate | the two #1620 detectors; each with its own CS target (§3.1) |
| `BOUND` | howard_ci, mixture_sprt | theory does not pick one |
| `ALPHA` | 0.01, 0.05, 0.10 | confidence level |
| `DISPWIN` | 4 levels | dispersion-estimation window |
| `INDET` | conservative, aggressive | INDETERMINATE -> verdict policy |

FPR calibration stays OUTSIDE the factor space (existing invariant DS1): calibrated per
row on the calibration band and frozen before any ladder is read. Searching it jointly
would tune sensitivity with knowledge of the answers.

## 8. Soundness checks (asserted, not trusted)

- **Ratchet check.** On fixed load, raw `R_t` is non-increasing after its peak while
  `beta_t` has no systematic trend. If raw `R_t` shows no drift, §3.1 is wrong -- report
  it and simplify.
- **CS coverage.** On known-stable calibration traffic the interval must not exclude the
  healthy value more often than `alpha`. A CS failing coverage is invalid however fast it
  concludes.
- **Response-interior check.** The winner's objective must be INTERIOR to the metric's
  range -- the response-side analogue of `knob_at_grid_edge`, and the rule whose absence
  produced §6.1's clipped finding.

## 9. Risks, stated up front

1. **The speed win may be small.** If composite already decides by event 3, there is
   little room to be faster on smooth traffic; the wrapper's value would then rest on
   the BURSTY side (correctness where a fixed horizon over-fires), a narrower claim than
   "faster". Saying so now rather than reframing later.
2. **Four cliff measurements gate launch.** Skipping them is how epoch 1 died.
3. **`beta_t` estimation is itself noisy.** A log-log slope over a ratchet has few
   effective degrees of freedom; if its CS never narrows enough to decide, peak_rate's
   level fails on `indeterminate_forever` and that is a legitimate reported outcome.
