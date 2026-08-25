#!/usr/bin/env python3.11
"""Locate each burstiness level's capacity cliff by the §1.1 backlog-divergence test.

Ground truth is NOT a latency threshold: a regime is super-capacity iff mean E2E keeps
GROWING with the horizon instead of stabilizing. Two horizons per rate; the knee is the
last rate whose growth stays under GROWTH_SUB.

Two corrections against the plan's draft, both verified against the binary before use:

1. `--rate` is IGNORED under `--workload-spec` (findings/RATE-FLAG-IGNORED-UNDER-SPEC.md).
   `aggregate_rate` inside the spec is the sole rate authority, so load is varied by
   rewriting `aggregate_rate` in a COPY of the frozen spec. The frozen specs under
   campaign/apparatus/burstiness/ are never mutated in place -- they are hashed into
   burstiness_ladder_hash.

2. The mean-E2E key is `e2e_mean_ms`, not `mean_e2e_ms`, and `--metrics-path /dev/stdout`
   is NOT bare JSON (stdout carries a `=== Simulation Metrics ===` banner). The metrics
   go to a private temp file and are parsed from there. A missing key raises KeyError --
   there is deliberately no sentinel fallback (R1: never a silent continue).
"""
import json, os, pathlib, subprocess, sys, tempfile

import yaml

GROWTH_SUB = 0.25          # <=25% growth n_lo->n_hi reads as sub-capacity
N_LO, N_HI = 800, 3200
SEED = 42
LEVELS = ["constant", "poisson", "gamma_cv2", "gamma_cv4", "weibull_cv3_heldout"]

REPO = pathlib.Path(__file__).resolve().parents[2]
BLIS = REPO / "blis"
SPECS = REPO / "campaign" / "apparatus" / "burstiness"
OUT = REPO / "campaign" / "apparatus" / "cliffs.json"
MODEL = "meta-llama/llama-3.1-8b-instruct"

E2E_KEY = "e2e_mean_ms"    # verified against ./blis --metrics-path output


def spec_at_rate(spec, rate, n, scratch):
    """Write a COPY of the frozen spec with aggregate_rate=rate. Returns its path.

    --rate is IGNORED under --workload-spec (findings/RATE-FLAG-IGNORED-UNDER-SPEC.md):
    aggregate_rate is the sole rate authority, so passing --rate would run every rung at
    the same load and fabricate a cliff. The frozen spec is never mutated in place.
    """
    src = SPECS / f"{spec}.yaml"
    d = yaml.safe_load(src.read_text())
    d["aggregate_rate"] = float(rate)
    d["num_requests"] = int(n)
    out = pathlib.Path(scratch) / f"{spec}-r{rate}-n{n}.yaml"
    out.write_text(yaml.safe_dump(d, sort_keys=False))
    return str(out)


def mean_e2e(spec, rate, n, seed=SEED, scratch=None):
    """Run BLIS once; return mean E2E ms. Raise on failure -- never return a sentinel."""
    with tempfile.TemporaryDirectory(prefix="cliff-") as td:
        scr = scratch or td
        metrics = pathlib.Path(td) / "metrics.json"
        cmd = [str(BLIS), "run", "--model", MODEL,
               "--workload-spec", spec_at_rate(spec, rate, n, scr),
               "--num-requests", str(n), "--seed", str(seed),
               "--metrics-path", str(metrics)]
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=3600,
                           cwd=str(REPO))
        if p.returncode != 0:
            raise RuntimeError(
                f"blis failed for {spec} rate={rate} n={n}: {p.stderr[-2000:]}")
        if not metrics.exists():
            raise RuntimeError(f"blis wrote no metrics file for {spec} rate={rate} n={n}")
        return json.loads(metrics.read_text())[E2E_KEY]


def find_cliff(spec, rates):
    knee = None
    rows = []
    for r in rates:
        lo, hi = mean_e2e(spec, r, N_LO), mean_e2e(spec, r, N_HI)
        growth = (hi - lo) / lo
        sub = growth <= GROWTH_SUB
        print(f"  {spec:24} rate={r:5.1f}  {lo:9.1f} -> {hi:9.1f}  {growth:+.1%}"
              f"  {'sub' if sub else 'SUPER'}", flush=True)
        rows.append({"rate": r, "e2e_lo": lo, "e2e_hi": hi, "growth": growth,
                     "regime": "sub" if sub else "super"})
        if sub:
            knee = r
        else:
            break
    if knee is None:
        raise RuntimeError(f"{spec}: every rate probed is already super-capacity -- "
                           f"lower the sweep, do not guess a cliff")
    if rows[-1]["regime"] == "sub":
        raise RuntimeError(
            f"{spec}: the sweep NEVER left sub-capacity (top rate {rows[-1]['rate']} still "
            f"grows only {rows[-1]['growth']:+.1%}) -- so `knee` here is just the last rate "
            f"probed, NOT a measured cliff. RAISE the sweep, do not report this. Without "
            f"this guard the loop returns the sweep's top rate for every level and nothing "
            f"raises -- the same open-top fabrication shape as "
            f"findings/RATE-FLAG-IGNORED-UNDER-SPEC.md.")
    return knee, rows


def main():
    # Sweep range set from MEASURED evidence, not from LADDER.md's r_nominal=20.
    # LADDER.md's 20 rps knee belongs to a DIFFERENT workload than these frozen specs
    # (see BURSTINESS.md "Why the cliff is not 20"): under the frozen burstiness specs
    # Poisson serves 36 rps with no backlog, and its knee sits at 88 rps. The plan's
    # [8..26] range is entirely sub-capacity for every level, which the open-top guard
    # in find_cliff() now refuses to report.
    rates = [60, 70, 76, 80, 84, 88, 92, 96, 104, 112, 128]
    cliffs, tables = {}, {}
    for lvl in LEVELS:
        print(f"=== {lvl} ===", flush=True)
        cliffs[lvl], tables[lvl] = find_cliff(lvl, rates)
    OUT.write_text(json.dumps(cliffs, indent=2, sort_keys=True) + "\n")
    (OUT.parent / "cliff-tables.json").write_text(
        json.dumps(tables, indent=2, sort_keys=True) + "\n")
    print("\ncliffs:", cliffs, flush=True)
    assert cliffs["gamma_cv4"] < cliffs["poisson"], \
        "burstier traffic must saturate STRICTLY earlier than Poisson. NOTE the strict " \
        "inequality: `<=` would be satisfied by EQUAL cliffs, which is exactly what an " \
        "inert rate knob produces -- a guard that cannot tell 'measured and equal' from " \
        "'never varied' is not a guard (RATE-FLAG-IGNORED-UNDER-SPEC.md)"
    print("PASS")


if __name__ == "__main__":
    main()
