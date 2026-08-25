# Frozen apparatus: per-burstiness-level capacity cliffs

**Measured 2026-08-25 by `campaign/probes/measure_cliffs.py`.** This file records the
cliff *measurement* that `LADDER.md` performed for Poisson only, repeated for all five
burstiness levels of the frozen ladder.

| Artifact | sha256 |
|---|---|
| `cliff_table_hash` (`cliffs.json`) | `36902022383bcde62c208565cf8860f00a445faec0b0a2144e5012833ee14ed4` |
| `burstiness_ladder_hash` (the five specs, concatenated in filename order) | `4f91898a981911353bdadfff6a6954bde7c8bfef7e2202641e59417c697f0af7` |

## The freeze

**This apparatus is frozen. An apparatus change is an epoch boundary, not an edit.**
Per `../agentic-strategy-evolution/docs/optimization-campaign-guide.md`, the workload
generator, its data, and the cliff table are under the measurement freeze. Nothing in
this file may change while an epoch is measuring: re-measuring a cliff, re-tuning
`GROWTH_SUB`, editing a spec, or swapping the model/GPU all invalidate every row
already scored against `cliff_table_hash`. Changing any of it ends the epoch and starts
a new one with a re-registered policy.

## Ground truth: backlog divergence, not a latency threshold

Per §1.1, a regime is super-capacity iff **backlog diverges**, which shows up as mean
E2E that keeps GROWING with the horizon instead of stabilizing. Each rate is run at two
horizons (`n=800` and `n=3200`, seed 42) and the growth `(e2e_hi - e2e_lo) / e2e_lo` is
compared against `GROWTH_SUB = 0.25`. The knee is the **last rate whose growth stays
sub**; the first rate above it is SUPER.

A latency threshold was deliberately not used: an absolute millisecond bound cannot
distinguish a slow-but-stable regime from a diverging one, and it would have to be
re-chosen per burstiness level (defeating the controlled comparison).

## Why the cliff is not 20

`LADDER.md` records `r_nominal = 20 rps` with mean E2E ~9700 ms at the knee. Under the
frozen burstiness specs the same measurement gives **~1046 ms at 20 rps with +1.4%
growth** — deeply sub-capacity — and Poisson's knee sits at **88 rps**. Directly
measured corroboration: at rate 40 the cluster completes all 800 requests with
`responses_per_sec = 36.3`, `still_queued = 0`, `still_running = 0`. It is not
saturated anywhere near 20.

The two numbers describe **different workloads**. `LADDER.md`'s knee was measured on the
earlier synthesized workload; the frozen burstiness specs fix a distinct token profile
(prompt ~512±128, output ~128±32 truncated to [16, 256]) whose short outputs make decode
far cheaper per request, so far more requests per second fit. This is exactly the reason
Task 2 exists: `r_nominal` is a property of the (workload, hardware) pair, so reusing a
cliff measured on another workload is a declaration, not a measurement.

**Consequence for the plan as written.** `PLAN-anytime-valid.md` Task 2 sweeps
`[8, 10, ..., 26]`. Every one of those rates is sub-capacity for every level, and
`find_cliff`'s `knee` variable would simply hold the **last rate probed** — returning
`26` for all five levels with nothing raised, because the only guard was for the
*opposite* open end (`knee is None`, every rate already super). That is the same
open-top fabrication shape as `findings/RATE-FLAG-IGNORED-UNDER-SPEC.md`: five plausible
cliffs, equal to each other, hashed into `cliff_table_hash`, and the
`gamma_cv4 < poisson` assertion would then have *caught* it only by accident of
strictness (26 < 26 is false). `find_cliff` now carries the missing guard: if the top
rate probed is still sub, it **raises** rather than reporting the sweep ceiling as a
cliff.

## Hardware, model, and workload

Unchanged from `LADDER.md`: `meta-llama/llama-3.1-8b-instruct`, H100, TP=1,
`trained-physics` latency model — all committed BLIS defaults for this model. The five
specs are identical except for their `arrival` block, which is what makes the ladder a
controlled comparison: any effect measured across levels is attributable to arrival
burstiness alone.

**Rate is applied by rewriting `aggregate_rate` in a COPY of the frozen spec.**
`--rate` is silently ignored under `--workload-spec`
(`findings/RATE-FLAG-IGNORED-UNDER-SPEC.md`); the frozen specs are never mutated in
place. The probe asserts the mechanism is connected before it measures anything
(vary-the-knob check): same spec at rate 6 vs 24 gave 955.8 ms vs 1092.1 ms, a 14.3%
difference, so the knob has an EFFECT and not merely an existence.

## Measured cliffs

