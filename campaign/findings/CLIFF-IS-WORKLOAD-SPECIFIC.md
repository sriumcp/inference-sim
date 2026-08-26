# `r_nominal` is a property of the (workload, hardware) PAIR, and the sweep range was a
# third silent-fabrication defect

**Measured in Task 2, independently reproduced.** Two findings, one substantive and one a
plan defect of exactly the shape as the previous two.

## 1. The inherited cliff does not transfer

`LADDER.md` records `r_nominal = 20 rps` with +21% growth at n=800 -> n=3200. On the frozen
burstiness specs, the same rate gives:

```
rate= 20   n=800  1046.3 -> n=3200  1060.5    +1.4%   deeply SUB
rate= 88   n=800  2013.4 -> n=3200  2434.3   +20.9%   the knee
rate=176   n=800  3625.5 -> n=3200  9807.0  +170.5%   far SUPER
```

Cause, and it is physics rather than a bug: these specs' outputs are gaussian(mean 128,
sd 32, capped 256), so decode is far cheaper per request than the workload `LADDER.md`
measured. Capacity is ~4.4x higher. **`r_nominal` describes a (workload, hardware) pair,
not a hardware configuration** -- which is precisely why the plan required measuring it per
level instead of inheriting 20.

Had the inherited value been reused, every rung of every ladder would have sat an order of
magnitude below its own cliff: T1 could never fire, and the campaign would have measured a
detector's behaviour on exclusively healthy traffic while reporting it as a capacity study.

## 2. The sweep range was all-sub, and only the closed end was guarded

The plan's sweep was `[8, 10, ..., 26]` -- entirely sub-capacity for all five levels. The
loop's only guard was `knee is None`, which catches the OPPOSITE open end (every rate
already super). With every rate sub, `knee` simply holds the **last rate probed**, so
`find_cliff` returns **26 for all five levels and nothing raises**.

Same shape as `RATE-FLAG-IGNORED-UNDER-SPEC.md` and `DETECTION-DELAY-IS-CLIPPED.md`: a
measurement that cannot fail in the direction being tested.

Fixed: the range is now `[60..128]`, set from measured evidence and verified to bracket all
five cliffs, and the missing open-top guard is in place. Verified to BITE -- `find_cliff`
on the old all-sub range now raises:

> `poisson: the sweep NEVER left sub-capacity (top rate 20 still grows only +1.4%) -- so
> knee here is just the last rate probed, NOT a measured cliff.`

**The general rule:** a bracketing search needs a guard at BOTH ends. One-sided bracketing
silently returns an endpoint, and an endpoint is indistinguishable from a measurement.

## The measured ladder

| Level | CV | r_nominal | knee growth | first SUPER |
|---|---|---|---|---|
| `constant` | 0.0 | **96** | +21.9% | 104 (+54.6%) |
| `poisson` | 1.0 | **88** | +20.9% | 92 (+26.6%) |
| `gamma_cv2` | 2.0 | **88** | +19.8% | 92 (+31.8%) |
| `gamma_cv4` | 4.0 | **80** | +22.1% | 84 (+27.1%) |
| `weibull_cv3` (held out) | 3.0 | **96** | +22.9% | 104 (+55.9%) |

`gamma_cv4 < poisson` holds STRICTLY (80 < 88, an 8 rps margin -- two sweep steps, not a
rounding tie), so burstier traffic does saturate earlier, as the design predicted.

Every ladder was verified to cross its own cliff EMPIRICALLY rather than arithmetically
(`0.3r < r < 2.0r` is a tautology that proves nothing): each end rung was re-run through
the two-horizon test. Bases sit at -0.9% to +3.4% growth (an order of magnitude below the
25% line); tops at +163% to +171%. None vacuous, none unfalsifiable.

## Three caveats that must reach the campaign, not be smoothed away

**(a) The `poisson`/`gamma_cv2` tie at 88 is a grid artifact, and it points the WRONG way.**
At every common rate `poisson` shows the HIGHER growth (rate 80: 15.3% vs 11.0%; rate 88:
20.9% vs 19.8%) -- so on the continuous statistic Poisson is marginally CLOSER to saturation
than gamma CV=2, opposite to the CV ordering. They tie only because both cross 25% inside
the same 88->92 interval. **Do not use this pair as a fine-grained burstiness contrast.**
`constant` vs `gamma_cv4` (96 vs 80) is the pair with real separation.

**(b) Capacity is NOT monotone in CV across arrival FAMILIES.** `weibull_cv3` (CV 3.0) sits
between gamma_cv2 and gamma_cv4 by CV, yet its cliff (96) ties `constant`, the least bursty
level. So CV alone does not order capacity -- the family matters. This is a live risk for the
HELD-OUT level specifically: the pre-registered prediction is that observations-to-verdict
grows monotonically in the measured DISPERSION INDEX `I`, and if weibull's `I` does not sit
where its CV suggests, a held-out miss could be an artifact of the family rather than a
refutation of the mechanism. **Measure `I` per level directly** (Task 3) rather than
inferring it from CV, and report weibull's `I` alongside any held-out verdict.

**(c) The 25% threshold was NOT tuned, and the ordering does not depend on it.** `poisson`
(20.9 -> 26.6) and `gamma_cv4` (22.1 -> 27.1) straddle 25% by only a few points, so a 30%
threshold would move both knees one step up. The pre-registered 0.25 was kept unchanged. The
ordering is robust regardless: at every common rate growth is ordered
`gamma_cv4 > gamma_cv2 > constant`, and because every rung is a multiple of its OWN
`r_nominal`, a uniform threshold shift moves each ladder together and preserves rung regimes.

`constant` and `weibull` knees are the least precisely located: the 96->104 step is 8 rps
wide where earlier steps are 4, so their true knees lie in (96, 104].

## Operational note for Task 3

`.gitignore:44` is a repo-wide `*.json`, which silently excluded `cliffs.json` -- the exact
file `cliff_table_hash` pins and the adapter reads. It was force-added rather than editing
the shared `.gitignore` (outside `campaign/`). **Any new JSON artifact under `campaign/`
needs `git add -f`**, or it vanishes from the commit with no warning.
