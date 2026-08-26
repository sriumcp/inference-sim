#!/usr/bin/env python3.11
"""Every key the campaign YAML reads must appear in the adapter's output.

This is the check whose absence cost an epoch twice: predicates read cfg_resolved.* while
the adapter emitted cfg.* (every row REJECTED), and a factor pointer named nothing in the
template (every row of that level ABORTED). Derive the required set FROM THE YAML so the
two cannot drift.

Two additions against the plan's draft, both forced by facts verified against the tree:

1. `sys.executable` is NOT used to invoke the adapter. The campaign's `run_command` is
   `python3 campaign/bench/score_anytime.py ...`, and plain `python3` on this box has no
   `yaml` module, so invoking the adapter through the probe's own interpreter would test
   an interpreter no row will ever use. The probe runs the adapter under BOTH the
   campaign's literal interpreter AND python3.11, so an interpreter-availability defect
   is caught here rather than at row 1. See INTERPRETER_CHECK below.

2. The adapter is run in `--incumbent-only` mode. `sim/saturation/anytime.go` does not
   exist yet -- it is authored by the campaign's own `build` stage -- so there is no
   anytime detector to score. An adapter that quietly produced anytime numbers today
   would be fabricating them. `--incumbent-only` makes that explicit, and the probe
   asserts the adapter REFUSES the anytime path while the detector is absent rather than
   degrading to a silent fallback.
"""
import json
import os
import pathlib
import re
import shutil
import subprocess
import tempfile
import sys

import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
CAMPAIGN = REPO / "campaign" / "anytime-valid-detector.yaml"
ADAPTER = REPO / "campaign" / "bench" / "score_anytime.py"


def required_keys():
    d = yaml.safe_load(CAMPAIGN.read_text())
    o, r = d["optimization"], d["optimization"]["response"]
    keys = {r["primary"]["metric"]}
    keys |= {c["metric"] for c in r["constraints"]}
    keys |= {g["metric"] for g in r["regimes"]}
    keys |= set(r.get("held_out", []))
    keys |= {s["metric"] for s in r.get("self_check", [])}
    keys |= set(r.get("constant_fields", []))
    keys |= {i["observable"] for i in o["design_space"]["invariants"]
             if "." not in i["observable"]}
    return keys


def manipulation_prefixes():
    d = yaml.safe_load(CAMPAIGN.read_text())
    return {f["manipulation"]["observable"].split(".")[0]
            for f in d["optimization"]["factors"]}


def manipulation_leaves():
    """The exact cfg_resolved.<leaf> names every factor's predicate compares.

    A prefix check only proves the DICT exists. An earlier epoch aborted because a
    pointer named nothing in the template; the leaf check is what would have caught it.
    """
    d = yaml.safe_load(CAMPAIGN.read_text())
    out = {}
    for f in d["optimization"]["factors"]:
        obs = f["manipulation"]["observable"]
        pre, _, leaf = obs.partition(".")
        out.setdefault(pre, set()).add(leaf)
    return out


def campaign_interpreter():
    """The literal interpreter token in optimization.run_command."""
    d = yaml.safe_load(CAMPAIGN.read_text())
    return d["optimization"]["run_command"].split()[0]


# Set in main() to the staged at.yaml every adapter invocation is pointed at.
ATP = None
TEMPLATE = REPO / "campaign" / "apparatus" / "at.yaml.template"
LIVE_AT = REPO / "at.yaml"


def test_template_declares_every_pointer():
    """The at.yaml template must declare every pointer the factors patch.

    `at.yaml` is referenced by run_command and by all five `apply` blocks, but a
    config_patch NEVER CREATES STRUCTURE -- an absent pointer aborts every row of that
    factor's levels. It existed nowhere in the tree and no task created it, which is
    the defect this check closes.
    """
    assert TEMPLATE.exists(), (
        f"{TEMPLATE} is absent. The campaign patches five pointers into at.yaml and a "
        f"config_patch never creates structure, so every row would abort at row 1.")
    text = TEMPLATE.read_text()
    for leaf in sorted(set().union(*manipulation_leaves().values())):
        assert re.search(rf"^\s*{re.escape(leaf)}\s*:", text, re.M), (
            f"the at.yaml template declares no `{leaf}:` key, so "
            f"config_patch /anytime/{leaf} addresses nothing and aborts every row of "
            f"that factor's levels")
    assert re.search(r"^\s*threshold\s*:", text, re.M), (
        "the template declares no `threshold:` line, so the adapter cannot calibrate "
        "the per-row FPR knob by rewriting it")


