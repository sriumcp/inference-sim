# Diagnosis: why the Peak-based statistics need a SEARCH, not a hand-tune

**Status:** the reason `peak_over_elapsed` and `peak_ratio_stability` are campaign
FACTORS with searchable sub-parameters rather than a single author-chosen formula.

## The theory, stated correctly

For a reflected walk, `R_t = Peak_t / t` behaves as:

| regime | R_t as t -> infinity |
|---|---|
| positive drift (OVERLOADED) | converges to a positive CONSTANT (p_up - p_down) |
| zero drift (CRITICAL) | decays to 0 as 1/sqrt(t); unstable/heavy-tailed |
| negative drift (HEALTHY) | decays to 0 as 1/t |

So the correct reading is: **near-constant when overloaded, falling when
underloaded, unstable at criticality.** Verified by simulation (10^2..10^5 steps):
positive drift converged to 0.1016 vs a predicted 0.10; zero drift fell ~3.2x per
decade; negative drift ~10x per decade.

**Criticality is not experimentally usable as an anchor.** Determining the exact
critical boundary is not possible from measurement -- which is why the zero-drift
case is a theory tool for understanding the middle band, not a calibration target.
Every operating point the suite uses is declared relative to a MEASURED cliff
(r_nominal = 20 rps, campaign/apparatus/LADDER.md), never to a theoretical rho=1.

## Three hand-built attempts, all measured, none discriminating

**1. `peak_over_elapsed` — the LEVEL of R_t.** Fails: all three regimes make R_t
large EARLY (at t=100 they span 0.031..0.139 and OVERLAP), so a fixed threshold on
the level reads "early in the run" more strongly than "saturated". On the real
apparatus it fired at 0.7s elapsed and then LATCHED: exactly one STABLE ->
OVERLOADED transition in 1200 records, R_t decaying 31.4 -> 9.2 within the run
because `peak` is a ratchet while t keeps growing.

This is the level-vs-trend error `metamorphic_tests.md` §1.0 names as the single
most common detector defect -- committed while implementing a statistic whose whole
point is to be a trend.

**A CORRECTION to STATISTIC-SCREENING.md.** That document called
`peak_over_elapsed` "strong, monotone" on a 35x span across the rate ladder. The
span is real but CONFOUNDED: it compared final values across runs of different
DURATIONS (higher rate => shorter run => larger R_t purely because t is smaller).
Measured at fixed rate: 0.3x nominal gives R_t = 0.594 at n=300 but 0.174 at
n=1200. Part of that 35x was run length, not load.

**2. `peak_ratio_stability` with an event-INDEX midpoint.** Fails: at high load
arrivals cluster early while completions trail, so the index midpoint is far from
the time midpoint. At 2.0x nominal the peak correctly grew 1.21x, but elapsed grew
3.08x (vs 2.01x at 0.3x) and the elapsed term dominated, INVERTING the verdict.

**3. `peak_ratio_stability` with a TIME-anchored horizon.** Run-length invariance
achieved (0.3x: 0.57/1.00 at n=400/1000; 2.0x: 0.91/0.88) but still not
discriminating -- sub-capacity reaches 1.00 where the theory wants it FALLING, and
1.5x super sits at 0.57 where the theory wants NEAR-CONSTANT. Ordering scrambled.

## Why this is a search, not a formula

Each attempt fixed the previous one's defect and exposed a new one. The free
choices are not deducible from the theory -- the theory is asymptotic (t ->
infinity) while every measurement is a finite run, and the gap is exactly where
these parameters live:

- WHICH horizon ratio (half-time? fixed window? log-spaced pair?)
- HOW MUCH warm-up to discard before Peak is meaningful (R_t is largest when t is
  smallest, so this is load-bearing, not cosmetic)
- WHAT the walk is (concurrency vs unserved work -- and note the two need
  thresholds orders of magnitude apart, which is why per-row FPR calibration
  rather than a shared threshold is what makes them comparable)
- WHERE the band sits, at a matched false-alarm rate

That is a response surface over declared factors, which is what
`kind: optimization` exists to search with a policy hash committed before the
first measurement. Continuing to hand-tune would mean choosing the winner by my
own judgement on tests I was simultaneously rewriting -- the failure mode already
recorded in ESTIMATOR-IMPOSSIBILITY.md, where five hand-built variants each traded
one failure for another.

**So both statistics stay as factor levels with their sub-parameters (WARM,
horizon, SRC, CONSEC, MINOBS) as searchable axes, and the campaign decides.** The
honest state today: NO Peak-based configuration has yet been shown to discriminate
on this apparatus. Reporting one as a finding before the campaign runs would be
exactly the p-hacking the pre-registration exists to prevent.