| Level | arrival CV | `r_nominal` (rps) | first SUPER rate |
|---|---|---|---|
| `constant` | 0.0 (deterministic) | **96** | 104 |
| `poisson` | 1.0 | **88** | 92 |
| `gamma_cv2` | 2.0 | **88** | 92 |
| `gamma_cv4` | 4.0 | **80** | 84 |
| `weibull_cv3_heldout` | 3.0 | **96** | 104 |

**Burstier traffic saturates strictly earlier**, monotonically in CV over the three
levels that share a family: `gamma_cv4` (80) < `gamma_cv2` (88) < `constant` (96). The
plan's launch assertion `cliffs["gamma_cv4"] < cliffs["poisson"]` holds **strictly**:
80 < 88, a margin of 8 rps (one full sweep step). The strict form matters — `<=` would
be satisfied by equal cliffs, which is precisely what an inert rate knob produces.

`weibull_cv3_heldout` is the held-out level and is **not** monotone with the gamma
family: its CV of 3.0 sits between `gamma_cv2` and `gamma_cv4`, yet its cliff (96) ties
`constant`'s, the *least* bursty level. CV alone therefore does not order capacity
across arrival *families* — the shape of the interarrival distribution matters, not just
its second moment. Worth stating plainly because it is a genuine result and not a
measurement artifact: Weibull's growth curve is the flattest of all five levels
(+1.3% at rate 60, still only +22.9% at 96), so its knee is well-resolved rather than
marginal. Being held out, it is never used to fit anything; it exists to test whether
a detector tuned on the gamma family transfers.

## Growth tables

Seed 42, `n=800` vs `n=3200`. `sub` iff growth ≤ 25%. The sweep stops at the first
SUPER rate, so each table's last row is the first super-capacity rate and the row above
it is the knee.

### `constant` — `r_nominal = 96` rps

| rate (rps) | E2E n=800 (ms) | E2E n=3200 (ms) | growth | regime |
|---|---|---|---|---|
| 60 | 1555.9 | 1568.3 | +0.8% | sub |
| 70 | 1745.4 | 1787.7 | +2.4% | sub |
| 76 | 1875.8 | 1950.6 | +4.0% | sub |
| 80 | 1968.6 | 2074.8 | +5.4% | sub |
| 84 | 2065.7 | 2214.2 | +7.2% | sub |
| 88 | 2166.6 | 2372.1 | +9.5% | sub |
| 92 | 2270.0 | 2551.7 | +12.4% | sub |
| 96 **(knee)** | 2374.5 | 2894.0 | +21.9% | sub |
| 104 | 2572.0 | 3975.0 | +54.6% | SUPER |

### `poisson` — `r_nominal = 88` rps

| rate (rps) | E2E n=800 (ms) | E2E n=3200 (ms) | growth | regime |
|---|---|---|---|---|
| 60 | 1485.8 | 1593.6 | +7.3% | sub |
| 70 | 1648.6 | 1821.4 | +10.5% | sub |
| 76 | 1759.7 | 1990.6 | +13.1% | sub |
| 80 | 1840.1 | 2120.7 | +15.3% | sub |
| 84 | 1924.4 | 2267.1 | +17.8% | sub |
| 88 **(knee)** | 2013.4 | 2434.3 | +20.9% | sub |
| 92 | 2105.3 | 2664.8 | +26.6% | SUPER |

### `gamma_cv2` — `r_nominal = 88` rps

| rate (rps) | E2E n=800 (ms) | E2E n=3200 (ms) | growth | regime |
|---|---|---|---|---|
| 60 | 1560.9 | 1626.6 | +4.2% | sub |
| 70 | 1747.8 | 1863.9 | +6.6% | sub |
| 76 | 1878.1 | 2042.5 | +8.8% | sub |
| 80 | 1969.1 | 2185.6 | +11.0% | sub |
| 84 | 2066.7 | 2357.7 | +14.1% | sub |
| 88 **(knee)** | 2167.0 | 2596.1 | +19.8% | sub |
| 92 | 2271.2 | 2992.4 | +31.8% | SUPER |

### `gamma_cv4` — `r_nominal = 80` rps

| rate (rps) | E2E n=800 (ms) | E2E n=3200 (ms) | growth | regime |
|---|---|---|---|---|
| 60 | 1593.5 | 1752.1 | +10.0% | sub |
| 70 | 1768.8 | 2023.4 | +14.4% | sub |
| 76 | 1894.2 | 2241.8 | +18.3% | sub |
| 80 **(knee)** | 1985.0 | 2424.1 | +22.1% | sub |
| 84 | 2082.0 | 2645.8 | +27.1% | SUPER |

### `weibull_cv3_heldout` — `r_nominal = 96` rps

