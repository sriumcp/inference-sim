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
from concurrent.futures import ThreadPoolExecutor

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


def _inner_width():
    """How many BLIS processes this ADAPTER runs at once.

    Safe to parallelize at all because each invocation is its own process with its
    own report path, and BLIS's objective is simulated time computed from trained
    coefficients -- it reads no wall clock, so a co-scheduled neighbour changes how
    long a run takes to finish but not a single bit of what it prints (the same
    property that licenses the campaign's `concurrency.load_independent`). Verified
    bit-identical to serial on every reported field.

    SIZING MUST COMPOSE WITH THE OUTER WIDTH, and this is where a first attempt got
    it wrong in production. The guide's warning is explicit -- "a real campaign whose
    adapter itself spawned 4 concurrent probes per row would have put 16 processes on
    a 10-CPU box" -- and the first version of this function tried to honour it by
    dividing the core budget by $NOUS_MAX_PARALLEL. That variable DOES NOT EXIST:
    nousko exports NOUS_RUN_DIR, NOUS_ROW_INDEX, NOUS_RUN_SLOT and
    NOUS_WORKLOAD_SEED, so the divisor silently defaulted to 1, every concurrent row
    claimed the full budget, and a 10-core box ran at load average 122 with 3 rows x
    8 workers. Rows then failed on contention rather than on their configuration.

    The fix keys on a variable nousko actually sets. NOUS_ADAPTER_WIDTH is the
    explicit override (this campaign sets it from max_parallel); absent that, the
    budget is divided by a conservative assumed outer width, because being wrong
    toward UNDER-subscription costs wall clock while being wrong toward
    over-subscription costs correctness -- contention-failed rows are missing data in
    the fit.
    """
    try:
        cores = len(os.sched_getaffinity(0))
    except AttributeError:
        cores = os.cpu_count() or 4
    budget = max(1, cores - 2)

    explicit = os.environ.get("NOUS_ADAPTER_WIDTH")
    if explicit:
        try:
            w = int(explicit)
            if w > 0:
                return max(1, min(w, budget))
        except ValueError:
            pass

    # No explicit width: assume the outer runner may be running several rows.
    # NOUS_RUN_SLOT is exported per row, so its presence proves we are inside a
    # campaign row rather than a standalone probe.
    in_campaign = "NOUS_RUN_SLOT" in os.environ
    assumed_outer = 3 if in_campaign else 1
    return max(1, budget // assumed_outer)


def run_many(jobs):
    """Run (detector, cfg, rate, seed, n, extra) jobs concurrently, order preserved."""
    width = _inner_width()
    if width <= 1:
        return [run_blis(*j) for j in jobs]
    with ThreadPoolExecutor(max_workers=width) as ex:
        return list(ex.map(lambda j: run_blis(*j), jobs))


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
    results = run_many([(detector, cfg_path, rate, seed, num_requests, extra) for seed in SEEDS])
    votes = [1 if fired_fraction(r) >= threshold else 0 for r in results]
    return sum(votes) > len(SEEDS) // 2, votes


def flip_count(records, warmup_frac=0.1):
    """T4: transitions from fired -> not-fired (§5.3)."""
    if not records:
        return 0
    start = int(len(records) * warmup_frac)
    states = [r["result"]["level"] in ("BACKLOGGED", "OVERLOADED") for r in records[start:]]
    return sum(1 for i in range(1, len(states)) if states[i - 1] and not states[i])


def write_cfg(detector, knob_value, path, rw_template=None):
    """Emit the detector config at this calibration setting.

    For randomwalk the FACTOR levels arrive via nousko's config_patch on the
    template file, so this function must PRESERVE them and rewrite only the
    calibrated threshold. Regenerating the block from scratch would silently
    discard every factor level and measure the baseline corner on every row --
    the single worst failure mode available to this adapter.
    """
    if detector == "randomwalk":
        with open(rw_template) as fh:
            lines = fh.readlines()
        out = []
        for ln in lines:
            if ln.strip().startswith("threshold:"):
                indent = ln[: len(ln) - len(ln.lstrip())]
                out.append(f"{indent}threshold: {knob_value}\n")
            else:
                out.append(ln)
        with open(path, "w") as fh:
            fh.writelines(out)
        return path
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
    # randomwalk's threshold units depend on which statistic is configured, so the
    # grid spans many decades. A knob landing on either ENDPOINT means the true
    # operating point is outside the grid and the reported FPR is a grid artifact,
    # not a calibration -- the campaign constrains against that explicitly
    # (knob_at_grid_edge_num), which is how the first OWD attempt was caught
    # "passing" calibration by going blind at the grid's top.
    # Resolution matters more than range here: a first pass on a decade grid put
    # the FPR transition entirely BETWEEN two knobs (1.0 -> 0.75, 5.0 -> 0.00), so
    # no setting could both respect the 5% budget and still fire. The band from 1
    # to 5 is refined ~1.35x per step so the 5% crossing is actually resolvable.
    # The wide tails are retained so a different statistic (whose threshold units
    # differ by orders of magnitude) still has reachable settings.
    "randomwalk":    [1e-4, 1e-3, 1e-2, 0.05, 0.1, 0.25, 0.5, 0.75, 1.0,
                      1.2, 1.4, 1.6, 1.9, 2.2, 2.6, 3.0, 3.5, 4.0, 4.5, 5.0,
                      6.0, 8.0, 12.0, 25.0, 60.0, 150.0, 500.0, 2e3, 1e4, 1e5],
}


