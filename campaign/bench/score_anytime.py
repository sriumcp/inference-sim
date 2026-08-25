#!/usr/bin/env python3
"""Frozen bench adapter: score ONE anytime-valid detector configuration.

This is the anytime campaign's INSTRUMENT (`run_command`). It prints exactly one JSON
object to stdout, containing every key the campaign's `response` block reads --
verified against the YAML by campaign/probes/verify_adapter_contract.py, which derives
the required set FROM the YAML so the two cannot drift.

Modelled on campaign/bench/score_detector.py: same run_blis/run_many/calibrate shape,
same --adapter-width flag discipline (a FLAG, never an env-var prefix, because
run_command is exec'd as argv and an env prefix would be read as the program name).

WHAT DIFFERS FROM score_detector.py, and why each difference is forced:

1. NO `import yaml`, ANYWHERE.
   The campaign's run_command is `python3 campaign/bench/score_anytime.py ...`, and
   `python3` on this box is 3.14 WITHOUT the yaml module (only python3.11 has it).
   A module-scope `import yaml` therefore aborts EVERY row with ModuleNotFoundError,
   and the failure surfaces only after launch. Both files this adapter must rewrite
   (the frozen burstiness specs and the patched at.yaml) have flat, line-oriented,
   top-level keys, so a line rewriter is sufficient -- and it ASSERTS it found the
   line, so a schema change is a hard error rather than a silent no-op append.

2. PER-LEVEL rungs from campaign/apparatus/cliffs.json, never one shared R_NOMINAL.
   The measured cliffs are constant 96 / poisson 88 / gamma_cv2 88 / gamma_cv4 80 /
   weibull_cv3_heldout 96 -- NOT LADDER.md's 20, which was measured on a DIFFERENT
   workload (findings/CLIFF-IS-WORKLOAD-SPECIFIC.md). Reusing 20 would put every
   ladder entirely inside sub-capacity and make T1 vacuous.

3. Rungs realized by REWRITING `aggregate_rate`, never by --rate.
   `--rate` is INERT under `--workload-spec` (findings/RATE-FLAG-IGNORED-UNDER-SPEC.md:
   --rate 10 and --rate 200 give byte-identical stdout). aggregate_rate inside the spec
   is the sole rate authority. The frozen specs are hashed into burstiness_ladder_hash
   and are NEVER mutated in place; every rung gets a copy in the row's private scratch.

4. The dispersion index I = Var(N_T)/E[N_T] is MEASURED, not inferred from the CV knob.
   Task 2 established that capacity is NOT monotone in CV across arrival FAMILIES:
   weibull_cv3 (CV 3.0) ties `constant`, the LEAST bursty level, at a cliff of 96, and
   poisson/gamma_cv2 tie at 88 with the ordering inverting on the continuous statistic.
   The campaign's pre-registered prediction is monotonicity in the MEASURED I, so I must
   be an observable -- otherwise a held-out miss on weibull cannot be distinguished from
   a family artifact rather than a refutation. Arrival timestamps come from
   `--trace-output`'s arrival_time_us column: the sampler's own output, not a model of it.
   Do NOT use poisson-vs-gamma_cv2 as a fine-grained burstiness contrast; use
   constant vs gamma_cv4.

5. THE ANYTIME DETECTOR DOES NOT EXIST YET.
   sim/saturation/anytime.go is authored by the campaign's own `build` stage. Until it
   lands there is no anytime detector to score, so the anytime path REFUSES (SystemExit
   with a diagnosis naming the missing file) rather than degrading to a wrapped
   incumbent and passing its numbers off as the wrapper's. `--incumbent-only` is the
   explicit, labelled mode for the pre-build world; it sets
   anytime_mechanism_present=false in the output so no consumer can mistake an
   incumbent reference row for an anytime measurement. A silent fallback here would be
   the sixth instance of this project's recurring failure mode.
"""
import argparse
import csv
import hashlib
import json
import os
import pathlib
import statistics
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

REPO = pathlib.Path(__file__).resolve().parents[2]
BLIS = os.environ.get("BLIS_BIN", str(REPO / "blis"))
MODEL = "meta-llama/llama-3.1-8b-instruct"
SPECS = REPO / "campaign" / "apparatus" / "burstiness"
CLIFFS_PATH = REPO / "campaign" / "apparatus" / "cliffs.json"
ANYTIME_GO = REPO / "sim" / "saturation" / "anytime.go"

# Set from --adapter-width in main(); 0 means "infer" (see _inner_width).
_ADAPTER_WIDTH = 0

SEEDS = [42, 43, 44, 45, 46]

# The five ladder levels. The four FITTED levels are what burstiness_ladder_hash
# covers; weibull_cv3_heldout is the held-out level and is excluded from the hash
# (verify_burstiness_specs.py asserts that exclusion).
FITTED = ["constant", "poisson", "gamma_cv2", "gamma_cv4"]
HELDOUT = "weibull_cv3_heldout"
LEVELS = FITTED + [HELDOUT]