| rate (rps) | E2E n=800 (ms) | E2E n=3200 (ms) | growth | regime |
|---|---|---|---|---|
| 60 | 1591.7 | 1612.3 | +1.3% | sub |
| 70 | 1786.6 | 1822.1 | +2.0% | sub |
| 76 | 1918.5 | 1978.7 | +3.1% | sub |
| 80 | 2009.8 | 2097.4 | +4.4% | sub |
| 84 | 2105.0 | 2233.4 | +6.1% | sub |
| 88 | 2200.8 | 2395.5 | +8.9% | sub |
| 92 | 2293.8 | 2602.7 | +13.5% | sub |
| 96 **(knee)** | 2385.5 | 2931.1 | +22.9% | sub |
| 104 | 2564.0 | 3996.6 | +55.9% | SUPER |

### Is 25% a good discriminator here?

Yes, on this evidence, though not by a wide margin on every level. The growth statistic
rises smoothly and then turns sharply upward, and the threshold is crossed on the sharp
part rather than the smooth part: the step from knee to first-SUPER roughly doubles or
more the growth for four of five levels (`poisson` 20.9% → 26.6%, `gamma_cv2`
19.8% → 31.8%, `constant` 21.9% → 54.6%, `weibull` 22.9% → 55.9%), and `gamma_cv4`
goes 22.1% → 27.1%.

The honest caveat: `poisson` (20.9 → 26.6) and `gamma_cv4` (22.1 → 27.1) straddle the
25% line with only a few points of margin, so on those two levels a threshold of, say,
30% would move the knee up one sweep step (to 92 and 84 respectively). The threshold was
**not** tuned to make the levels come out tidy — it is the plan's pre-registered 0.25 and
was never changed. The reason the resulting ordering is trustworthy anyway is that it
does not depend on the threshold: at *every* rate probed in common the growth is
ordered `gamma_cv4 > gamma_cv2 > constant` (rate 60: 10.0% > 4.2% > 0.8%; rate 70:
14.4% > 6.6% > 2.4%; rate 76: 18.3% > 8.8% > 4.0%; rate 80: 22.1% > 11.0% > 5.4%; rate 84:
27.1% > 14.1% > 7.2%), so the burstier-saturates-earlier conclusion survives any threshold
in a wide band. What a
different threshold would shift is the absolute `r_nominal` values, uniformly, and every
rung is defined as a *multiple* of its own level's `r_nominal` — so a uniform shift moves
the whole ladder together and preserves each rung's regime.

`constant` and `weibull_cv3_heldout` jump 21.9% → 54.6% and 22.9% → 55.9% across the
96 → 104 step. That step is 8 rps wide where the earlier grid steps are 4 rps, so their
knees are the most *decisively* super on the far side but the most *coarsely bracketed*: the
true knee for each lies somewhere in (96, 104]. A finer grid would resolve it, and would
not change the ordering.

**One grid-resolution artifact to record honestly.** `poisson` and `gamma_cv2` are assigned
the SAME cliff (88), yet at every rate probed in common `poisson` has the *higher* growth
(rate 80: 15.3% vs 11.0%; rate 84: 17.8% vs 14.1%; rate 88: 20.9% vs 19.8%). On the
continuous statistic `poisson` is therefore slightly *closer* to saturation than
`gamma_cv2` at equal load — the opposite of the CV ordering — and the tie in `cliffs.json`
is an artifact of both crossing 25% in the same 88→92 grid interval, not evidence that
their capacities are equal. Two consequences: (a) the `gamma_cv4 < poisson` assertion is
the right one to gate on (it compares levels a full sweep step apart, not two that tie),
and (b) CV does not cleanly order capacity even *within* the sub-Poisson-to-gamma range at
this grid resolution. The gamma_cv2/poisson pair should not be used as a fine-grained
burstiness contrast; `constant` vs `gamma_cv4` (96 vs 80) is the contrast with real
separation.

## Ladders

Rung multipliers reuse `LADDER.md`'s bands unchanged — calibration `0.3–0.6`, gray
`0.7–0.95`, cliff `1.0`, super `1.1–2.0`. What changes per level is only the anchor:
**every rung is a multiple of its OWN level's measured `r_nominal`**, never of a shared
constant. That is the whole point of this task — anchoring the gamma ladders to Poisson's
cliff would place their base rungs relatively higher against their own (earlier) cliffs.

### `constant` — anchored to `r_nominal = 96` rps

| Band | Multipliers | Rates (rps) | Role |
|---|---|---|---|
| Calibration (§3.4) | 0.3, 0.4, 0.5, 0.6 | 28.8, 38.4, 48, 57.6 | FPR budget measured HERE only |
| Gray zone (§3.4) | 0.7, 0.8, 0.9, 0.95 | 67.2, 76.8, 86.4, 91.2 | charged to no budget |
| Cliff | 1.0 | 96 | the knee itself |
| Super-capacity | 1.1, 1.25, 1.5, 2.0 | 105.6, 120, 144, 192 | T1 must fire; T4 scored here |

