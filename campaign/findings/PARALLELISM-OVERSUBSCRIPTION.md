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

## CORRECTION: the "three failed rows" were never failures

**An earlier version of this document claimed rows 15, 22 and 27 failed on
contention, and that the missing-data pattern therefore correlated with factor
levels. That claim was WRONG and is retracted.**

`runs/iter-N/failed_runs/row-N/` is every row's PRIVATE SCRATCH DIRECTORY, not a
failure record. Verified in nousko's source: `stage_runner.py` passes
`log_dir = runs/iter-<n>/failed_runs` to `make_config_runner`, and
`concurrency.py` derives each row's `NOUS_RUN_DIR` under that root. So the
directory appears for EVERY row the moment it starts, succeed or fail. The name
is misleading; its presence carries no information about the outcome.

Confirmed observationally: in epoch 3, `failed_runs/row-15|22|27` are exactly the
three rows CURRENTLY EXECUTING, and their contents grew (`cal-14.yaml` ->
`cal-14.yaml cal-6.yaml`) between two checks a minute apart -- the bisection
walking its grid, which is a live row, not a dead one.

What misled me: the same three row indices appeared in epoch 2, which I read as a
reproducible failure. They recur because the screen executes rows in a
pre-registered randomized order at a fixed seed (seed=2), so the SAME rows are
in flight at the same point in both epochs. That is determinism working, not a bug.

## What the oversubscription actually cost

The load-122 measurement is real and the fix is still correct: 3 rows x 8 workers
on 10 cores is genuinely 12x oversubscribed, and 3 x 2 = 6 is what was declared.
But the cost was WALL CLOCK, not corrupted data -- no row was lost, so there is no
level-correlated missing-data bias. Epoch 2 was stopped for a defensible reason
(the apparatus did not match its declaration) but a weaker one than I claimed.

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
