# `at.yaml` was referenced six times and existed nowhere

**Status:** fixed in Task 3 (`campaign/apparatus/at.yaml.template`, staged by the launch gate)
**Severity:** would have aborted every row of the campaign at row 1
**Class:** the "pointer names nothing" failure, third occurrence in this work

## What was wrong

`campaign/anytime-valid-detector.yaml` refers to `at.yaml` in six places:

| line | reference |
|---|---|
| 176 | `run_command: "python3 campaign/bench/score_anytime.py --anytime-config at.yaml ..."` |
| 315 | `apply: {kind: config_patch, path: at.yaml, pointer: /anytime/wrapped, ...}` |
| 340 | `apply: {kind: config_patch, path: at.yaml, pointer: /anytime/bound, ...}` |
| 364 | `apply: {kind: config_patch, path: at.yaml, pointer: /anytime/alpha, ...}` |
| 387 | `apply: {kind: config_patch, path: at.yaml, pointer: /anytime/dispersion_window_us, ...}` |
| 413 | `apply: {kind: config_patch, path: at.yaml, pointer: /anytime/indeterminate_policy, ...}` |

`find . -name at.yaml` returned nothing, and no task in `PLAN-anytime-valid.md` creates
it. Task 3 builds the adapter that READS it; Task 4's launch gate only *verifies* that
"each factor's `pointer` addresses a key already present in the `at.yaml` template the
build authors" — which assumes a template that no step produces.

## Why it is fatal rather than cosmetic

**A `config_patch` never creates structure.** An absent pointer aborts every row of that
factor's levels. With `at.yaml` absent entirely, all five factors point at nothing, so
every row of the campaign aborts — not row 60, row 1.

The failure would also be silent in the direction that matters. The adapter, handed a
nonexistent `--anytime-config`, resolves an EMPTY `cfg_resolved`. Every factor's
manipulation predicate (`cfg_resolved.wrapped == composite`, …) then compares against a
missing key and REJECTS the row. That is the identical shape as the epoch that emitted
`cfg.*` while the predicates read `cfg_resolved.*`, and it is the second half of the same
lesson: it is not enough for the adapter to emit the right *prefix*, the *leaves* must be
there too.

## The fix, and the guard that would have caught it

`campaign/apparatus/at.yaml.template` declares all five pointers plus the `threshold:`
line the adapter calibrates by rewriting. It lives under `campaign/apparatus/` (campaign-
owned, and never mutated in place by a patch) and the launch gate stages it:

```bash
cp campaign/apparatus/at.yaml.template at.yaml
```

`campaign/probes/verify_adapter_contract.py` now asserts, BEFORE running the adapter:

1. the template exists;
2. it declares a key for **every leaf** any factor's `manipulation.observable` names —
   derived from the YAML, so adding a factor without adding its key fails the probe;
3. it declares `threshold:`, or the per-row FPR knob cannot be calibrated by rewriting;
4. and after running, that every one of those leaves comes back in `cfg_resolved` **with
   its type preserved** (int `2000000`, not `2000000.0`, or `==` never compares equal).

The probe deliberately does NOT create the live `at.yaml` as a side effect. It stages a
copy in a temp dir and prints a NOTE if the repo-root file is missing, so the launch
gate's own staging step stays a real check rather than a vacuous one.

## Note for the build stage

`anytime:` is not yet a block `sim/saturation/config.go` knows, and
`LoadSaturationConfig` parses with `KnownFields(true)`. So the staged `at.yaml` is a HARD
PARSE ERROR until `config.go` gains the block, its resolver, its registry case and its
`blockOwners()` row. That is the build stage's work — the five keys are the surface the
factors have already committed to, and they cannot be renamed without editing the
campaign.