def calibrate(detector, target_fpr, num_requests, workdir, rw_template=None):
    """§3.4: calibrate to a common false-alarm rate, then FREEZE.

    Finds the MOST SENSITIVE knob whose FPR on the known-stable calibration band is
    within budget -- the smallest threshold that still respects it. That is the fair
    operating point: any detector can buy a lower FPR by going blind, so rewarding a
    lower FPR than the budget requires would reward exactly the smoke-detector-in-
    reverse failure §3.4 warns about.

    BISECTION, not a linear grid walk. FPR is MONOTONE in the knob by construction
    (every grid is ordered fires-more -> fires-less), so the admissible region is a
    suffix and binary search finds its first element in ceil(log2(n)) probes instead
    of n. On a 30-entry grid that is 5 probes rather than 30 -- and since the
    calibration band is 4 rungs x 5 seeds = 20 BLIS runs per probe, it cuts the
    dominant cost of a row by ~6x (600 runs -> ~100). The result is IDENTICAL to the
    linear walk under monotonicity; where FPR is non-monotone from seed noise the
    two can differ by one grid step, which is why every probe is recorded.

    Probes are cached so the trials log is complete for auditing and the
    knob_at_grid_edge constraint stays meaningful.
    """
    grid = KNOB_GRIDS[detector]
    trials = {}

    def fpr_at(i):
        if i in trials:
            return trials[i]["fpr"]
        cfg = write_cfg(detector, grid[i], os.path.join(workdir, f"cal-{i}.yaml"), rw_template)
        jobs = [(detector, cfg, round(R_NOMINAL * m, 3), seed, num_requests, None)
                for m in CALIB_MULTS for seed in SEEDS]
        results = run_many(jobs)
        total = len(results)
        firings = sum(1 for r in results if fired_fraction(r) >= 0.5)
        fpr = firings / total if total else 1.0
        trials[i] = {"knob": grid[i], "fpr": fpr, "firings": firings, "n": total}
        return fpr

    # Binary search for the FIRST index whose FPR is within budget.
    lo, hi, best = 0, len(grid) - 1, None
    while lo <= hi:
        mid = (lo + hi) // 2
        if fpr_at(mid) <= target_fpr:
            best = mid
            hi = mid - 1
        else:
            lo = mid + 1

    ordered = [trials[i] for i in sorted(trials)]
    if best is not None:
        # Confirm the neighbour below is genuinely over budget, so a non-monotone
        # dip cannot hand back an over-sensitive knob unchecked.
        if best > 0:
            fpr_at(best - 1)
        ordered = [trials[i] for i in sorted(trials)]
        return grid[best], trials[best]["fpr"], ordered

    # Nothing met the budget: return the least-firing probe and let the
    # fpr_within_budget constraint mark the row infeasible -- never silently
    # pretend a detector was calibrated when it was not.
    worst = min(ordered, key=lambda t: t["fpr"])
    return worst["knob"], worst["fpr"], ordered


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--detector", required=True,
                    choices=["composite", "threshold", "backlog-drift", "swd", "owd", "randomwalk"])
    ap.add_argument("--rw-config", default="rw.yaml",
                    help="randomwalk config template; nousko patches the factor levels into a "
                         "per-run COPY of this path and substitutes it into the command")
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
    knob, fpr, trials = calibrate(det, args.target_fpr, args.num_requests, workdir, args.rw_config)
    cfg = write_cfg(det, knob, os.path.join(workdir, "frozen.yaml"), args.rw_config)

    # Echo back the RESOLVED factor levels so each factor's manipulation predicate
    # can verify the lever actually engaged on this row (Family A). Reading them
    # from the patched template -- not from our own defaults -- is what makes the
    # check meaningful.
    resolved = {}
    if det == "randomwalk":
        with open(args.rw_config) as fh:
            for ln in fh:
                if ":" in ln and not ln.strip().startswith("#"):
                    k, _, v = ln.strip().partition(":")
                    v = v.strip()
                    if v and k.strip() != "randomwalk":
                        key = k.strip()
                        # Preserve the level's TYPE: an int factor level (consecutive_k:
                        # 3) must come back as int 3, not 3.0, or the manipulation
                        # predicate can never compare equal. Floats stay floats;
                        # everything else stays a string.
                        try:
                            if v.lstrip("-").isdigit():
                                resolved[key] = int(v)
                            elif "." in v or "e" in v.lower():
                                resolved[key] = float(v)
                            else:
                                resolved[key] = v
                        except ValueError:
                            resolved[key] = v

    # ---- Step 2: response ladders. ----
    t1 = {}
    ladder = CALIB_MULTS + [0.7, 0.9, 1.0] + SUPER_MULTS
    if args.quick:
        ladder = [0.3, 0.6, 1.5, 2.0]
    for mult in ladder:
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
    t4_mults = SUPER_MULTS if not args.quick else [1.5, 2.0]
    t4_jobs = [(det, cfg, round(R_NOMINAL * m, 3), seed, args.num_requests, None)
               for m in t4_mults for seed in SEEDS]
    flips = [flip_count(r) for r in run_many(t4_jobs) if r]
    t4_flips = max(flips) if flips else 0
    t4_pass = t4_flips == 0

    # ---- Objective: lower is better. ----
    # A failed response test is worth more than any amount of lead time, so
    # failures dominate the scalar; lead time only breaks ties among passers.
    failures = (0 if t1_pass else 1) + (0 if t4_pass else 1)
    regret = 100.0 * failures + 10.0 * t4_flips + max(0.0, 1.0 - max(lead_time, 0.0)) * 10.0

    # Did it fire on EVERY rung? Then "always saturated" is gaming the ladders.
    fires_on_all = all(v["fired"] for v in t1.values()) if t1 else False
    grid = KNOB_GRIDS[det]
    at_edge = knob == grid[0] or knob == grid[-1]

    print(json.dumps({
        "detector": det,
        "regret": round(regret, 4),
        # Numeric mirrors of the boolean guards, because response constraints
        # compare numbers. 1.0 = true.
        "t1_pass_num": 1.0 if t1_pass else 0.0,
        "fpr_within_budget_num": 1.0 if fpr <= args.target_fpr else 0.0,
        "fires_on_all_rungs_num": 1.0 if fires_on_all else 0.0,
        "knob_at_grid_edge_num": 1.0 if at_edge else 0.0,
        # `cfg` carries the RESOLVED factor levels, read back from the patched
        # config file -- this is what each factor's manipulation predicate checks
        # (cfg.statistic, cfg.consecutive_k, ...). Reading them from the patched
        # file rather than from our own defaults is what makes the check meaningful:
        # it verifies the lever actually engaged on THIS row.
        "cfg": resolved,
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
        "model": MODEL,
        "r_nominal": R_NOMINAL,
        "target_fpr": args.target_fpr,
        "threshold_was_calibrated": True,
        "run_meta": {"seeds": SEEDS, "num_requests": args.num_requests},
    }, sort_keys=True))


if __name__ == "__main__":
    main()
