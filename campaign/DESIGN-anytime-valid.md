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
| `peak_rate` | `R_t = Peak_t/t`, RATCHET numerator (`peak_rate.go:219`) | Peak growth exponent `gamma_t = d log Peak_t / d log t` | above the criticality exponent 0.5? |

`R_t`'s expectation drifts downward by construction even at constant load, so a CS over
raw `R_t` would narrow around a moving target -- a confident wrong answer, the exact
failure an anytime-valid method is adopted to prevent. The PR's own theory says the
regimes differ in the DECAY EXPONENT (`rho>1` -> constant; `rho=1` -> `1/sqrt(t)`;
`rho<1` -> `1/t`), not in the level, so the exponent is the stationary quantity. One CS
implementation, two adapters.

#### 3.1.1 Parameterize on Peak, not on R_t (raised by the user)

`log R_t = log Peak_t - log t`, so the two slopes are equivalent up to an ADDITIVE
CONSTANT:

```
gamma_t = d log Peak_t / d log t          beta_t = d log R_t / d log t = gamma_t - 1
```

| regime | Peak grows as | `gamma_t` | `beta_t` |
|---|---|---|---|
| `rho > 1` overloaded | `t` | **1** | 0 |
| `rho = 1` critical | `sqrt(t)` | **0.5** | -0.5 |
| `rho < 1` healthy | `O(1)` | **0** | -1 |

For a point estimate the choice is cosmetic. For the CONFIDENCE SEQUENCE it is not, and
`gamma_t` is the correct target:

1. **It is estimated from the raw observable.** `Peak_t` is already tracked (`p.peak`), so
   regressing `log Peak` on `log t` touches the ratchet directly. Forming `R_t` first
   injects the deterministic `-log t` into every sample before the regression -- no added
   information, and on an increment-based CS a zero-variance term with nonzero leverage.
   Estimate the free parameter; subtract the known constant afterward.
2. **Its support is bounded and physical.** `Peak_t` is non-decreasing so `gamma_t >= 0`
   ALWAYS, and in-flight cannot outgrow arrivals so `gamma_t <= 1`. A parameter bounded
   in `[0, 1]` is exactly where empirical-Bernstein / Howard-style bounds are tightest,
   and the interval can be clipped to the support for free. `beta_t in [-1, 0]` has the
   same width but carries an offset to remember at every comparison, where a sign error
   lands a full regime away.

**Free soundness check this buys:** `gamma_t < 0` is IMPOSSIBLE, so a CS interval dipping
below zero is a bug indicator rather than a verdict -- a diagnostic the `beta`
parameterization hides. The verdict test reads: fire when the interval lies ABOVE `0.5`
(Peak growing faster than `sqrt(t)`), stay STABLE when it lies BELOW.

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

### 4.1 The `Level` enum trap (found while reading the seam)

`Level` has three values and `UnmarshalJSON` maps every unrecognized string to `Stable`
"for unknown values". So adding a fourth enum value means an older reader decodes
`"INDETERMINATE"` as **STABLE** -- a silently WRONG VERDICT, not a parse error. The
existing reducer also asserts `Level >= Stable && Level <= Overloaded` and scans
`Overloaded -> Stable`, so a new value changes the severity ordering too.

Consequences, which the build must implement rather than discover:

- INDETERMINATE must NOT be a fourth `Level` on the shared enum. It is carried
  out-of-band, on the wrapper's own result surface, so `--saturation-report` stays
  decodable by every existing reader and `ReduceAll`'s ordering is untouched.
- The wrapper's own report must make undecidedness legible without widening `Level`.
- A round-trip test must pin this: an anytime report decoded by the existing
  `UnmarshalJSON` must never silently become STABLE.

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

## 7. Stages: let nousko do its job

The existing campaign declares NO build stage, correctly -- its detector already
existed. **Here the detector does not exist yet, so this campaign is the `plan` + `build`
case**, and declaring them is not ceremony. The guide's measured comparison, three builds
of one mechanism on one target:

| build | outcome |
|---|---|
| prompt never received the cost facts | removed 70% of the per-item work and ran **23.7% SLOWER** |
| prompt received them | +3.65%, certified |
| designs first (a separate pricing call) | **-10.4%**, and named the winning architecture in its design artifact BEFORE writing code |

```yaml
stages: [plan, build, verify, screen, confirm]
max_turns: {plan: 60}
```

`plan` spends one call pricing the mechanism and writes `mechanism_plan.json`
(schema-checked: `cost_model`, `approach` with BOTH `cost_of_deciding` and
`cost_avoided`, `rejected` with at least one priced alternative, `failure_modes` with
`symptom`/`cause`/`guard`). `build` implements it. `screen` then FALSIFIES the plan's
`cost_avoided > cost_of_deciding` prediction. Both sit outside the compiled epoch, so the
epoch stays tokenless.

**`plan` is not idempotent** -- a relaunch after a later failure spends a second call and
gets a DIFFERENT design, so the campaign would measure a mechanism whose plan nobody read.
To reuse a reviewed plan, copy `mechanism_plan.json` into the new work-dir root and drop
`plan` from `stages`, recording in the YAML that this was deliberate.

### 7.1 The division of labour, corrected by the validator

