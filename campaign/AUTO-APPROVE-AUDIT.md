# Auto-approve audit: what an unattended run does and does not protect

**Why this file exists.** `kind: optimization` **auto-approves by default** -- `nous run
--help`: *"optimization campaigns auto-approve by default (no per-stage human decision
changes the pure-Python stage rule)"*. So `--auto-approve` is redundant here, and the
question is not whether to pass the flag but whether this campaign is SAFE to run without
a human at the gates.

The gates skipped are `HUMAN_DESIGN_GATE` and `HUMAN_FINDINGS_GATE`, which the README calls
*"nous's primary safety mechanisms for catching design-agent deviations from campaign
intent"*. The pure-Python stages (`screen`/`refine`/`confirm`) are deterministic, so there
is genuinely nothing for a human to decide there. **But this campaign declares two
MODEL-FACING stages, `plan` and `build`** -- which is exactly where an agent can deviate.
So the interesting risk is not the search; it is the build.

## The five preconditions (README #255 / F10), checked

| # | Precondition | Status | Evidence |
|---|---|---|---|
| 1 | `locked_parameters` for every spec-critical knob | **MET** | ~20 keys declared; nous hard-fails any bundle whose `verified_parameters` contradicts one, *regardless* of auto-approve |
| 2 | `locked_workload` if there is a canonical workload | **OPEN** | the burstiness ladder IS canonical; blocked on Task 2 finishing (see below) |
| 3 | Target docs do not contradict the locked spec | **MET, conditionally** | see below -- the condition is a launch procedure, not a YAML field |
| 4 | Apparatus checks validate ATTRIBUTION, not just totals | **MET** | `response_interior_num`, `cs_coverage_ok_num`, `gamma_within_support_num` check the mechanism's OWN claims, not just the objective |
| 5 | Stale `principles.json` acceptable | **N/A** | auto-approve never gates on it |

## Precondition 3, and the silent-degradation path inside it

The concern is real for this target: BLIS's own `CLAUDE.md:558` documents peak-rate's
prior-epoch validation *verbatim*, including the horizon numbers (2.3x at n=500, 14.6x at
n=8000) and *"Validated by an optimization campaign (5 seeds x 11 load rungs,
false-alarm-calibrated first)"*. Nothing there contradicts our locked values, but a build
agent reading it could reasonably conclude the horizon question is SETTLED -- when deriving
the horizon is this campaign's entire point.

The README says the methodology prompt's "campaign > target-repo-docs" hierarchy covers
this, *"but a stale methodology prompt would not"*. Verified present:
`prompts/methodology/design.md:11`, the §247/F2 block -- and its worked example is literally
BLIS's `CLAUDE.md` showing `qwen/qwen3-14b` against a campaign pinning llama.

Verified it reaches the BUILD stage, not just a design stage:
`orchestrator/sdk_dispatch.py:54 _load_methodology_preamble` *"concatenates the design +
execute_analyze methodology files"*, and `build.py:544` loads it as the build's
`system_prompt`.

**The condition, and it is a LAUNCH PROCEDURE rather than a YAML field:**
`prompts.methodology_layer: prompts/methodology` resolves relative to the **nousko repo**,
not the target. Measured:

```
/Users/sri/Documents/Projects/agentic-strategy-evolution/prompts/methodology  EXISTS
<target worktree>/prompts/methodology                                        missing
```

And `build.py:545` treats a missing preamble as a **warning, not a fatal**
(`"a missing preamble must not be fatal"`). So invoking `nous run` from the wrong directory
silently strips the source-of-truth hierarchy from the build's system prompt, leaving
precondition 3 unmet with nothing but a log line to say so.

> **Launch rule: run `nous run` from `/Users/sri/Documents/Projects/agentic-strategy-evolution`.**
> Before launching, confirm the preamble loaded -- a `could not load methodology preamble`
> warning in the build log means precondition 3 is VOID and the run must be stopped.

## Precondition 2 is still open

`locked_workload` (#265 / F20) makes the validator diff `bundle.inputs/*.yaml` against the
canonical workload, with deliberate deviations requiring
`bundle.workload_changes_from_canonical`. Our burstiness ladder is canonical by
construction -- five specs whose whole purpose is to be identical except for the arrival
block.

Deferred to after Task 2 on purpose: the specs' `aggregate_rate` is a documented
PLACEHOLDER until the per-level cliffs are measured, so declaring the workload canonical
now would pin a value Task 2 is about to replace. Declaring it before launch is a launch
gate; declaring it now would be locking a number known to be wrong.

## The watchdog, since nobody is watching

Even under auto-approve every design gate writes a deterministic `campaign_spec_diff`
(#249 / F4):

```bash
jq '.campaign_spec_diff' "$NOUS_CAMPAIGN_PARENT"/<run>/runs/iter-*/gate_summary_design.json
```

A non-empty `locked_parameters_violations` means F1's hard-fail triggered. This will be
wired to a live `Monitor` at launch so a violation surfaces the moment it happens rather
than at the end of the run -- the README's own suggested mitigation
(*"invoke an external watchdog process to compare bundles against your campaign spec"*).

## What this campaign exercises

Worth stating, because it is the reason to run this one unattended at all. It touches paths
a simpler campaign does not:

- the `plan` -> `build` seam, where the guide records THREE defects that survived until a
  real campaign ran it end to end -- all invisible from the artifacts, one a
  26,833-character hole where the plan never reached the build prompt;
- `guidance.factor_nomination` -> build prompt, the field test where guidance reached
  nobody and the build shipped an already-diagnosed defect;
- `screen` falsifying the `plan`'s own `cost_avoided > cost_of_deciding` prediction;
- `locked_parameters`' hard-fail, with ~20 pinned knobs and a build agent free to drift;
- seven constraints on a mechanism with a NEW way to game the objective (answer
  INDETERMINATE forever, which `indeterminate_forever_num` closes).