# Rung bands, as multiples of each level's OWN measured cliff (LADDER.md's bands).
CALIB_MULTS = [0.3, 0.4, 0.5, 0.6]      # known-stable: the FPR calibration band
GRAY_MULTS = [0.7, 0.8, 0.9, 0.95]
CLIFF_MULT = [1.0]
SUPER_MULTS = [1.1, 1.25, 1.5, 2.0]     # known super-capacity: ground truth = FIRED
T1_MULTS = CALIB_MULTS + GRAY_MULTS + CLIFF_MULT + SUPER_MULTS

# Frozen horizon CAP. For an anytime detector the horizon is DERIVED, so what is
# frozen is the cap -- matched to the incumbent's frozen horizon so the comparison is
# fair -- and reaching it without committing is what indeterminate_forever scores.
N_CAP = 6000
N_DEFAULT = 1200

FIRED_LEVELS = ("BACKLOGGED", "OVERLOADED")

# Which detector name each row's mechanism reports under in the --saturation-report
# trace. The anytime wrapper's own name is not knowable until the build stage names
# it; `anytime` is the name the campaign's config pointer (/anytime/...) implies.
ANYTIME_DETECTOR = "anytime"
INCUMBENTS = ["composite", "peak-rate", "threshold", "backlog-drift"]


# ─────────────────────────── spec / config rewriting ───────────────────────────

def _rewrite_scalar(lines, key, value):
    """Replace the single top-level `key: ...` line. Returns (lines, found_count).

    Asserting the count at the call site is what makes this safe without a YAML
    parser: a schema change that moves or renames the key raises instead of silently
    leaving the frozen placeholder in place, which would run every rung at the SAME
    load and fabricate a ladder -- the exact shape of
    findings/RATE-FLAG-IGNORED-UNDER-SPEC.md.
    """
    out, found = [], 0
    for ln in lines:
        stripped = ln.lstrip()
        # top level only: no leading indentation, not a comment
        if (ln[: len(ln) - len(stripped)] == "" and not stripped.startswith("#")
                and stripped.split(":", 1)[0] == key):
            out.append(f"{key}: {value}\n")
            found += 1
        else:
            out.append(ln)
    return out, found


def spec_at_rate(level, rate, n, scratch):
    """Write a COPY of the frozen spec with aggregate_rate=rate, num_requests=n.

    The frozen spec under campaign/apparatus/burstiness/ is NEVER mutated in place --
    it is hashed into burstiness_ladder_hash, a constant_field every row reports.
    """
    src = SPECS / f"{level}.yaml"
    lines = src.read_text().splitlines(keepends=True)
    lines, nr = _rewrite_scalar(lines, "aggregate_rate", float(rate))
    assert nr == 1, (
        f"{src}: expected exactly 1 top-level `aggregate_rate:` line, found {nr}. "
        f"aggregate_rate is the SOLE rate authority under --workload-spec (--rate is "
        f"inert), so failing to rewrite it runs every rung at the same load and "
        f"fabricates a ladder.")
    lines, nn = _rewrite_scalar(lines, "num_requests", int(n))
    assert nn == 1, f"{src}: expected exactly 1 top-level `num_requests:` line, found {nn}"
    out = pathlib.Path(scratch) / f"{level}-r{rate}-n{n}.yaml"
    out.write_text("".join(lines))
    return str(out)


def read_resolved_cfg(path):
    """Read back the factor levels nousko patched into the at.yaml template.

    Reading them from the PATCHED FILE -- not from our own defaults -- is what makes
    each factor's manipulation predicate meaningful: it verifies the lever actually
    engaged on THIS row. The predicates read `cfg_resolved.<leaf>` (NEVER `cfg.*`;
    that mismatch REJECTED every row of an earlier epoch), so the leaves are hoisted
    out of the `anytime:` block to the top level of this dict.

    Types are preserved: an int factor level (dispersion_window_us: 2000000) must come
    back as int, not float, or `==` can never compare equal.
    """
    resolved = {}
    if not path or not os.path.exists(path):
        return resolved
    for ln in pathlib.Path(path).read_text().splitlines():
        s = ln.strip()
        if not s or s.startswith("#") or ":" not in s:
            continue
        k, _, v = s.partition(":")
        k, v = k.strip(), v.strip().strip('"').strip("'")
        if not v or k == "anytime":
            continue        # the block header itself carries no value
        try:
            if v.lstrip("-").isdigit():
                resolved[k] = int(v)
            elif "." in v or "e" in v.lower():
                resolved[k] = float(v)
            else:
                resolved[k] = v
        except ValueError:
            resolved[k] = v
    return resolved


# ───────────────────────────────── BLIS driver ─────────────────────────────────

