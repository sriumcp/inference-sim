# Concurrency: measured under real screen load, and the sizing was inherited from the wrong
# cost model

**Checklist item 4 demands a POSITIVE assertion -- process count observed during a real row,
never inferred from the config.** Measured during epoch 1's `screen` stage (19 rows,
pre-registered randomized order, seed=4).

## The ceiling holds exactly

```
declared : max_parallel 3 x --adapter-width 2 = 6
observed : 6 blis processes
cores    : 10
```

So `--adapter-width` COMPOSES with `max_parallel` rather than multiplying against it. That is
the fix for the defect that destroyed a prior epoch (`PARALLELISM-OVERSUBSCRIPTION.md`): a guard
keyed on `$NOUS_MAX_PARALLEL`, a variable nousko never exports, defaulted its divisor to 1 and
put 24 processes at load 122 on 10 cores. Passing the width as a FLAG -- read from argv, asserted
present, never `os.environ.get(name, default)` -- is what makes the composition checkable.

## Load 10.86 is NOT our oversubscription

The load average alone would look alarming. The per-process breakdown says otherwise:

| consumer | %CPU |
|---|---|
| 4 visible blis processes | 44.8 / 43.7 / 30.2 / 27.9 |
| **total blis (all 6)** | **267.9** |
| CrowdStrike falcon agent | 44.5 |
| WindowServer | 42.9 |
| Lakeside lsiagentd | 33.0 |
| airportd | 32.3 |

**Our six processes use ~2.7 of 10 cores.** The rest is a co-tenant desktop environment
(endpoint security, window server, management agents). Each blis process gets ~45% of a core and
none is starved, so no row is at risk of dying on contention -- which is what would produce
LEVEL-CORRELATED BIAS, since configs needing more calibration probes hold the machine longer.

Note precisely what `concurrency.load_independent: true` does and does not buy: it guarantees a
neighbour cannot change the NUMBER a row prints (BLIS accumulates simulated time from trained
coefficients and reads no wall clock -- verified: width 1 vs 2 gave byte-identical output on all
20 response fields, differing only in `adapter_width` recording its own knob). It does NOT
guarantee the row FINISHES. Only the process-count measurement above speaks to that.

## The sizing is inherited, and from the wrong cost model

`max_parallel: 3` / `--adapter-width 2` came from `saturation-detector-rsm.yaml`, where a row
cost ~265s at a FIXED n=6000 horizon. **This campaign's rows are structurally cheaper**: an
anytime detector stops as soon as the evidence is decisive, which is the entire point. Sizing
carried over from a fixed-horizon campaign prices the wrong thing.

Measured adapter fan-out, which also matters:

| `--adapter-width` | peak blis procs |
|---|---|
| 1 | 2 |
| 2 | 2 |
| 4 | 4 |

**Widths 1 and 2 are indistinguishable** -- the adapter's inner concurrency does not scale
between them, so the declared 2 buys nothing over 1 on this workload.

## Why it was NOT changed mid-epoch

`max_parallel` is inside the compiled policy (`97557929819c`, epoch 1). Editing it is exactly the
mid-epoch apparatus change the hash check exists to refuse, and re-registering would discard the
`plan` and `build` work already spent. A ~40% wall-clock saving does not outrank the
pre-registration.

## Recommendation for epoch 2

`max_parallel: 4` with `--adapter-width 2` (~8 processes). Justification: the box has real
headroom (2.7 of 10 cores used by us), but the remainder is contested by system agents rather
than idle, so `4 x 4 = 16` would buy contention instead of throughput -- and 16 is within reach
of the 24-process configuration that produced load 122. Raise the OUTER width, not the inner one,
since the inner one measurably does not scale from 1 to 2.
