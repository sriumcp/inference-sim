# The 0.0 ms detection delay is a clipping artifact, and the tie it reported is not established

**Retracts the central quantitative claim of `THE-TIE-IS-REAL.md`.** Raised by the user,
who objected that a detector cannot correctly detect saturation "without seeing anything
at time 0". The objection is correct, and the defect is worse than a misreading: the
metric is structurally incapable of measuring what it was added to measure.

## The claim being retracted

`THE-TIE-IS-REAL.md` reported that `randomwalk` and `composite` are equivalent because
both post a **median detection delay of 0.0 ms** at matched FPR, and concluded the
candidate "matches but does not beat" the incumbent. That table's speed column is void.

## The mechanism

`campaign/bench/score_detector.py::detection_delay_us` discards the first
`warmup_frac = 10%` of records, then measures from the first SURVIVING record:

```python
start = int(len(records) * warmup_frac)
tail  = records[start:]
t0    = tail[0]["timestamp"]
for r in tail:
    if r["result"]["level"] in ("BACKLOGGED", "OVERLOADED"):
        return max(0, r["timestamp"] - t0)
```

If the detector is ALREADY FIRED when the scored window opens, the loop returns on its
first iteration and the value is `max(0, t0 - t0) == 0`. The `max(0, ...)` makes the
clipping silent rather than negative.

Measured over the real function on a 2000-record trace (`warmup_frac=0.1`, so the dead
zone is events 0-199):

| first fires at event | reported delay |
|---|---|
| 3 | **0 us** |
| 21 | **0 us** |
| 100 | **0 us** |
| 199 | **0 us** |
| 201 | 1000 us |
| 400 | 200000 us |

Every first-fire in the first 200 events reports exactly 0. The metric has a 200-record
dead zone at its origin.

## Why this specifically voids the tie

The function's OWN docstring records the discriminating measurement:

> "on the same 2000-request trace at 0.9x nominal, randomwalk first fires at event 21
> while composite first fires at event 3"

Events 3 and 21 are both inside the dead zone. So the two detectors were mapped to the
same clipped 0.0, and the honest reading of those numbers is the opposite of a tie:
**composite fires ~7x earlier than randomwalk.** The difference was measured, then
discarded by the metric built to capture it.

`THE-TIE-IS-REAL.md` opens by correctly diagnosing a ceiling effect in the rung rule and
adding a continuous metric to escape it. The new metric **moved the ceiling** from the
50%-fired test to the warm-up boundary instead of removing it. Both tied detectors sit
at 97-99% fired and fire within the first 1% of the run, so they saturate the old
ceiling and the new one.

## What is retracted, and what survives

RETRACTED:
- "median detection delay 0.0 ms" for randomwalk and composite (both clipped).
- "the tie is real" as a claim about SPEED. It is unmeasured, not equal.
- the derived claim that the candidate "matches composite" on lead time. On the only
  unclipped evidence available (event 21 vs event 3) it is SLOWER.

SURVIVES (unaffected -- none depends on the delay metric):
- `threshold` is genuinely slower: 6598 ms is far outside the dead zone.
- `backlog-drift` is broken: regret 340, fails T1/T2-OUT/T4, FPR 0.25.
- The horizon finding: separation grows 2.3x -> 14.6x from n=500 to n=8000.
- The gaming guards: a 100%-FPR config scoring best on regret was correctly rejected.

## The fix

Measure from the moment the DETECTOR starts observing, not from the moment the SCORER
starts looking, and report the quantity in events as well as microseconds:

1. `t0` = timestamp of `records[0]` -- the true observation start -- while still
   requiring the fired state to PERSIST past the warm-up so a cold-start blip is not
   scored as a detection.
2. Report `first_fire_event_index` alongside the microsecond value. An index cannot be
   clipped by a time origin, and it is the quantity the docstring was already using
   informally.
3. Emit an explicit `delay_clipped` flag when the first fire precedes the scored window,
   so a zero can never again be read as "instant" rather than "before measurement".
4. Add a self-check: a reported delay of exactly 0 with `delay_clipped=false` is a
   contradiction the adapter must surface, not print.

## The pattern this belongs to

Fourth occurrence in this work of ONE failure mode -- **an author-built measurement that
cannot fail in the direction being tested**:

| # | Defect | Could not fail because |
|---|---|---|
| 1 | T3 factor-B trap | inflated residency without reducing completed tokens/sec |
| 2 | Lindley-coherence bug | hand-built stream violated the queue recursion |
| 3 | "incumbents blind to T2-IN" | service time held FIXED while prompts scaled 32x |
| 4 | **this** | speed metric clipped to 0 for anything faster than the warm-up |

Each was found by checking the apparatus against a physical expectation, never by the
apparatus reporting a problem. `LAUNCH-CHECKLIST.md` item 6 already bans synthetic
streams from cross-detector claims; the missing sibling rule is:

> **A metric that saturates at either endpoint cannot rank the things that reach it.**
> Before reporting a comparison, verify the winner's score is INTERIOR to the metric's
> range -- the same rule the campaign already enforces for a calibrated knob
> (`knob_at_grid_edge`), applied to the response instead of the factor.

The campaign's `knob_at_grid_edge_num <= 0` constraint exists for exactly this reason on
the factor side. Nothing enforced it on the response side, so a clipped response was
reported as a finding.

## Consequence for the anytime-valid epoch

That epoch's PRIMARY objective is observations-to-confident-verdict -- a speed metric.
Launching it on this delay function would have produced a fifth instance, with a
guaranteed floor of 0 for any detector fast enough to matter. The metric is fixed and
its interior-range self-check is in place BEFORE that epoch's apparatus is frozen.
