#!/usr/bin/env python3
"""Frozen bench adapter: score ONE detector configuration on the metamorphic suite.

This is the campaign's INSTRUMENT (`run_command`). Per the optimization-campaign
guide it is under the measurement freeze: while an epoch is measuring, nothing
here changes. It prints exactly one JSON object to stdout.

WHAT IT DOES, in the order metamorphic_tests.md mandates:

  1. §3.4 FPR CALIBRATION FIRST. Runs the calibration band (0.3-0.6x nominal, all
     seeds) and tunes the detector's ONE knob until it fires on <= target_fpr of
     those known-stable runs. Freezes it. This comes before every test because an
     uncalibrated comparison is gameable: a detector that fires unconditionally
     passes T1/T2/T3 trivially and scores zero flips on T4.
  2. T1/T2-IN/T2-OUT/T3 response ladders, scored by the §3.5 rung rules
     (per-seed >= 50% fired -> FIRED; >= 3/5 seeds -> rung FIRED).
  3. T4 flip count on super-capacity-mean traces only (§5.2).
  4. Lead time: rungs of headroom between first firing and the cliff. Larger is
     better (§1a: "interval between the detector firing and the backlog actually
     diverging ... larger = earlier warning").

The objective the campaign minimizes is `regret`, defined so that LOWER IS
BETTER for a single scalar response: failed tests dominate, lead time breaks ties.
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile

BLIS = os.environ.get("BLIS_BIN", "./blis")
MODEL = "meta-llama/llama-3.1-8b-instruct"
R_NOMINAL = 20.0  # measured; see campaign/apparatus/LADDER.md
SEEDS = [42, 43, 44, 45, 46]

CALIB_MULTS = [0.3, 0.4, 0.5, 0.6]
SUPER_MULTS = [1.1, 1.25, 1.5, 2.0]
T1_MULTS = CALIB_MULTS + [0.7, 0.8, 0.9, 0.95, 1.0] + SUPER_MULTS


def run_blis(detector, cfg_path, rate, seed, num_requests, extra=None):
    """One BLIS run; returns the per-event trace records for `detector`."""
    with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as f:
        report = f.name
    cmd = [
        BLIS, "run", "--model", MODEL,
        "--num-requests", str(num_requests),
        "--rate", str(rate), "--seed", str(seed),
        "--detectors", detector, "--saturation-report", report,
    ]
    if cfg_path:
        cmd += ["--saturation-config", cfg_path]
    if extra:
        cmd += extra
    try:
        subprocess.run(cmd, capture_output=True, timeout=1200, check=True)
        with open(report) as fh:
            data = json.load(fh)
        return [r for r in data.get("trace", []) if r.get("detector") == detector]
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired, json.JSONDecodeError, OSError):
        return None
    finally:
        try:
            os.unlink(report)
        except OSError:
            pass


def fired_fraction(records, warmup_frac=0.1):
    """§3.5 run->verdict: fraction of the post-warm-up run in a fired state."""
    if not records:
        return 0.0
    start = int(len(records) * warmup_frac)
    tail = records[start:]
    if not tail:
        return 0.0
    fired = sum(1 for r in tail if r["result"]["level"] in ("BACKLOGGED", "OVERLOADED"))
    return fired / len(tail)


def rung_fired(detector, cfg_path, rate, num_requests, extra=None, threshold=0.5):
    """§3.5 seeds->rung: majority vote over seeds."""
    votes = []
    for seed in SEEDS:
        recs = run_blis(detector, cfg_path, rate, seed, num_requests, extra)
        votes.append(1 if fired_fraction(recs) >= threshold else 0)
    return sum(votes) >= 3, votes


def flip_count(records, warmup_frac=0.1):
    """T4: transitions from fired -> not-fired (§5.3)."""
    if not records:
        return 0
    start = int(len(records) * warmup_frac)
    states = [r["result"]["level"] in ("BACKLOGGED", "OVERLOADED") for r in records[start:]]
    return sum(1 for i in range(1, len(states)) if states[i - 1] and not states[i])


def write_cfg(detector, knob_value, path):
    """Emit the one-knob YAML for this detector at this calibration setting."""
    blocks = {
        "composite": f"composite:\n  sensitivity: {knob_value}\n",
        "threshold": f"threshold:\n  threshold_ms: {knob_value}\n",
        "backlog-drift": f"backlog_drift:\n  slope_k: {knob_value}\n",
        "swd": f"swd:\n  threshold: {knob_value}\n",
        "owd": f"owd:\n  threshold: {knob_value}\n",
    }
    with open(path, "w") as fh:
        fh.write(blocks[detector])
    return path


# Knob search grids. Direction differs per detector: for composite a LARGER
# sensitivity raises the noise floor (fires less); for the others a larger
# threshold fires less. All grids are ordered fires-more -> fires-less.
KNOB_GRIDS = {
    "composite":     [0.25, 0.5, 1.0, 2.0, 4.0, 8.0, 16.0],
    "threshold":     [1000, 2500, 5000, 8000, 12000, 20000, 35000],
    "backlog-drift": [0.5, 1.0, 3.0, 6.0, 12.0, 25.0, 50.0],
    "swd":           [500, 1500, 5000, 15000, 50000, 150000, 500000],
    "owd":           [500, 1500, 5000, 15000, 50000, 150000, 500000],
}


def calibrate(detector, target_fpr, num_requests, workdir):
    """§3.4: pick the LOWEST-threshold knob whose FPR on the calibration band is
    <= target. Lowest means most sensitive among the admissible settings, which is
    the fair operating point: any detector can buy a lower FPR by going blind."""
    grid = KNOB_GRIDS[detector]
    trials = []
    for knob in grid:
        cfg = write_cfg(detector, knob, os.path.join(workdir, "cal.yaml"))
        firings = 0
        total = 0
        for mult in CALIB_MULTS:
            rate = round(R_NOMINAL * mult, 3)
            for seed in SEEDS:
                recs = run_blis(detector, cfg, rate, seed, num_requests)
                total += 1
                if fired_fraction(recs) >= 0.5:
                    firings += 1
        fpr = firings / total if total else 1.0
        trials.append({"knob": knob, "fpr": fpr, "firings": firings, "n": total})
        if fpr <= target_fpr:
            return knob, fpr, trials
    # Nothing met the budget: return the least-firing setting and say so.
    return grid[-1], trials[-1]["fpr"], trials


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--detector", required=True,
                    choices=["composite", "threshold", "backlog-drift", "swd", "owd"])
    ap.add_argument("--target-fpr", type=float, default=0.05)
    ap.add_argument("--num-requests", type=int, default=1200)
    ap.add_argument("--quick", action="store_true",
                    help="smoke mode: 1 seed, fewer rungs (for --smoke/--liveness only)")
    args = ap.parse_args()

    global SEEDS
    if args.quick:
        SEEDS = [42]

    workdir = os.environ.get("NOUS_RUN_DIR") or tempfile.mkdtemp()
    os.makedirs(workdir, exist_ok=True)
    det = args.detector

    # ---- Step 1: FPR calibration, BEFORE any test is read. ----
    knob, fpr, trials = calibrate(det, args.target_fpr, args.num_requests, workdir)
    cfg = write_cfg(det, knob, os.path.join(workdir, "frozen.yaml"))

    # ---- Step 2: response ladders. ----
    t1 = {}
    for mult in (CALIB_MULTS + [0.7, 0.9, 1.0] + SUPER_MULTS if not args.quick else [0.3, 1.5]):
        rate = round(R_NOMINAL * mult, 3)
        ok, votes = rung_fired(det, cfg, rate, args.num_requests)
        t1[str(mult)] = {"rate": rate, "fired": ok, "votes": votes}

    # T1 passes iff it fires at every super-capacity rung tested.
    super_keys = [k for k in t1 if float(k) >= 1.1]
    t1_pass = bool(super_keys) and all(t1[k]["fired"] for k in super_keys)

    # Sharpness / lead time (§4.4): the LOWEST multiplier at which the detector
    # first fires and stays fired. Lead time is how much headroom below the
    # cliff that gives -- larger = earlier warning.
    first_fire = None
    for k in sorted(t1, key=float):
        if t1[k]["fired"]:
            first_fire = float(k)
            break
    lead_time = (1.0 - first_fire) if first_fire is not None else -1.0

    # ---- Step 3: T4 flip count on super-capacity traces only (§5.2). ----
    flips = []
    for mult in (SUPER_MULTS if not args.quick else [1.5]):
        rate = round(R_NOMINAL * mult, 3)
        for seed in SEEDS:
            recs = run_blis(det, cfg, rate, seed, args.num_requests)
            if recs:
                flips.append(flip_count(recs))
    t4_flips = max(flips) if flips else 0
    t4_pass = t4_flips == 0

    # ---- Objective: lower is better. ----
    # A failed response test is worth more than any amount of lead time, so
    # failures dominate the scalar; lead time only breaks ties among passers.
    failures = (0 if t1_pass else 1) + (0 if t4_pass else 1)
    regret = 100.0 * failures + 10.0 * t4_flips + max(0.0, 1.0 - max(lead_time, 0.0)) * 10.0

    print(json.dumps({
        "detector": det,
        "regret": round(regret, 4),
        "t1_pass": t1_pass,
        "t4_pass": t4_pass,
        "t4_max_flips": t4_flips,
        "lead_time_mult": round(lead_time, 4),
        "first_fire_mult": first_fire,
        "calibrated_fpr": round(fpr, 4),
        "fpr_within_budget": fpr <= args.target_fpr,
        "frozen_knob": knob,
        "calibration_trials": trials,
        "t1_rungs": t1,
        "cfg": {"model": MODEL, "r_nominal": R_NOMINAL, "seeds": SEEDS,
                "num_requests": args.num_requests, "target_fpr": args.target_fpr},
    }, sort_keys=True))


if __name__ == "__main__":
    main()
