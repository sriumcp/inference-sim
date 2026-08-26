# `--rate` is silently ignored under `--workload-spec`, and it would have fabricated
# five capacity cliffs

**Found by the Task 1 agent while implementing the burstiness specs; independently
reproduced before acting on it.** A plan defect, not a spec defect, and the fifth
instance in this work of a measurement that cannot fail in the direction being tested.

## The finding

`blis run --workload-spec S --rate R` ignores `R` entirely. The spec's `aggregate_rate`
is the sole rate authority.

Reproduced on the committed poisson rung, same seed, same request count:

```
--rate 10   ->  stdout byte-identical
--rate 200  ->  stdout byte-identical      (diff -q reports no difference)
```

Cause, in `cmd/root.go`: `rate` is read only inside the two workload-SYNTHESIS branches
(the `--workload`/distribution paths, around lines 1678-1709). The `--workload-spec`
branch never consults it. `--num-requests` is likewise spec-authoritative; `--seed` IS
honoured as an override.

## Why this was going to be silent, and expensive

`PLAN-anytime-valid.md` Task 2's `mean_e2e()` varies load by passing `--rate`, and
`find_cliff` walks rates upward looking for the first one whose mean-E2E growth
`n=800 -> n=3200` exceeds 25%. With `--rate` inert, **every rung of every sweep runs at
`aggregate_rate`**, so:

- growth is ~flat at every probed rate (it is the same run five times over),
- no rate ever trips the SUPER test,
- `find_cliff` walks to the end of the sweep and returns the LAST rate as the knee,
- for all five burstiness levels, which then differ only by their arrival process at one
  fixed rate.

The result is five fabricated `r_nominal` values, each looking plausible, written into
`cliffs.json`, hashed into `cliff_table_hash`, and pinned as the apparatus every later row
is anchored to. Nothing would have raised. The `assert cliffs["gamma_cv4"] <=
cliffs["poisson"]` guard I put at the end of the probe would have PASSED, because equal
satisfies `<=` — a guard that cannot distinguish "measured and equal" from "never varied".

## The fix

A rung driver must **rewrite `aggregate_rate` in a copy of the spec**, never pass
`--rate`. Concretely, for each (level, multiplier) pair: load the frozen spec, set
`aggregate_rate = multiplier * r_nominal[level]`, write it to the row's private scratch
(`NOUS_RUN_DIR`), and run BLIS against that copy. The frozen spec under
`campaign/apparatus/burstiness/` is never mutated in place.

Every committed spec header now carries this instruction, so the next reader cannot repeat
it.

## The guard that catches the general case

The specific bug is fixed by not using `--rate`. The CLASS of bug -- a knob the driver
believes it is varying but is not -- needs a positive assertion, so Task 2 gains one
before its sweep runs:

> **Vary-the-knob check.** Before measuring anything, run the SAME spec at two rates that
> must differ observably, and assert the outputs DIFFER. If they do not, the rate mechanism
> is not connected and the sweep must abort rather than report.

This is the response-side sibling of `LAUNCH-CHECKLIST.md` item 5 ("every env var and
pointer verified to EXIST"): item 5 asserts the input exists, this asserts the input has
an EFFECT. An inert knob passes item 5 trivially.

## Pattern count, now five

| # | Defect | Could not fail because |
|---|---|---|
| 1 | T3 factor-B trap | inflated residency without reducing completed tokens/sec |
| 2 | Lindley-coherence bug | hand-built stream violated the queue recursion |
| 3 | "incumbents blind to T2-IN" | service time held FIXED while prompts scaled 32x |
| 4 | clipped detection delay | speed metric floored at 0 for anything faster than warm-up |
| 5 | **this** | the load knob the cliff sweep varies was never connected |

Four of the five were caught by checking the apparatus against a physical expectation
rather than by the apparatus reporting a problem. This one was caught by an implementer
reading the code path before trusting the flag -- which is the cheapest of the five, and
the only one caught BEFORE it produced a number.
