# Finding: §2e.4's three estimator requirements cannot all hold

**Status:** blocking for OWD; SWD unaffected. Found 2026-08-24 while implementing
`detection_strategies.md` §2e.

## The claim

§2e.4 requires the online `r_dec` estimator to be simultaneously:

1. **Adaptive** — `r̂_dec` must FALL when capacity collapses, else T3 fails.
2. **Universal** — must work on prefill-dominant, decode-dominant, mixed, **and
   constant-shape** traffic.
3. **Non-circular** — must not require knowing whether the system is saturated in
   order to calibrate the detector that decides saturation.

**These three cannot hold at once**, because of one structural fact:

> **Capacity is only observable while the server is saturated.**

Below capacity, throughput equals the ARRIVAL rate — it measures demand, not
capacity. So any estimator that learns `r_dec` from healthy traffic is learning
offered load, and a residual netted against it can never diverge.

## Five implementations, each measured on the real ladder

| Variant | Failure | Evidence |
|---|---|---|
| Two-column ridge on `(κ, r_dec)` per §2e.4 | Unidentifiable on constant-shape traffic: one equation, two unknowns | κ drifted to 37.9 from a 0.02 prior; `r_dec` to 347,968 tok/s (absurd for 8B on one H100). Residual INVERTED: 153,659 at 0.3× vs 107,498 at 1.5× — fired more on healthy traffic |
| Aggregate work / busy-time, κ at prior | `r_dec` adapts to offered load ⇒ residual cannot diverge | Residuals collapsed into a 133–227 band across the whole ladder instead of separating 0 from ~97,000 |
| Drain over inter-ARRIVAL gaps | Units mismatch vs per-completion charging; warm-up offset frozen by `max(0,·)` | Residual stuck at 33,000 at 0.3× nominal ⇒ fired on 93% of a healthy run |
| Drain over inter-COMPLETION gaps | Collapse makes completions sparser ⇒ drain term grows, cancelling the T3 signal | T3 went PASS → FAIL |
| High-water (max observed work rate) | Ratchets to the peak burst rate, then over-drains everything | 6 test failures incl. T3 and monotonicity |

Non-identifiability proved numerically: for `I=3270, O=3102, T=1e6`, the pairs
`(κ=0, r_dec=3102)`, `(0.185, 3708)`, `(0.461, 4609)`, `(1.793, 8965)` all fit
**exactly**. The ridge picks one by prior strength, not evidence.

## What this means for the campaign

- **SWD is unaffected** and is the variant to carry forward: its threshold `h` is
  computed from the workload spec's burst envelope, so it never needs to learn
  capacity from live traffic. §2e.5 already predicts SWD is the sharper of the two.
- **OWD as specified is not implementable** without resolving the trilemma. Its
  production claim ("router-deployable, needs no spec") is what the trilemma costs.
- The honest resolution is to DECLARE capacity (from the hardware config or an
  offline calibration run) rather than fit it online. That makes `r_dec` a
  calibrated constant, keeps the residual well-posed on every workload shape, and
  T3 still passes THROUGH THE RESIDUAL: when the served rate falls below the
  declared capacity, arriving work outruns the drain and R diverges. A pinned-rate
  detector fails T3 because its rate is a GUESS, not because adaptivity is required.

**This does not invalidate the drift-based approach.** The Lindley residual
separated the regimes cleanly and monotonically once the drain term was correct
(33,262 → 246,300 across 0.3×–2.0× nominal). The defect is confined to how
`r_dec` is obtained.

## Not yet done

The declared-capacity variant is designed but NOT implemented or measured. Until
it is, no detector comparison should be reported: OWD would be scored on a
statistic known to be mis-specified. The five attempts are preserved in
`estimator_attempt5_highwater.go.txt` and in this repo's git history.