An earlier draft of this section declared NO `factors:` block, on the theory that
enumerating factors was the author searching the space. **`nous validate campaign`
rejects that: `factors` is a REQUIRED property.** And the code says why it must be --
`build.py:707` `declared_native_tests(factors)` reads every factor's relations to learn
which native tests the mechanism must satisfy. The build is TOLD the axes and authors a
mechanism honouring them; it does not invent them.

So `LAUNCH-CHECKLIST.md`'s rule is finer than "nousko picks the factors". Applying its own
test -- if the answer needs a MEASUREMENT it is nousko's; if it needs
sameness-for-everyone it is mine:

| | Mine (the instrument) | Nousko's (the search) |
|---|---|---|
| axes | which exist, and that every level RUNS | whether an axis is dead |
| levels | which are REACHABLE through `--saturation-config` | which one WINS |
| relations | what each level must GUARANTEE | which interactions are consequential |
| apparatus | ladder, cliffs, seeds, metric, adapter | where the optimum sits |

**Declaring a level is not predicting it.** The five axes are the mechanism's genuinely
free choices, and the theory fixes none of them: `WRAPPED` (composite vs peak_rate, whose
CS targets differ per §3.1.1), `BOUND` (howard_eb vs mixture_sprt), `ALPHA`, `DISPWIN`
(the burstiness-sensing window -- the campaign's central claim runs through it), `INDET`
(strict vs lean_stable). FPR calibration stays outside (§7.3).

### 7.1.1 Two schema facts the guide's prose gets wrong

Both found by running the validator, not by reading:

- **`stages` lives under `optimization:`** (`stage.py:148` reads
  `campaign["optimization"]["stages"]`), but **`max_turns` is read at TOP level**
  (`stage_runner.py:_plan_max_turns` reads `campaign["max_turns"]`). The guide's snippet
  shows both unindented together; top-level `stages` fails with *"Additional properties
  are not allowed ('stages' was unexpected)"*.
- A factor patching a file the **`build` stage will author** raises a WARNING, not an
  error -- the validator names this case as legitimate. Expected here for `at.yaml`.

**Corollary for launch:** the validator warns that a `native_test` identifier absent from
the target counts as a FAILED correctness relation, which aborts the campaign at `verify`.
All 14 declared tests must exist and run under `test_command` before `nous run`.

### 7.2 `factor_nomination` is the ONLY channel to the build agent

`guidance.factor_nomination` is passed VERBATIM into the `build` prompt as "AUTHOR'S
GUIDANCE ON THE MECHANISM". `guidance.interpretation` is **reserved and read by no
stage**. `target_system.description` is the only other prose that reaches the build.

This is load-bearing rather than stylistic: the guide records a field test where the
author put the target's known crash mode into `factor_nomination` before it was wired to
the prompt, it reached nobody, and the build shipped the exact defect already diagnosed --
confounding a two-arm comparison by 10x. **If the build must know it, it goes in
`factor_nomination` or `target_system.description`.**

Therefore these facts MUST appear in `factor_nomination`, because a build agent that
misses any one of them writes a wrong mechanism:

1. **Zero lines change in `peak_rate.go`, `composite.go`, `backlog_drift.go`.** #1620 is
   under review; its byte-identity tests must stay valid.
2. **`Signals` is an OUTPUT surface** (commit `3a61aa3b`) -- it serializes into
   `--saturation-report`, so the wrapper must NOT read `peak_backlog`/`elapsed_sec` from
   it. Track peak and elapsed from the `Event` stream directly.
3. **`R_t` is a ratchet** (`peak_rate.go:219`), so the CS target is the Peak exponent
   `gamma_t in [0,1]`, not the raw score (§3.1, §3.1.1).
4. **`Level.UnmarshalJSON` silently defaults unknown strings to `Stable`**
   (`detector.go:69`) -- see §4.1.
5. **`reduce.go` indexes counts by `Level`** and scans `Overloaded -> Stable` for the
   severity tie-break, asserting `Level >= Stable && Level <= Overloaded`.
6. The detector must be constructible from the registry by name and reachable purely
   through `--saturation-config`.

### 7.3 What stays outside the factor space regardless

FPR calibration (existing invariant DS1): calibrated per row on the calibration band and
frozen before any ladder is read. Searching it jointly with the mechanism would tune
sensitivity with knowledge of the answers.

## 8. Soundness checks (asserted, not trusted)

- **Ratchet check.** On fixed load, raw `R_t` is non-increasing after its peak while
  `gamma_t` has no systematic trend. If raw `R_t` shows no drift, §3.1 is wrong -- report
  it and simplify.
- **Support check.** `gamma_t in [0, 1]` by construction (§3.1.1). An estimate outside it
  is an estimator bug, not a regime -- assert it rather than clamping silently.
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
3. **The growth exponent is itself noisy.** A log-log slope over a ratchet has few
   effective degrees of freedom -- `Peak_t` only moves when a new maximum is set, so the
   effective sample size is the NUMBER OF RECORD-SETTING EVENTS, not the record count. If
   its CS never narrows enough to decide, peak_rate's level fails on
   `indeterminate_forever` and that is a legitimate reported outcome.
