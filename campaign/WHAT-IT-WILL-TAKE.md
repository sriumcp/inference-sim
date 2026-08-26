# What it will actually take to build a robust always-valid detector

**Written in answer to: "we've run a lot of epochs -- what's it going to take?"** The honest
answer has two halves, and the first is that most of those epochs were not buying detector
quality.

## Part 0: where the six epochs actually went

| epoch | ended on | was it about the detector? |
|---|---|---|
| e1 | adapter knob grid was a comment describing a function nobody wrote | no -- my apparatus |
| e2 | `plan` re-running non-idempotently | no -- my process error |
| e3 | two malformed invariants (a literal `"baseline"`, a mutable branch ref) | no -- my apparatus |
| e4 | semantic exception: foldover could not form its fit | partly -- peak_rate's infeasibility broke it |
| e5 | output contract changed type mid-epoch (`float -> null`) | no -- my apparatus |
| e6 | ran to completion, `certified=False` | **yes** |

**Five of six were my instrument, not the mechanism.** That is the real cost, and it is not a
statistics problem -- it is a "build the instrument before launching" problem, which
`LAUNCH-CHECKLIST.md` already says and which I violated repeatedly. Any future epoch is cheap
only if the adapter is frozen and contract-stable FIRST.

## Part 1: the mechanism has three genuine statistical defects

These are real, they are in the current `anytime.go`, and epoch 6's own results are the evidence.

### Defect 1 (fatal): the CS bounds a NON-STATIONARY quantity

The wrapper applies an empirical-Bernstein / mixture-SPRT bound to the **mean of a sample
stream**. But look at what the stream is (`anytime.go:399-411`):

- `wrapped=peak_rate` -> the **record-setting indicator** (did this event set a new high-water
  mark?)
- `wrapped=composite` -> composite's **Score**

An empirical-Bernstein bound requires samples that are conditionally centred given the past. The
record-setting indicator is not: under exchangeable draws, `P(record at t) = 1/t` **exactly** --
a deterministic decay, because the maximum it must beat is non-decreasing. So `E[sample]` is not
a fixed parameter, and a mean bound over it narrows around a moving target.

**This is the exact defect I identified for raw `R_t` in the design (§3.1.1) and then
reintroduced one level down.** I moved from "level of a ratchet" to "mean of the ratchet's
derivative", which is better but still not stationary.

Symptom, and it is diagnostic rather than cosmetic: **all five confirm replicates returned
EXACTLY 129.0.** A genuine evidence-driven stopping time varies with the data. A stopping time
determined by deterministic decay arithmetic does not. The zero variance that blocked
certification is not just a seed problem -- it is telling us the stopping rule is not reading
evidence.

### Defect 2: the `max(1, I)` floor erases the burstiness signal where it matters

The dispersion correction is `neff = windowN / max(1, I)`. The floor is correct for coverage --
narrowing below the i.i.d. bound is not licensed by the martingale argument -- but it means
**every level with `I < 1` is treated identically.** That is precisely why the pre-registered
monotonicity claim half-failed: `constant` (I=0.002) and `poisson` (I=0.43) get the same width,
so the prediction had no room to hold at the smooth end.

More importantly, dividing `n` by `I` is a **heuristic, not a theorem**. There is a proper
treatment (below) and this is not it.

### Defect 3: the fire rule's conjunction makes the CS partly decorative

`lo > boundary AND wrapped.Level != Stable`. The second clause exists for a good reason (an inert
calibration knob is not comparable), but it means the wrapped detector can veto the sequence. So
the reported stopping time is `max(CS decision time, wrapped decision time)` -- and epoch 6's
result that the wrapper is **2x SLOWER than bare composite** (129 vs 63) is largely this: the CS
is not accelerating anything, it is adding a second gate.

## Part 2: what a correct construction looks like

Four changes, in dependency order. Items 1-2 are the statistics; 3-4 are what makes it robust.

### 1. Bound the right object: a self-normalized statistic for the GROWTH EXPONENT

Stop bounding a mean. The quantity of interest is the exponent `gamma` in `Peak_t ~ t^gamma`,
which is a **regression slope**, not a mean. The correct tools:

- **Frequentist:** a time-uniform confidence sequence for a regression coefficient via the
  self-normalized / "Robbins mixture" construction -- de la Pena-Klass-Lai style bounds on
  `sum(x_i * eps_i) / sqrt(sum(x_i^2))`, where `x_i = log(t_i) - mean(log t)`. This is
  time-uniform by construction and does NOT require the samples to be identically distributed,
  only the *errors* to be conditionally centred, which is a far weaker and actually-satisfiable
  assumption.
- **Bayesian:** a conjugate normal-inverse-gamma posterior on `(gamma, sigma^2)` updated online,
  reported as a credible interval. Anytime-valid in the Bayesian sense automatically (the
  posterior is a martingale under the prior), and it composes naturally with the next item.