def staged_at_yaml(tmp):
    """Stage the template as a row's patched at.yaml would appear.

    Deliberately NOT copied to REPO/at.yaml: the probe must not create the live file as
    a side effect, or the launch gate's own check that it was staged becomes vacuous.
    """
    dst = pathlib.Path(tmp) / "at.yaml"
    dst.write_text(TEMPLATE.read_text())
    return str(dst)


def run_adapter(interpreter, extra, at_path):
    cmd = [interpreter, str(ADAPTER),
           "--anytime-config", at_path, "--target-fpr", "0.05",
           "--adapter-width", "1", "--quick"] + extra
    return subprocess.run(cmd, capture_output=True, text=True, timeout=3600,
                          cwd=str(REPO))


def test_campaign_interpreter_can_run_the_adapter():
    """The interpreter the campaign will actually exec must be able to run the adapter.

    This is not pedantry: `run_command` says `python3`, and `python3` here has no yaml.
    An adapter that imports yaml at module scope aborts EVERY row with
    ModuleNotFoundError, and the failure surfaces only after launch.
    """
    interp = campaign_interpreter()
    assert shutil.which(interp), f"run_command names '{interp}', which is not on PATH"
    p = run_adapter(interp, ["--incumbent-only"], ATP)
    assert p.returncode == 0, (
        f"the campaign's own interpreter ('{interp}') cannot run the adapter:\n"
        f"{p.stderr[-4000:]}")
    return json.loads(p.stdout)


def test_anytime_path_refuses_while_the_detector_is_absent():
    """No silent fallback: without sim/saturation/anytime.go the anytime path must ERROR.

    The recurring failure mode in this work is a mechanism that appears to produce
    numbers it cannot possibly have measured. Until the build stage authors the
    detector, asking for anytime numbers must fail loudly.
    """
    if (REPO / "sim" / "saturation" / "anytime.go").exists():
        return  # build stage has landed; the anytime path is live and this check retires
    p = run_adapter("python3.11", [], ATP)
    assert p.returncode != 0, (
        "the adapter produced ANYTIME numbers with no sim/saturation/anytime.go present "
        "-- that is a silent fallback fabricating the campaign's primary response")
    assert "anytime.go" in p.stderr or "not built" in p.stderr.lower(), (
        f"the anytime path failed, but not with a diagnosis naming the missing "
        f"mechanism:\n{p.stderr[-2000:]}")


def main():
    global ATP
    test_template_declares_every_pointer()
    tmp = tempfile.mkdtemp(prefix="at-contract-")
    ATP = staged_at_yaml(tmp)

    got = test_campaign_interpreter_can_run_the_adapter()

    req = required_keys()
    missing = sorted(req - set(got))
    assert not missing, f"adapter omits keys the campaign reads: {missing}"

    for p, leaves in manipulation_leaves().items():
        assert p in got, f"manipulation predicates read '{p}.*' but it is absent"
        assert isinstance(got[p], dict), f"'{p}' must be an object, got {type(got[p])}"
        absent = sorted(leaves - set(got[p]))
        assert not absent, (
            f"manipulation predicates read {['%s.%s' % (p, a) for a in absent]} "
            f"but those leaves are absent -- a pointer naming nothing aborts every row "
            f"of that factor's levels")

    assert got["zero_delay_unclipped_num"] == 0, \
        "a 0us delay with clipped=false is the signature of the metric bug this replaces"

    # Every dispersion index the plan requires, for all five levels.
    for lvl in ["constant", "poisson", "gamma_cv2", "gamma_cv4", "weibull_cv3_heldout"]:
        k = f"dispersion_index_{lvl}"
        assert k in got, f"{k} is absent -- the pre-registered prediction is monotonicity in MEASURED I"
        assert got[k] is not None and got[k] >= 0.0, f"{k} = {got[k]!r} is not a dispersion index"

    test_anytime_path_refuses_while_the_detector_is_absent()

    if not LIVE_AT.exists():
        print("NOTE: %s is not staged. `nous run` executes from the repo root and the "
              "config_patch cannot create it, so the launch gate must run:\n"
              "        cp campaign/apparatus/at.yaml.template at.yaml" % LIVE_AT,
              file=sys.stderr)

    print("PASS —", len(req), "required keys present")


if __name__ == "__main__":
    main()
