# Launch checklist: what must be TRUE before `nous run`

Three epochs were stopped for apparatus defects, and **all three were preventable at
launch**. Each cost hours of measurement that had to be discarded, not because the
candidate was wrong but because the instrument was incomplete when I started it.

| epoch | policy hash | stopped because | why it was preventable |
|---|---|---|---|
| e1 | `890dfd5a` | horizon n=500 -- the worst point on R_t's separation curve | R_t vs n was never measured before declaring the horizon |
| e2 | `d70dc05d` | 24 BLIS procs at load 122 on 10 cores | the composition guard was keyed on `$NOUS_MAX_PARALLEL`, a name never verified to exist |
| e3 | `83bc1b67` | scored only 2 of 4 tests | T2-IN/T2-OUT were wired in AFTER launch, so rows measured a superseded objective |

The common cause is not carelessness about any one field. It is launching before the
INSTRUMENT was finished, on the assumption that a defect found later could be patched.
It cannot: an apparatus change is an epoch boundary, so every mid-epoch discovery
throws away the whole epoch.

## The checklist

Run every item and record the evidence. An unchecked item is a discarded epoch.

**1. The objective is FINAL.**
- [ ] Every test the goal names is wired into the adapter and appears in its output.
- [ ] `regret` weights are settled; no test will be added later.
- [ ] Evidence: one adapter run showing every `*_pass` key the objective consumes.

**2. Every ladder crosses the cliff.**
- [ ] For each ladder, the base rung is sub-capacity AND the top rung is
      super-capacity, by the §1.1 ground-truth test (does mean E2E keep GROWING with
      the horizon?).
- [ ] A ladder that never tips makes its test vacuous. A base rung that already tips
      makes it unfalsifiable.
- [ ] Evidence: the growth table, per ladder.

**3. The horizon is measured, not chosen.**
- [ ] The statistic's discrimination has been measured AS A FUNCTION of run length,
      and the declared horizon is on the plateau, not the slope.
- [ ] Evidence: separation vs n, plus seed spread at the chosen n.

**4. Concurrency is bounded by a POSITIVE assertion.**
- [ ] The adapter's fan-out composes with `max_parallel` via a value it is GIVEN
      (a flag), never one it hopes to read from the environment.
- [ ] Verified by counting processes during a real row, not by reading the code.
- [ ] Evidence: `rows x workers` observed, and the load average.

**5. Every env var and pointer is verified to EXIST.**
- [ ] Any `os.environ.get(name, default)` in the adapter: `name` confirmed present in
      a live row's environment.
- [ ] Every `config_patch` pointer addresses a key already in the template (a patch
      never creates structure).
- [ ] Evidence: `ps eww` on a live row; `--smoke` passing.

**6. No synthetic stream underpins a cross-detector claim.**
- [ ] Any comparison between detectors comes from the SIMULATOR, at a matched FPR --
      never from hand-built events.
- [ ] Rationale: three false conclusions in this work came from author-built streams
      (the T3 factor-B trap; the Lindley-coherence bug; the "incumbents are blind to
      T2-IN" claim, which held service time fixed while scaling prompt tokens).
      Hand-built streams are for ABLATIONS WITHIN one statistic, where both legs share
      the stream.

**7. Pre-flight, both stages.**
- [ ] `nous validate campaign FILE --smoke` -- catches manipulation predicates that
      can never match, unmatched native tests, an unexecutable `run_command`.
- [ ] `... --smoke --liveness` -- the ONLY way to catch a level that aborts the target
      and a factor whose effect is below the noise floor.
- [ ] Evidence: both outputs, with the effect table read and acted on.

`--smoke` alone has already caught three campaign-killing defects here (`cfg.*` vs
`cfg_resolved`; the missing `/randomwalk/horizon_ratio` pointer; the `VAR=value`
prefix in `run_command`, which is exec'd as argv). It is cheap and it is not optional.

## The rule behind the checklist

**Launch only when a mid-epoch discovery would be a surprise, not a matter of time.**
If any part of the instrument is known-incomplete, finishing it costs minutes;
launching without it costs the epoch.