### `poisson` — anchored to `r_nominal = 88` rps

| Band | Multipliers | Rates (rps) | Role |
|---|---|---|---|
| Calibration (§3.4) | 0.3, 0.4, 0.5, 0.6 | 26.4, 35.2, 44, 52.8 | FPR budget measured HERE only |
| Gray zone (§3.4) | 0.7, 0.8, 0.9, 0.95 | 61.6, 70.4, 79.2, 83.6 | charged to no budget |
| Cliff | 1.0 | 88 | the knee itself |
| Super-capacity | 1.1, 1.25, 1.5, 2.0 | 96.8, 110, 132, 176 | T1 must fire; T4 scored here |

### `gamma_cv2` — anchored to `r_nominal = 88` rps

| Band | Multipliers | Rates (rps) | Role |
|---|---|---|---|
| Calibration (§3.4) | 0.3, 0.4, 0.5, 0.6 | 26.4, 35.2, 44, 52.8 | FPR budget measured HERE only |
| Gray zone (§3.4) | 0.7, 0.8, 0.9, 0.95 | 61.6, 70.4, 79.2, 83.6 | charged to no budget |
| Cliff | 1.0 | 88 | the knee itself |
| Super-capacity | 1.1, 1.25, 1.5, 2.0 | 96.8, 110, 132, 176 | T1 must fire; T4 scored here |

### `gamma_cv4` — anchored to `r_nominal = 80` rps

| Band | Multipliers | Rates (rps) | Role |
|---|---|---|---|
| Calibration (§3.4) | 0.3, 0.4, 0.5, 0.6 | 24, 32, 40, 48 | FPR budget measured HERE only |
| Gray zone (§3.4) | 0.7, 0.8, 0.9, 0.95 | 56, 64, 72, 76 | charged to no budget |
| Cliff | 1.0 | 80 | the knee itself |
| Super-capacity | 1.1, 1.25, 1.5, 2.0 | 88, 100, 120, 160 | T1 must fire; T4 scored here |

### `weibull_cv3_heldout` — anchored to `r_nominal = 96` rps

| Band | Multipliers | Rates (rps) | Role |
|---|---|---|---|
| Calibration (§3.4) | 0.3, 0.4, 0.5, 0.6 | 28.8, 38.4, 48, 57.6 | FPR budget measured HERE only |
| Gray zone (§3.4) | 0.7, 0.8, 0.9, 0.95 | 67.2, 76.8, 86.4, 91.2 | charged to no budget |
| Cliff | 1.0 | 96 | the knee itself |
| Super-capacity | 1.1, 1.25, 1.5, 2.0 | 105.6, 120, 144, 192 | T1 must fire; T4 scored here |

## Launch gate: every ladder crosses its OWN cliff

`LAUNCH-CHECKLIST.md` item 2: *"A ladder that never tips makes its test vacuous. A base
rung that already tips makes it unfalsifiable."* Both ends are checked, and checked
**empirically** — the arithmetic fact that `0.3 × r < r < 2.0 × r` is a tautology and
proves nothing, so each end rung was actually run through the same two-horizon growth
test as the sweep:

| Level | cliff | base rung 0.3x | measured growth | top rung 2.0x | measured growth |
|---|---|---|---|---|---|
| `constant` | 96 | 28.8 | -0.9% sub | 192 | +168.6% SUPER |
| `poisson` | 88 | 26.4 | +1.9% sub | 176 | +170.5% SUPER |
| `gamma_cv2` | 88 | 26.4 | -0.3% sub | 176 | +170.4% SUPER |
| `gamma_cv4` | 80 | 24 | +3.4% sub | 160 | +163.0% SUPER |
| `weibull_cv3_heldout` | 96 | 28.8 | +1.2% sub | 192 | +171.0% SUPER |

Every base rung is firmly sub-capacity (−0.9% to +3.4% growth, an order of magnitude
below the 25% line) and every top rung is firmly super (+163% to +171%, far above it).
No ladder is unfalsifiable at its base and none is vacuous at its top.

## Reproduction

```bash
python3.11 campaign/probes/measure_cliffs.py > campaign/apparatus/cliff-sweep.log 2>&1
```

Writes `cliffs.json` (the `{level: r_nominal}` table the adapter reads) and
`cliff-tables.json` (the full per-rate growth rows reproduced above). Deterministic at
seed 42. Requires `python3.11` (plain `python3` lacks `yaml`) and a built `./blis`.
