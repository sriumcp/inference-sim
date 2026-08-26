#!/usr/bin/env python3
"""Behavioral tests for the campaign adapter -- the code where every defect has lived.

WHY THIS FILE EXISTS. The detector has 8 behavioral tests and its statistics were verified
before it shipped. The ADAPTER had none, and six of the seven campaign defects so far were in
it, each costing a launched epoch to discover:

  1. calibration_grid was described in a comment but never written  -> killed a screen
  2. NOUS_WORKLOAD_SEED declared in the campaign, never read        -> blocked certification
  3. at.yaml.template carried a superseded design's keys            -> aborted every row
  4. DS1 duplicated a constraint, so feasible rows were rejected    -> discarded real data
  5. mean_lead_mult indexed levels quick mode does not score        -> raised mid-row
  6. null-valued keys made the contract unpoolable                  -> ended an epoch

Every one is a BEHAVIORAL property -- "does this function return the right thing for this
input" -- and every one is cheap to assert. None needed a campaign to find.

These tests read only public behaviour: function returns, emitted keys, and the campaign YAML
as data. They assert no private state and reimplement no formula, so they survive a rewrite of
the adapter's internals.

Run: python3 campaign/bench/test_adapter.py
"""
import json
import os
import pathlib
import re
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[1]
sys.path.insert(0, str(HERE))

FAILURES = []


def check(name, ok, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"   {detail}" if detail else ""))
    if not ok:
        FAILURES.append(name)


# --- Defect 1: a knob grid that exists only in a comment -----------------------------------
def test_every_detector_has_a_reachable_knob_grid():
    """Every detector the adapter scores must resolve to a non-empty grid.

    The anytime grid was documented in a comment as "resolved in calibration_grid()", a
    function that did not exist -- so every anytime row died on KeyError inside the FPR
    calibration. Asserting resolution for EVERY name makes that unrepresentable.
    """
    import score_anytime2 as S
    for det in S.STATIC + ["anytime"]:
        grid = S.KNOB_GRIDS.get(det)
        check(f"knob grid resolves for {det!r}",
              isinstance(grid, (list, tuple)) and len(grid) >= 2,
              f"{len(grid) if grid else 0} levels")


# --- Defect 2: a declared config field with no reader --------------------------------------
def test_workload_seed_env_actually_changes_the_seeds():
    """The campaign declares workload.seed_env; the adapter must READ it.

    It did not: SEEDS was hardcoded, so every confirm replicate ran identical seeds on a
    deterministic simulator, sd=0, and no t-interval could be formed. A populated config field
    with no reader looks correct in every artifact and does nothing.

    Asserted behaviorally -- different env value must yield a different seed set -- rather than
    by grepping for the variable name, which a rename would defeat.
    """
    import importlib
    import score_anytime2 as S
    seen = {}
    for offset in ("0", "1", "2"):
        os.environ["NOUS_WORKLOAD_SEED"] = offset
        importlib.reload(S)
        seen[offset] = tuple(S.SEEDS)
    os.environ.pop("NOUS_WORKLOAD_SEED", None)
    importlib.reload(S)
    check("distinct NOUS_WORKLOAD_SEED gives distinct seed sets",
          len(set(seen.values())) == 3, f"{seen['0'][:2]} vs {seen['1'][:2]} vs {seen['2'][:2]}")


# --- Defect 4: an observable used as both invariant and constraint -------------------------
def test_no_observable_is_both_invariant_and_constraint():
    """An invariant REJECTS a row; a constraint marks it INFEASIBLE and keeps it.

    Declaring one metric as both means a configuration that merely violates a bound is
    discarded rather than recorded -- which destroyed exactly the data the campaign exists to
    collect. The two have opposite semantics, so no metric may serve both roles.
    """
    campaign = REPO / "campaign" / "anytime-robust.yaml"
    text = campaign.read_text()
    constraints = set(re.findall(r"- \{metric: (\w+), op:", text))
    invariants = set(re.findall(r'observable: "(\w+)"', text))
    overlap = constraints & invariants
    check("no metric is both a constraint and an invariant",
          not overlap, f"overlap: {sorted(overlap)}" if overlap else "disjoint")


# --- Defects 5 and 6: the emitted contract ------------------------------------------------
def test_emitted_contract_is_complete_and_numeric():
    """Every key the campaign reads must be present, and every value numeric.

    Two defects here. mean_lead_mult indexed all FITTED levels unconditionally and raised in
    quick mode, which scores a reduced set. And null-valued keys made the response contract
    change TYPE between rows (float -> null), which the drift guard correctly refuses to pool --
    it ended an epoch in a retry loop.

    Runs the adapter for real in quick mode, so it exercises the same path a row does.
    """
    # Parsed by line-scan rather than with PyYAML: `run_command` invokes `python3`, which on
    # this box has no yaml module, and a test that needs a different interpreter than the rows
    # do is testing something the campaign never runs.
    text = (REPO / "campaign" / "anytime-robust.yaml").read_text()
    required = set(re.findall(r"\{metric: (\w+),", text))          # primary, constraints, regimes, self_check, ceiling
    required |= set(re.findall(r'observable: "(\w+)"', text))      # design_space invariants
    for block in ("held_out", "constant_fields"):
        m = re.search(rf"{block}: \[([^\]]*)\]", text)
        if m:
            required |= {x.strip() for x in m.group(1).split(",") if x.strip()}

    tmpl = REPO / "campaign" / "apparatus" / "at.yaml.template"
    cfg = REPO / "at.yaml"
    cfg.write_text(tmpl.read_text())
    out = subprocess.run(
        [sys.executable, str(HERE / "score_anytime2.py"), "--anytime-config", str(cfg),
         "--target-fpr", "0.05", "--adapter-width", "3", "--quick", "--num-requests", "300"],
        capture_output=True, text=True, timeout=2400, cwd=str(REPO))
    if out.returncode != 0:
        check("adapter runs in quick mode", False, out.stderr[-300:])
        return
    d = json.loads(out.stdout)

    missing = sorted(required - set(d))
    check("every metric the campaign reads is emitted", not missing, f"missing: {missing}")

    nulls = sorted(k for k, v in d.items() if v is None)
    check("no emitted value is null", not nulls, f"null keys: {nulls}")

    nonnum = sorted(k for k in required if k in d
                    and not isinstance(d[k], (int, float, bool, str)))
    check("every required metric is a scalar", not nonnum, f"non-scalar: {nonnum}")


# --- The template guard, already mutation-tested, run here too -----------------------------
def test_template_matches_the_detector_surface():
    """at.yaml.template must declare exactly the keys the detector accepts.

    A stale template from a superseded design parses fine and aborts every row, because a
    config_patch never creates structure. Delegates to the existing probe so there is one
    implementation of the check.
    """
    r = subprocess.run([sys.executable, str(REPO / "campaign" / "probes" / "verify_template.py")],
                       capture_output=True, text=True, cwd=str(REPO))
    check("template matches the detector's config struct", r.returncode == 0,
          r.stdout.strip().splitlines()[0] if r.stdout else "")


if __name__ == "__main__":
    print("=== adapter behavioral tests ===")
    test_every_detector_has_a_reachable_knob_grid()
    test_workload_seed_env_actually_changes_the_seeds()
    test_no_observable_is_both_invariant_and_constraint()
    test_template_matches_the_detector_surface()
    test_emitted_contract_is_complete_and_numeric()
    print()
    print("ALL PASS" if not FAILURES else f"FAILED: {FAILURES}")
    sys.exit(1 if FAILURES else 0)
