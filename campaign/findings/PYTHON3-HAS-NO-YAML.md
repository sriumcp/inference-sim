# `run_command` names an interpreter that cannot import `yaml`

**Status:** worked around in Task 3 (the adapter imports no `yaml` at all)
**Severity:** would have aborted every row with `ModuleNotFoundError`
**Class:** the environment the campaign runs in is not the environment it was tested in

## What was wrong

`optimization.run_command` is:

```
python3 campaign/bench/score_anytime.py --anytime-config at.yaml --target-fpr 0.05 --adapter-width 2
```

On this box:

```
$ python3 -c "import yaml"
ModuleNotFoundError: No module named 'yaml'      # /opt/homebrew/bin/python3 -> 3.14
$ python3.11 -c "import yaml; print(yaml.__version__)"
6.0.3
```

`PLAN-anytime-valid.md` states the fact in its Tech Stack line ("`yaml` module required —
plain `python3` on this box lacks it") and every probe it writes is shebanged
`python3.11`. But `run_command` says `python3`, and nousko execs `run_command` as argv —
no shell, no PATH re-resolution, no fallback. Task 3's own adapter must read YAML (the
frozen burstiness specs, the patched `at.yaml`), so the obvious implementation imports
`yaml` at module scope and every row dies on line 1.

## Why the plan's own probe would not have caught it

The plan's draft of `verify_adapter_contract.py` invokes the adapter with
`sys.executable`. Run the probe under `python3.11` — which the plan instructs — and
`sys.executable` IS `python3.11`, so the adapter is tested under an interpreter no row
will ever use. The probe passes; the campaign aborts.

## The fix

Two independent changes, either of which suffices, both applied:

1. **The adapter imports no `yaml`, anywhere.** Both files it must rewrite have flat,
   line-oriented, top-level keys (`aggregate_rate:`, `num_requests:`, and the `anytime:`
   block's scalars), so a line rewriter is sufficient. `_rewrite_scalar` returns a FOUND
   COUNT and every call site asserts it is exactly 1 — a schema change that moves or
   renames the key is a hard error, never a silent no-op that leaves the frozen
   placeholder in place. (That silent-no-op shape is exactly
   `RATE-FLAG-IGNORED-UNDER-SPEC.md`: it would run every rung at the same load and
   fabricate a ladder.)
2. **The contract probe runs the adapter under the campaign's LITERAL interpreter**, read
   out of `run_command` rather than assumed — `campaign_interpreter()` splits
   `run_command` and asserts `shutil.which()` finds it — so the interpreter the rows will
   actually use is the one that gets tested. Changing `run_command` to `python3.11` would
   also fix the campaign; keeping the adapter yaml-free fixes it for any interpreter.

## The general lesson

The plan recorded the fact and then wrote a command that contradicts it, and its own
verification step was written in a way that could not see the contradiction. A probe that
reconstructs the production invocation from the config — instead of approximating it with
the interpreter that happens to be running the probe — is the check that closes this
class.
