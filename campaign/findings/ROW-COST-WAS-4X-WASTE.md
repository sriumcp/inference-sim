# The adapter recomputed the static reference in every row: 4/5 of the work was waste

**Found by measuring why the pre-flight had produced 6 levels in 53 minutes and then nothing
for 41.** Not a hang -- the adapter was alive and BLIS was cycling. The rows were simply
enormous, and most of each one was repeated work.

## The arithmetic

`score_anytime2.py` scored the anytime detector AND all four shipped static detectors inside
every row. Per detector, per row:

| stage | runs |
|---|---|
| calibration (4 CAL mults x 5 levels x 5 seeds) | 100 |
| ladder (8 mults x 5 levels x 5 seeds) | 200 |
| **per detector** | **300** |
| **x 5 detectors** | **1500 BLIS runs per row** |

And the calibration walk may sweep up to 7 knob values, so 1500 is the floor rather than the
ceiling.

**The waste:** the four static detectors' scores depend on the frozen ladder and their own
calibration grids -- and on NOTHING this campaign varies. ALPHA, KAPPA and LATCH are anytime-only
knobs. So the same four reference measurements were being recomputed identically for each of
~20 rows. Four fifths of the total work, repeated: roughly 40 hours where 8 would do.

## The fix, and why the cache key matters

Compute the static reference ONCE, cache it beside the frozen apparatus, reuse it.

The key covers every input the reference legitimately depends on: FPR budget, horizon, ladder
resolution (quick vs full), seeds, the per-level measured cliffs, and the detector set. It
deliberately does NOT cover the anytime factors -- that independence is precisely what makes
caching sound, and it is a property of the code rather than an assumption: `score_detector()`
for a static detector reads only its own `KNOB_GRIDS` entry and the frozen ladder.

A key covering less than that would be the dangerous version: it would silently serve a
reference measured against a different ladder or horizon, which is worse than the waste it
saves. That is the same class as the stale-binary defect -- plausible numbers from the wrong
inputs.

Cached under `campaign/apparatus/` rather than in `NOUS_RUN_DIR`, because that directory is
private to one row: a cache there would never be hit and the waste would persist silently.

## Why this belongs in the findings rather than just a commit

It is the fourth instance in this work of the same shape -- **an apparatus cost that looked like
a measurement.** The pre-flight was not broken and nothing failed; the instrument was simply
doing four times the necessary work, and the only symptom was slowness. Slowness is easy to
attribute to the problem being hard.

The check that would have caught it earlier: **before launching, compute the row's BLIS-run
count from the design and ask whether every one of those runs depends on something the campaign
varies.** Any run that does not is either apparatus (cache it) or waste (delete it).