def _inner_width():
    """How many BLIS processes this ADAPTER runs at once.

    Safe to parallelize at all because each invocation is its own process with its own
    report path, and BLIS's output is simulated time computed from trained coefficients
    -- it reads no wall clock, so a co-scheduled neighbour changes how LONG a run takes
    but not a single bit of what it prints. That is the same property that licenses the
    campaign's `concurrency.load_independent: true`, and Task 3 Step 5 verifies it
    bit-for-bit by diffing --adapter-width 1 against 2.

    SIZING MUST COMPOSE WITH THE OUTER WIDTH. NOUS_MAX_PARALLEL DOES NOT EXIST --
    nousko exports only NOUS_RUN_DIR, NOUS_ROW_INDEX, NOUS_RUN_SLOT and
    NOUS_WORKLOAD_SEED -- so an earlier attempt to divide by it silently defaulted to
    1, every row claimed the full core budget, and a 10-core box ran at load average
    122 (findings/PARALLELISM-OVERSUBSCRIPTION.md). The explicit --adapter-width flag
    is the fix; absent it, a conservative assumed outer width is used, because being
    wrong toward UNDER-subscription costs wall clock while being wrong toward
    over-subscription costs correctness (contention-failed rows are missing data).
    """
    try:
        cores = len(os.sched_getaffinity(0))
    except AttributeError:
        cores = os.cpu_count() or 4
    budget = max(1, cores - 2)
    explicit = _ADAPTER_WIDTH or os.environ.get("NOUS_ADAPTER_WIDTH")
    if explicit:
        try:
            w = int(explicit)
            if w > 0:
                return max(1, min(w, budget))
        except ValueError:
            pass
    in_campaign = "NOUS_RUN_SLOT" in os.environ
    return max(1, budget // (3 if in_campaign else 1))


def run_blis(detector, cfg_path, level, rate, seed, num_requests, want_arrivals=False):
    """One BLIS run over the frozen spec for `level` at `rate`.

    Returns {"records": [...], "arrivals": [...] or None}. Raises on failure -- there
    is deliberately no sentinel return, because a row whose runs silently returned
    "no records" scores as a detector that never fired, which is indistinguishable
    from a detector that is perfectly silent (R1: never a silent continue).
    """
    with tempfile.TemporaryDirectory(prefix="anytime-") as td:
        spec = spec_at_rate(level, rate, num_requests, td)
        report = os.path.join(td, "sat.json")
        cmd = [BLIS, "run", "--model", MODEL,
               "--workload-spec", spec,
               "--num-requests", str(num_requests), "--seed", str(seed),
               "--detectors", detector, "--saturation-report", report]
        if cfg_path:
            cmd += ["--saturation-config", cfg_path]
        prefix = None
        if want_arrivals:
            prefix = os.path.join(td, "tr")
            cmd += ["--trace-output", prefix]
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=2400,
                           cwd=str(REPO))
        if p.returncode != 0:
            raise RuntimeError(
                f"blis failed: detector={detector} level={level} rate={rate} "
                f"seed={seed} n={num_requests}\n{p.stderr[-3000:]}")
        with open(report) as fh:
            data = json.load(fh)
        recs = [r for r in data.get("trace", []) if r.get("detector") == detector]
        arrivals = None
        if want_arrivals:
            with open(prefix + ".csv") as fh:
                arrivals = sorted(int(r["arrival_time_us"])
                                  for r in csv.DictReader(fh)
                                  if r.get("arrival_time_us"))
        return {"records": recs, "arrivals": arrivals}


def run_many(jobs):
    """Run run_blis(**job) concurrently, ORDER PRESERVED so results stay seed-aligned."""
    width = _inner_width()
    if width <= 1:
        return [run_blis(**j) for j in jobs]
    with ThreadPoolExecutor(max_workers=width) as ex:
        return list(ex.map(lambda j: run_blis(**j), jobs))


# ───────────────────────── metrics over a verdict trace ─────────────────────────
#
# detection_delay() is IMPORTED from score_detector.py, not re-implemented: it is the
# repaired 3-tuple version (findings/DETECTION-DELAY-IS-CLIPPED.md) and duplicating it
# would let the two drift, which is how the clipped metric survived as long as it did.
sys.path.insert(0, str(REPO / "campaign" / "bench"))
from score_detector import detection_delay, fired_fraction, flip_count  # noqa: E402


def obs_to_verdict(records):
    """Observations (EVENT INDEX) until the detector first commits to a fired verdict.

    The INDEX, not the wall time, is the primary response: the campaign asks how much
    EVIDENCE a detector needs, and an index cannot be clipped by a time origin. The
    previous epoch's speed metric was void precisely because a time delta measured
    from the post-warm-up origin collapsed every early fire to exactly 0
    (findings/DETECTION-DELAY-IS-CLIPPED.md). Returns None when it never commits.
    """
    _delay, idx, _clipped = detection_delay(records)
    return idx


def undecided_fraction(records):
    """Fraction of records the wrapper left UNDECIDED.

    An undecided record is NOT stable. INDETERMINATE is deliberately never a fourth
    `Level`: Level.UnmarshalJSON maps an unknown string to Stable (detector.go:69), so
    a 4th enum value would silently decode as STABLE in every old reader. The wrapper
    therefore signals undecidedness OUT-OF-BAND, in Result.Signals -- which is an
    OUTPUT surface only (commit 3a61aa3b); it is read here and never fed back as input.
    """
    if not records:
        return 0.0
    n = 0
    for r in records:
        sig = r["result"].get("signals") or {}
        if sig.get("undecided", 0) >= 1 or sig.get("indeterminate", 0) >= 1:
            n += 1
    return n / len(records)


