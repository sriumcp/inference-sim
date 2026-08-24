# The composition guard was keyed on a variable that does not exist

**Epoch 2 stopped mid-screen and re-registered as epoch 3.** Recorded because the
defect is exactly the one the campaign guide warns about, I built a guard against
it, and the guard silently did nothing.

## What happened

The guide's warning is explicit: *"a real campaign whose adapter itself spawned 4
concurrent probes per row would have put 16 processes on a 10-CPU box."* The
adapter here DOES fan out internally (its calibration probes and per-rung seed
sweeps run concurrently), so `_inner_width()` was written to compose with the outer
width rather than multiply against it -- by dividing the core budget by
`$NOUS_MAX_PARALLEL`.

**`NOUS_MAX_PARALLEL` does not exist.** Verified against a live row's environment:
nousko exports `NOUS_RUN_DIR`, `NOUS_ROW_INDEX`, `NOUS_RUN_SLOT` and
`NOUS_WORKLOAD_SEED`. Nothing else. So the divisor defaulted to 1, every concurrent
row claimed the entire budget, and the box ran:

| | value |
|---|---|
| cores | 10 |
| concurrent rows (`max_parallel`) | 3 |
| BLIS workers PER row (measured) | 8 |
| total BLIS processes | ~24 |
| **load average** | **122** (12x oversubscribed) |

Measured directly: `ps` showed 8 `blis run` processes each for row-16, row-2 and
row-20 simultaneously.

## Why it corrupted measurements rather than merely being slow

Three rows (15, 22, 27) were recorded as failures. Each config REPRODUCES CLEANLY
standalone (exit 0, finite statistics, ~23s wall clock at n=6000 against the
adapter's 1200s ceiling -- a 52x margin). They failed on CONTENTION, not on
configuration.

A contention-failed row is missing data in the fit. Worse, contention is not
symmetric across the design: a row whose statistic needs more calibration probes
occupies the machine longer and is likelier to collide, so the missing-data pattern
CORRELATES WITH THE FACTOR LEVELS. That is the level-correlated-bias mechanism the
guide describes for changing a resource limit mid-epoch, arrived at accidentally.

Note what did NOT save us: `concurrency.load_independent` is still TRUE and was
verified (parallel output is bit-identical to serial). Load independence guarantees
a co-scheduled neighbour cannot change the NUMBER a row prints -- it does not
guarantee the row completes at all. Oversubscription attacks completion, not
correctness, which is why the declaration held while the epoch still degraded.

## The fix

Key on a variable that exists, and fail toward UNDER-subscription:

- `NOUS_ADAPTER_WIDTH` is now set explicitly in `run_command` (2), so the
  composition is declared in the campaign where a reader can audit it against
  `max_parallel: 3` -> 6 processes on 10 cores.
- Absent that, the adapter detects it is inside a row via `NOUS_RUN_SLOT` (which
  nousko really does export) and assumes an outer width of 3 rather than 1.
- A standalone probe (no `NOUS_RUN_SLOT`) still takes the full budget, which is
  correct -- there is no outer runner to compose with.

Verified: `2` inside a row with the explicit width, `2` inside a row without it
(was 8), `8` standalone.

## Generalizable lesson

An environment-variable-keyed guard is untestable by inspection: a typo and a
correct name are indistinguishable in code review, and both "work" -- one just does
nothing. The guard needed a POSITIVE assertion that the variable was present, not
a `.get(..., default)` that silently accepts absence. `_inner_width()` now uses
`NOUS_RUN_SLOT` presence as evidence it is inside a campaign, which is a fact about
the environment rather than a hope about it.

**Epoch 2's three rows are not discarded** -- they are preserved and are what
produced the `GUARDS-CAUGHT-IT.md` finding (the constraints correctly marking a
100%-FPR configuration infeasible). That finding is about the CONSTRAINTS and does
not depend on timing, so it survives the apparatus change. Epoch 3 re-measures the
response surface on a properly-sized box.