Either is defensible; they answer slightly different questions. The frequentist version gives a
guarantee that holds under the worst case in the model class; the Bayesian one gives a sharper
interval at the price of a prior. **For a detector shipped to operators I would take the
frequentist self-normalized bound**, because its guarantee does not depend on a prior nobody will
tune -- and then report the Bayesian posterior alongside as a diagnostic.

### 2. Handle burstiness with a variance PROCESS, not a scalar divisor

Replace `neff = n / max(1, I)` with an explicit model of arrival clustering:

- **The principled version:** treat arrivals as a **Hawkes process** (self-exciting: each arrival
  raises the intensity briefly). Its dispersion is `I = 1/(1-branching_ratio)^2`, so estimating
  ONE parameter -- the branching ratio -- gives both the burstiness measure and a generative model
  the CS variance can be computed from, rather than a plug-in divisor. The branching ratio is
  estimable online from inter-arrival times.
- **The cheap version that is still correct:** use a **variance-adaptive** self-normalized bound,
  where the normalizer is the observed `sum(x_i^2)` rather than an assumed `n`. Over-dispersion
  then inflates the normalizer automatically, with no `I` estimate and no floor -- the bound
  adapts because it is self-normalized, which is the whole point of that construction.

The second option removes Defect 2 entirely and costs less code than what is there now. **That is
the one to build.** It also dissolves the `max(1, I)` asymmetry: no floor is needed because
nothing is being divided.

### 3. Remove the conjunction; keep calibratability differently

Let the CS decide alone, and preserve knob-comparability by calibrating the **threshold on gamma**
(the criticality exponent, nominally 0.5) rather than by deferring to the wrapped detector. That
gives a real dial -- move the exponent boundary -- without a second gate that dominates the
stopping time. This is what would let the wrapper actually be FASTER than what it wraps, which
epoch 6 showed it currently is not.

### 4. Make the free choices measurable, then measure them

With 1-3 done, the remaining genuine unknowns are few and each is a real factor:

| factor | levels | why it is free |
|---|---|---|
| bound family | self-normalized-frequentist, NIG-Bayesian | different guarantees, not orderable a priori |
| prior strength (Bayesian arm only) | weak / moderate | sharpness vs robustness |
| exponent threshold | 0.4 / 0.5 / 0.6 | 0.5 is theory's criticality; the useful operating point is empirical |
| window anchoring | admission-anchored / trailing-ratio | already known to matter (the code documents a failure at fixed ratio) |
| latch | on / off | epoch 6 could not measure T4 because the latch suppresses flapping |

That is **5 factors, ~20 rows, one epoch** -- and it would answer the construction question with a
certified result, because the stopping time would finally vary with the data.

## Part 3: the apparatus fixes that make the epoch cheap

Non-negotiable, and all already diagnosed:

1. **Vary `NOUS_WORKLOAD_SEED` across confirm replicates.** Zero variance blocked certification in
   epoch 6 and made `--liveness` report `sd=0`, which collapsed its significance rule to
   `|effect| >= 0`. Without this, no epoch can ever certify.
2. **Emit per-level metrics for every level** (done in epoch 6) and keep every key **type-stable**
   (done -- no nulls). These two cost epochs 4 and 5.
3. **Score `INDET` against `t1_pass` / `correctness_bursty`**, which it can move. Its effect on the
   speed objective is structurally zero.
4. **Freeze the adapter before launching.** Five of six epochs died on adapter defects found after
   launch. `--smoke` AND `--liveness` are necessary but were not sufficient -- neither exercises
   the response keys the fit consumes, so add a contract probe that asserts every declared metric
   is present AND numeric on two different rows.

## Part 4: the honest bottom line

**Effort:** one focused implementation pass on `anytime.go` (items 1-3 above -- realistically the
self-normalized bound plus removing the conjunction is a few hundred lines and replaces roughly
as much), then ONE epoch of ~20 rows.

**Not six more epochs.** The reason this took six is that I was debugging an instrument while
believing I was measuring a detector. The mechanism's statistical defects were visible in epoch
6's output -- the identical 129.0 across replicates was the tell, and I initially read it as a
seed problem when it is also a stopping-rule problem.

**What is already worth keeping**, established and not in doubt:

- the admission gate (no verdict before the first completion -- the `gamma == 1` tautology is real
  and verified)
- the frozen burstiness ladder with per-level MEASURED cliffs
- the seven anti-gaming constraints, two of which independently caught the fastest configuration
  on the board
- lead time as a scored regime (it exposed the alpha speed-vs-earliness inversion, which is the
  most actionable finding the campaign produced)
- `mixture_sprt > howard_eb`, which will need REVALIDATING once the bounded object changes, since
  the comparison was made on a mis-specified target