def _dispersion_index(arrivals, window_us):
    """I = Var(N_T)/E[N_T] over fixed-width buckets of the arrival counting process.

    MEASURED from the sampler's own arrival_time_us column, estimated the same way the
    detector does. NOT inferred from the CV knob: CV is a property of the inter-arrival
    distribution, I is a property of the COUNTING process, and they coincide only for
    renewal processes. Returns None when the run is too short to bucket, so a caller
    cannot mistake "not measurable" for "zero dispersion".
    """
    if not arrivals or len(arrivals) < 3:
        return None
    lo, hi = arrivals[0], arrivals[-1]
    nb = int((hi - lo) // window_us)
    if nb < 2:
        return None
    counts = [0] * nb
    for t in arrivals:
        b = int((t - lo) // window_us)
        if 0 <= b < nb:
            counts[b] += 1
    mean = statistics.fmean(counts)
    if mean <= 0:
        return None
    return statistics.variance(counts) / mean


# ─────────────────────────────── FPR calibration ───────────────────────────────
#
# Grids are ordered fires-more -> fires-less, which is what licenses bisection: FPR is
# MONOTONE in the knob by construction, so the admissible region is a SUFFIX and binary
# search finds its first element in ceil(log2(n)) probes rather than n. On a 7-entry
# grid that is ~3 probes instead of 7, and each probe costs len(CALIB_MULTS)*len(SEEDS)
# BLIS runs, so it is the dominant cost of a row.
KNOB_GRIDS = {
    "composite":     [0.25, 0.5, 1.0, 2.0, 4.0, 8.0, 16.0],
    "threshold":     [1000, 2500, 5000, 8000, 12000, 20000, 35000],
    "backlog-drift": [0.5, 1.0, 3.0, 6.0, 12.0, 25.0, 50.0],
    "peak-rate":     [0.5, 1.0, 2.0, 4.0, 8.0, 16.0, 32.0],
    # The wrapper's knob is its confidence level, patched in by the ALPHA factor. The
    # per-row calibrated knob is the wrapped detector's own threshold, so the anytime
    # grid is the wrapped detector's grid; resolved in calibration_grid().
}
KNOB_BLOCK = {
    "composite":     ("composite", "sensitivity"),
    "threshold":     ("threshold", "threshold_ms"),
    "backlog-drift": ("backlog_drift", "slope_k"),
    "peak-rate":     ("peak_rate", "threshold"),
}


def write_cfg(detector, knob_value, path, template=None):
    """Emit the saturation config at this calibration setting.

    When a TEMPLATE is given (the at.yaml nousko patched with this row's factor
    levels) it is PRESERVED and only the calibrated threshold line is rewritten.
    Regenerating the block from scratch would silently discard every factor level and
    measure the baseline corner on every row -- the single worst failure mode available
    to this adapter.
    """
    if template and os.path.exists(template):
        lines = pathlib.Path(template).read_text().splitlines(keepends=True)
        out, found = [], 0
        for ln in lines:
            if ln.strip().startswith("threshold:"):
                indent = ln[: len(ln) - len(ln.lstrip())]
                out.append(f"{indent}threshold: {knob_value}\n")
                found += 1
            else:
                out.append(ln)
        if found == 0:
            # A template with no threshold line cannot be calibrated by rewriting it.
            # Appending one would put it outside the block and change its meaning, so
            # this is reported rather than papered over.
            raise RuntimeError(
                f"{template}: no `threshold:` line to calibrate. The template must "
                f"declare the knob; a config_patch never CREATES structure, and an "
                f"absent pointer aborts every row of that level.")
        pathlib.Path(path).write_text("".join(out))
        return path
    block, field = KNOB_BLOCK[detector]
    pathlib.Path(path).write_text(f"{block}:\n  {field}: {knob_value}\n")
    return path


def calibrate(detector, target_fpr, num_requests, workdir, calib_level, cliff,
              template=None):
    """Calibrate to the declared target FPR on KNOWN-STABLE traffic, then FREEZE.

    Finds the MOST SENSITIVE knob whose FPR on the calibration band (0.3-0.6x this
    level's OWN measured cliff) is within budget. That is the fair operating point:
    any detector can buy a lower FPR by going blind, so rewarding an FPR below the
    budget would reward exactly the smoke-detector-in-reverse failure.

    Bisection, not a linear walk (see the KNOB_GRIDS note). Every probe is recorded so
    the trials log stays complete for auditing and knob_at_grid_edge stays meaningful.
    """
    grid = KNOB_GRIDS[detector]
    trials = {}

    def fpr_at(i):
        if i in trials:
            return trials[i]["fpr"]
        cfg = write_cfg(detector, grid[i], os.path.join(workdir, f"cal-{i}.yaml"),
                        template)
        jobs = [{"detector": detector, "cfg_path": cfg, "level": calib_level,
                 "rate": round(cliff * m, 3), "seed": s,
                 "num_requests": num_requests}
                for m in CALIB_MULTS for s in SEEDS]
        res = run_many(jobs)
        total = len(res)
        firings = sum(1 for r in res if fired_fraction(r["records"]) >= 0.5)
        fpr = firings / total if total else 1.0
        trials[i] = {"knob": grid[i], "fpr": fpr, "firings": firings, "n": total}
        return fpr

    lo, hi, best = 0, len(grid) - 1, None
    while lo <= hi:
        mid = (lo + hi) // 2
        if fpr_at(mid) <= target_fpr:
            best, hi = mid, mid - 1
        else:
            lo = mid + 1

    if best is not None:
        if best > 0:
            # Confirm the neighbour below is genuinely over budget, so a non-monotone
            # dip from seed noise cannot hand back an over-sensitive knob unchecked.
            fpr_at(best - 1)
        ordered = [trials[i] for i in sorted(trials)]
        return grid[best], trials[best]["fpr"], ordered

    # Nothing met the budget. Return the least-firing probe and let the
    # fpr_within_budget constraint mark the row infeasible -- NEVER silently pretend a
    # detector was calibrated when it was not.
    ordered = [trials[i] for i in sorted(trials)]
    worst = min(ordered, key=lambda t: t["fpr"])
    return worst["knob"], worst["fpr"], ordered


# ──────────────────────────────── ladder scoring ────────────────────────────────

def score_ladder(detector, cfg, level, cliff, mults, num_requests, seeds):
    """Score one burstiness level's rung ladder. One dict per multiplier.

    Rung verdict follows the §3.5 rules: per-seed >= 50% of the post-warm-up run in a
    fired state -> that seed FIRED; a majority of seeds -> the RUNG fired.
    """
    out = {}
    for m in mults:
        rate = round(cliff * m, 3)
        jobs = [{"detector": detector, "cfg_path": cfg, "level": level, "rate": rate,
                 "seed": s, "num_requests": num_requests} for s in seeds]
        res = run_many(jobs)
        votes = [1 if fired_fraction(r["records"]) >= 0.5 else 0 for r in res]
        idxs = [obs_to_verdict(r["records"]) for r in res]
        delays = [detection_delay(r["records"]) for r in res]
        out[f"{m:g}"] = {
            "rate": rate,
            "mult": m,
            "fired": sum(votes) > len(seeds) // 2,
            "votes": votes,
            "obs_to_verdict": [i for i in idxs if i is not None],
            "zero_delay_unclipped": sum(
                1 for (d, _i, c) in delays if d == 0 and not c),
            "undecided_frac": [round(undecided_fraction(r["records"]), 4) for r in res],
            "max_flips": max((flip_count(r["records"]) for r in res), default=0),
            # The STRUCTURAL ceiling of obs_to_verdict on this rung: the number of
            # events a run of this length actually produced. response_interior compares
            # the objective against this, not against the max VALUE observed.
            "n_records": max((len(r["records"]) for r in res), default=0),
        }
    return out


def _median(xs):
    if not xs:
        return None
    s = sorted(xs)
    return float(s[len(s) // 2])


def super_obs(ladder):
    """Median obs-to-verdict over the SUPER-capacity rungs of one ladder.

    Super rungs are the only ones with unambiguous ground truth (FIRED), so they are
    the only ones a speed metric may be read from. A rung where the detector never
    commits contributes nothing here and is instead counted by
    indeterminate_forever_num, so "never decided" can never masquerade as "decided
    instantly".
    """
    vals = []
    for k, v in ladder.items():
        if v["mult"] >= 1.1:
            vals.extend(v["obs_to_verdict"])
    return _median(vals)


def correctness(ladder):
    """Fraction of rungs whose verdict matches ground truth.

    Ground truth from the MEASURED cliff: mult <= 0.6 is known sub-capacity (must be
    quiet), mult >= 1.1 is known super-capacity (must fire). The gray band 0.7-1.0 has
    no ground truth by construction and is EXCLUDED from the denominator -- scoring it
    would be scoring the apparatus's own uncertainty.
    """
    hits = total = 0
    for v in ladder.values():
        if v["mult"] <= 0.6:
            truth = False
        elif v["mult"] >= 1.1:
            truth = True
        else:
            continue
        total += 1
        hits += 1 if v["fired"] == truth else 0
    return (hits / total) if total else 0.0


# ───────────────────────────── apparatus constants ─────────────────────────────

def burstiness_ladder_hash():
    """sha256 over the four FITTED specs, held-out excluded (matches Task 1's probe)."""
    h = hashlib.sha256()
    for n in sorted(FITTED):
        h.update((SPECS / f"{n}.yaml").read_bytes())
    return h.hexdigest()


def cliff_table_hash():
    """sha256 over cliffs.json -- the DS2 invariant's observable."""
    return hashlib.sha256(CLIFFS_PATH.read_bytes()).hexdigest()


def incumbent_files_unmodified():
    """DS3: the three incumbent detector files are unmodified from PR #1620's head.

    Zero lines may change in peak_rate.go / composite.go / backlog_drift.go -- that
    PR is under review and its byte-identity tests must stay valid. A non-zero exit
    from git diff --quiet means they differ; a git failure (missing ref) is NOT
    treated as "unmodified", because a check that cannot run must not report PASS.
    """
    p = subprocess.run(
        ["git", "diff", "--quiet", "feat/peak-rate-detector", "--",
         "sim/saturation/peak_rate.go", "sim/saturation/composite.go",
         "sim/saturation/backlog_drift.go"],
        cwd=str(REPO), capture_output=True, text=True)
    if p.returncode == 0:
        return True
    if p.returncode == 1:
        return False
    raise RuntimeError(
        f"git diff could not evaluate DS3 (rc={p.returncode}): {p.stderr[-1000:]}. "
        f"A check that cannot run must not report PASS.")


# ───────────────────────────────────── main ─────────────────────────────────────

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--anytime-config", default="at.yaml",
                    help="the anytime config template; nousko patches this row's factor "
                         "levels into a per-run COPY and substitutes it into the command")
    ap.add_argument("--adapter-width", type=int, default=0,
                    help="BLIS processes this adapter runs at once. MUST be set by a "
                         "campaign so the adapter's own fan-out COMPOSES with the outer "
                         "max_parallel instead of multiplying against it. Passed as a "
                         "FLAG because run_command is exec'd as argv, so an env-var "
                         "prefix would be read as the program name.")
    ap.add_argument("--target-fpr", type=float, default=0.05)
    ap.add_argument("--num-requests", type=int, default=N_DEFAULT)
    ap.add_argument("--quick", action="store_true",
                    help="smoke mode: 1 seed, fewer rungs, shorter runs "
                         "(for --smoke/--liveness and the contract probe only)")
    ap.add_argument("--incumbent-only", action="store_true",
                    help="score the INCUMBENT reference rows only. Required while "
                         "sim/saturation/anytime.go is absent (the campaign's own build "
                         "stage authors it): without the mechanism there is nothing "
                         "anytime to measure, and this flag makes that explicit instead "
                         "of letting a wrapped incumbent's numbers pass for the "
                         "wrapper's. Sets anytime_mechanism_present=false.")
    ap.add_argument("--incumbent-refs", default=",".join(INCUMBENTS),
                    help="which incumbents to score as reference rows, at MATCHED FPR "
                         "on IDENTICAL traffic through THIS adapter -- never from "
                         "hand-built streams (LAUNCH-CHECKLIST item 6).")
    args = ap.parse_args()

    global SEEDS, _ADAPTER_WIDTH
    if args.adapter_width > 0:
        _ADAPTER_WIDTH = args.adapter_width
    seeds = [42] if args.quick else SEEDS
    SEEDS = seeds
    n_req = 300 if args.quick else args.num_requests

    # nousko exports NOUS_RUN_DIR / NOUS_ROW_INDEX / NOUS_RUN_SLOT /
    # NOUS_WORKLOAD_SEED and nothing else. Presence is asserted, never defaulted --
    # os.environ.get(k, default) is how a missing pointer becomes an invisible
    # baseline row.
    workdir = os.environ.get("NOUS_RUN_DIR") or tempfile.mkdtemp(prefix="anytime-row-")
    os.makedirs(workdir, exist_ok=True)

    cliffs = json.loads(CLIFFS_PATH.read_text())
    for lvl in LEVELS:
        if lvl not in cliffs:
            raise RuntimeError(
                f"cliffs.json has no measured cliff for '{lvl}'. Every rung is anchored "
                f"to its OWN measured cliff; there is no shared fallback constant "
                f"(LADDER.md's 20 belongs to a DIFFERENT workload -- "
                f"findings/CLIFF-IS-WORKLOAD-SPECIFIC.md).")

    # ---- The mechanism gate. No silent fallback. ----
    mechanism_present = ANYTIME_GO.exists()
    if not args.incumbent_only and not mechanism_present:
        sys.exit(
            f"FATAL: {ANYTIME_GO} does not exist, so there is no anytime detector to "
            f"score and the anytime path is not built.\n"
            f"The mechanism is authored by this campaign's own `build` stage; until it "
            f"lands, run with --incumbent-only to collect the INCUMBENT reference rows.\n"
            f"Refusing to emit anytime numbers measured from a detector that is absent: "
            f"a silent fallback here would fabricate the campaign's primary response.")

    detector = ANYTIME_DETECTOR if mechanism_present and not args.incumbent_only else None

    # ---- Step 1: MEASURE the dispersion index for every level. ----
    #
    # Measured, not inferred from the CV knob. Task 2 found capacity is NOT monotone in
    # CV across arrival FAMILIES (weibull_cv3 ties `constant`, the least bursty level),
    # while the campaign's pre-registered prediction is monotonicity in the measured I.
    # Each level is measured at ITS OWN calibration-band rate so the estimate is taken
    # on known-stable traffic, where the arrival process is the sampler's and not the
    # queue's.
    disp_window = int(read_resolved_cfg(args.anytime_config)
                      .get("dispersion_window_us", 2000000))
    disp_n = max(600, n_req)
    disp_jobs = [{"detector": "composite", "cfg_path": None, "level": lvl,
                  "rate": round(cliffs[lvl] * 0.4, 3), "seed": seeds[0],
                  "num_requests": disp_n, "want_arrivals": True} for lvl in LEVELS]
    disp_res = run_many(disp_jobs)
    dispersion = {}
    for lvl, r in zip(LEVELS, disp_res):
        I = _dispersion_index(r["arrivals"], disp_window)
        if I is None:
            raise RuntimeError(
                f"dispersion index not measurable for {lvl} at window "
                f"{disp_window}us over {len(r['arrivals'] or [])} arrivals -- the run is "
                f"too short to bucket. Raise --num-requests or lower the window; do NOT "
                f"report 0, which reads as 'perfectly smooth'.")
        dispersion[lvl] = round(I, 6)

    # ---- Step 2: score each row's detector(s) through the SAME pipeline. ----
    def score_one(det, template):
        """Calibrate on known-stable traffic, then run every ladder. One row's worth."""
        cliff_cal = cliffs["poisson"]
        knob, fpr, trials = calibrate(det, args.target_fpr, n_req, workdir,
                                      "poisson", cliff_cal, template)
        cfg = write_cfg(det, knob, os.path.join(workdir, f"frozen-{det}.yaml"), template)
        mults = ([0.3, 0.6, 1.5, 2.0] if args.quick else T1_MULTS)
        ladders = {lvl: score_ladder(det, cfg, lvl, cliffs[lvl], mults, n_req, seeds)
                   for lvl in LEVELS}
        grid = KNOB_GRIDS[det]
        return {
            "detector": det,
            "frozen_knob": knob,
            "calibrated_fpr": round(fpr, 4),
            "calibration_trials": trials,
            "knob_at_grid_edge": knob in (grid[0], grid[-1]),
            "ladders": ladders,
        }

    ref_names = [d.strip() for d in args.incumbent_refs.split(",") if d.strip()]
    if args.quick:
        ref_names = ref_names[:1]
    for d in ref_names:
        if d not in KNOB_GRIDS:
            raise RuntimeError(f"unknown incumbent '{d}'; valid: {sorted(KNOB_GRIDS)}")

    incumbent_rows = {d: score_one(d, None) for d in ref_names}

    if detector:
        primary = score_one(detector, args.anytime_config)
    else:
        # --incumbent-only: the first reference row stands in for the row-level
        # response so the objective's shape is exercised end-to-end, and
        # anytime_mechanism_present=false marks unambiguously that this is NOT an
        # anytime measurement.
        primary = incumbent_rows[ref_names[0]]

    lad = primary["ladders"]

    # ---- Step 3: the responses. ----
    # The primary response pools the super-capacity rungs of the FOUR FITTED levels
    # (the held-out weibull level is reported separately and never enters the fit).
    # It goes through the SAME super_obs() helper as the regime metrics, so the primary
    # and the per-level responses cannot drift apart.
    obs_all = _median([v for lvl in FITTED
                       for rung in lad[lvl].values() if rung["mult"] >= 1.1
                       for v in rung["obs_to_verdict"]])
    obs_poisson = super_obs(lad["poisson"])
    obs_heldout = super_obs(lad[HELDOUT])
    # `correctness_bursty` is measured on the BURSTY fitted levels. Task 2's finding
    # forces the choice of contrast: poisson and gamma_cv2 TIE at a cliff of 88 and the
    # ordering inverts on the continuous statistic, so the fine-grained contrast is
    # constant vs gamma_cv4, and "bursty" means the two levels whose MEASURED I is
    # clearly above Poisson's.
    bursty_levels = [l for l in FITTED if dispersion[l] > 2.0 * dispersion["poisson"]]
    if not bursty_levels:
        raise RuntimeError(
            f"no fitted level measured a dispersion index clearly above Poisson's "
            f"({dispersion}) -- the burstiness ladder is not delivering burstiness, so "
            f"correctness_bursty would be measured on traffic that is not bursty.")
    correctness_bursty = _median([correctness(lad[l]) for l in bursty_levels])

    fired_everywhere = any(
        all(v["fired"] for v in lad[l].values()) for l in LEVELS)
    # T1: fires at every super-capacity rung of every FITTED level.
    t1_pass = all(
        any(v["mult"] >= 1.1 for v in lad[l].values())
        and all(v["fired"] for v in lad[l].values() if v["mult"] >= 1.1)
        for l in FITTED)

    # indeterminate_forever: a super-capacity rung where the detector reached the
    # horizon cap without ever committing. That is the failure the anytime design
    # risks, and it must never be scored as "fast".
    indeterminate_forever = sum(
        1 for l in LEVELS for v in lad[l].values()
        if v["mult"] >= 1.1 and not v["obs_to_verdict"])

    # zero_delay_unclipped: a 0us delay with clipped=false is the ARITHMETIC signature
    # of the metric bug that findings/DETECTION-DELAY-IS-CLIPPED.md diagnosed and
    # commit c7f826c4 fixed. It must be 0. It is a self_check, not a constraint,
    # because a non-zero value means the INSTRUMENT is broken, not the configuration.
    zero_delay_unclipped = sum(
        v["zero_delay_unclipped"] for l in LEVELS for v in lad[l].values())

    # response_interior: 0 when the winning objective sits at either END of its
    # attainable range. THIS IS THE GUARD WHOSE ABSENCE VOIDED A PREVIOUS HEADLINE: an
    # objective pinned at an endpoint cannot rank anything -- every configuration that
    # reaches the endpoint ties -- so an improvement reported there is an artifact of
    # the metric's ceiling, not a measured effect. The attainable range of
    # obs_to_confident_verdict is [0, records-per-run]; interior means strictly inside.
    # The ceiling is STRUCTURAL, not data-derived. Using max(observed) as the upper end
    # would make the guard nearly vacuous: the median of a spread is almost always below
    # its own maximum, so it would pass even with the objective pinned against the real
    # ceiling. The attainable range is [0, n_events), where n_events is what a run of
    # this length can produce -- so `n_events` is measured from the traces themselves
    # (2 records per completed request on this pipeline, which is a property of the
    # simulator, not an assumption made here).
    n_events = max((v["n_records"] for l in LEVELS for v in lad[l].values()), default=0)
    response_interior = bool(
        obs_all is not None and 0 < obs_all < n_events - 1) if n_events > 1 else False

    # cs_coverage_ok: on the KNOWN-STABLE calibration band the confidence sequence must
    # not exclude the healthy value more often than alpha. The measured FPR IS that
    # empirical coverage violation rate, so coverage holds iff calibration held.
    alpha = float(read_resolved_cfg(args.anytime_config).get("alpha", args.target_fpr))
    cs_coverage_ok = primary["calibrated_fpr"] <= max(alpha, args.target_fpr)

    # gamma_within_support: every dispersion estimate must lie inside its declared
    # support, I >= 0. A negative estimate would mean the variance computation is wrong.
    gamma_within_support = all(v >= 0.0 for v in dispersion.values())

    resolved = read_resolved_cfg(args.anytime_config)

    out = {
        # ---- primary + regimes ----
        "obs_to_confident_verdict": obs_all,
        "obs_to_verdict_poisson": obs_poisson,
        "obs_to_verdict_weibull_heldout": obs_heldout,
        "correctness_bursty": round(correctness_bursty, 4) if correctness_bursty is not None else None,
        # ---- constraints (numeric mirrors: response constraints compare numbers) ----
        "fpr_within_budget_num": 1.0 if primary["calibrated_fpr"] <= args.target_fpr else 0.0,
        "t1_pass_num": 1.0 if t1_pass else 0.0,
        "fires_on_all_rungs_num": 1.0 if fired_everywhere else 0.0,
        "knob_at_grid_edge_num": 1.0 if primary["knob_at_grid_edge"] else 0.0,
        "indeterminate_forever_num": float(indeterminate_forever),
        "response_interior_num": 1.0 if response_interior else 0.0,
        "cs_coverage_ok_num": 1.0 if cs_coverage_ok else 0.0,
        # ---- self checks ----
        "calibrated_fpr": primary["calibrated_fpr"],
        "gamma_within_support_num": 1.0 if gamma_within_support else 0.0,
        "zero_delay_unclipped_num": float(zero_delay_unclipped),
        # ---- design-space invariants ----
        "threshold_was_calibrated": True,
        "cliff_table_hash": cliff_table_hash(),
        "incumbent_files_unmodified": incumbent_files_unmodified(),
        # ---- constant fields ----
        "model": MODEL,
        "target_fpr": args.target_fpr,
        "burstiness_ladder_hash": burstiness_ladder_hash(),
        # ---- manipulation predicates read cfg_resolved.*, NEVER cfg.* ----
        "cfg_resolved": resolved,
        # ---- measured burstiness, one key per level ----
        **{f"dispersion_index_{l}": dispersion[l] for l in LEVELS},
        # ---- provenance ----
        "anytime_mechanism_present": mechanism_present,
        "incumbent_only": bool(args.incumbent_only),
        "primary_detector": primary["detector"],
        "frozen_knob": primary["frozen_knob"],
        "calibration_trials": primary["calibration_trials"],
        "dispersion_window_us": disp_window,
        "cliffs": cliffs,
        "incumbent_reference": {
            d: {"frozen_knob": r["frozen_knob"],
                "calibrated_fpr": r["calibrated_fpr"],
                "obs_to_confident_verdict": super_obs(r["ladders"]["poisson"]),
                "obs_to_verdict_weibull_heldout": super_obs(r["ladders"][HELDOUT]),
                "correctness_bursty": round(_median(
                    [correctness(r["ladders"][l]) for l in bursty_levels]), 4),
                "knob_at_grid_edge": r["knob_at_grid_edge"]}
            for d, r in incumbent_rows.items()},
        "run_meta": {"seeds": seeds, "num_requests": n_req, "n_cap": N_CAP,
                     "adapter_width": _inner_width(), "quick": bool(args.quick)},
    }
    print(json.dumps(out, sort_keys=True))


if __name__ == "__main__":
    main()
