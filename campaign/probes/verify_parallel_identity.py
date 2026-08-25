#!/usr/bin/env python3.11
"""--adapter-width must not change a single MEASURED bit.

This is the concrete evidence for the campaign's `concurrency.load_independent: true`
claim. It holds because BLIS reads no wall clock -- simulated time is accumulated from
trained latency coefficients -- so a co-scheduled neighbour changes how LONG a run takes
to finish but not what it prints. That is a property to VERIFY, not to assert: the
campaign runs 3 rows x 2 adapter workers, and if width perturbed any measurement, every
row would carry contention noise indistinguishable from a configuration effect.

CORRECTION AGAINST THE PLAN'S DRAFT OF THIS CHECK. The plan runs
`--adapter-width 1` and `--adapter-width 2` and diffs the two JSON objects whole,
expecting an EMPTY diff. That check cannot pass, and not because of a defect: the same
plan requires the adapter to RECORD its resolved width in the output (it is the
provenance for the oversubscription failure of
findings/PARALLELISM-OVERSUBSCRIPTION.md), so run_meta.adapter_width differs by
construction -- 1 vs 2. A whole-object diff therefore fails on the one field that is
SUPPOSED to differ, and the natural "fix" -- dropping the field so the diff passes --
would delete the provenance and leave a real width regression invisible.

So the identity is asserted over every key EXCEPT the recorded width, and the width
field is separately asserted to have actually CHANGED. That second half matters: if the
adapter ignored --adapter-width, both runs would be serial, the diff would be trivially
empty, and the check would "pass" while proving nothing -- the same inert-knob shape as
findings/RATE-FLAG-IGNORED-UNDER-SPEC.md.
"""
import copy
import json
import pathlib
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parents[2]
ADAPTER = REPO / "campaign" / "bench" / "score_anytime.py"
TEMPLATE = REPO / "campaign" / "apparatus" / "at.yaml.template"

# The ONLY key licensed to differ: it records the knob under test.
PROVENANCE = ("run_meta", "adapter_width")


def run(width, at_path):
    p = subprocess.run(
        ["python3", str(ADAPTER), "--anytime-config", at_path,
         "--target-fpr", "0.05", "--adapter-width", str(width),
         "--quick", "--incumbent-only"],
        capture_output=True, text=True, timeout=3600, cwd=str(REPO))
    assert p.returncode == 0, f"width={width} failed:\n{p.stderr[-3000:]}"
    return json.loads(p.stdout)


def main():
    tmp = tempfile.mkdtemp(prefix="par-id-")
    at = str(pathlib.Path(tmp) / "at.yaml")
    pathlib.Path(at).write_text(TEMPLATE.read_text())

    ser, par = run(1, at), run(2, at)

    a, b = copy.deepcopy(ser), copy.deepcopy(par)
    outer, leaf = PROVENANCE
    got_ser, got_par = a[outer].pop(leaf), b[outer].pop(leaf)

    assert got_ser != got_par, (
        f"--adapter-width did not change the adapter's resolved width "
        f"({got_ser} both times) -- the knob is INERT, so a bit-identical diff proves "
        f"nothing about parallel-vs-serial")

    sa = json.dumps(a, sort_keys=True)
    sb = json.dumps(b, sort_keys=True)
    if sa != sb:
        for k in sorted(set(a) | set(b)):
            if a.get(k) != b.get(k):
                print(f"DIFFERS: {k}\n  width1={a.get(k)!r}\n  width2={b.get(k)!r}",
                      file=sys.stderr)
        raise AssertionError(
            "parallel is NOT bit-identical to serial on a measured field -- "
            "concurrency.load_independent is false and the campaign's rows would carry "
            "contention noise indistinguishable from a configuration effect")

    print(f"BIT-IDENTICAL — every measured field matches at width {got_ser} and "
          f"{got_par}; only run_meta.adapter_width differs, as it records the knob")


if __name__ == "__main__":
    main()
