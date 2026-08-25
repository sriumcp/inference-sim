# Anytime-Valid Saturation Detection — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the instrument a nousko `kind: optimization` campaign needs in order to
measure whether a saturation detector can derive its own horizon from evidence, and
whether that horizon tracks the traffic's measured burstiness.

**Architecture:** A wrapper detector (`sim/saturation/anytime.go`) composes an existing
BLIS detector through the exported 4-method `Detector` interface, maintains an online
index of dispersion over arrival timestamps, and applies an anytime-valid confidence
sequence to decide *when* the evidence is decisive. The campaign's `plan` and `build`
stages author the mechanism; this plan builds everything that must be **frozen and
identical across every row** — the apparatus, the adapter, the metric — plus the 14
native tests the campaign's factor relations declare.

**Tech Stack:** Go 1.22+ (detector, tests), Python 3.11+ (bench adapter; `yaml` module
required — plain `python3` on this box lacks it), `nous` CLI, BLIS `--saturation-config`
and `--workload-spec`.

**Spec:** `campaign/DESIGN-anytime-valid.md`
**Campaign:** `campaign/anytime-valid-detector.yaml` (validates: OK, 0 errors)

## Global Constraints

- **ZERO lines change** in `sim/saturation/peak_rate.go`, `composite.go`,
  `backlog_drift.go`. PR #1620 is under review; its byte-identity tests must stay valid.
  Verified by `git diff feat/peak-rate-detector -- sim/ cmd/` being empty for those paths.
- **Branch/worktree:** `campaign/anytime-valid` in `.worktrees/anytime-campaign`. Never
  commit to `main`, `feat/peak-rate-detector`, `feat/swd-owd-saturation`, or nousko's
  `nousko`.
- **Verify `pwd` before every `git commit`.** A `cd` inside a compound command silently
  reset the session directory once in this work and landed campaign commits on `main`.
- **INV-6 byte-identity:** absent an `anytime:` config block, stdout must be unchanged.
- **`Result.Signals` is an OUTPUT surface** (commit `3a61aa3b`). Never read it as input.
- **INDETERMINATE is never a fourth `Level`** — `Level.UnmarshalJSON` maps unknown strings
  to `Stable` (`detector.go:69`), so a 4th value silently decodes as STABLE in old readers.
- **Adapter env vars:** nousko exports only `NOUS_RUN_DIR`, `NOUS_ROW_INDEX`,
  `NOUS_RUN_SLOT`, `NOUS_WORKLOAD_SEED`. Assert presence; never `os.environ.get(k, default)`.
- **Test command:** `go test ./sim/saturation/... -count=1 -run 'TestAnytime'`.
- **Lint:** `golangci-lint run ./sim/saturation/` must pass before every commit.

---

## File Structure

| File | Responsibility |
|---|---|
| `sim/saturation/anytime.go` | **(build stage authors)** wrapper detector: dispersion estimator, confidence sequence, verdict policy |
| `sim/saturation/anytime_test.go` | **(build stage authors)** the 14 native tests the factor relations declare |
| `sim/saturation/config.go` | **(build stage modifies)** `anytime:` block, resolver, registry case, `blockOwners()` entry |
| `campaign/apparatus/burstiness/*.yaml` | Task 1 — four frozen workload specs + held-out |
| `campaign/apparatus/BURSTINESS.md` | Task 2 — the four MEASURED cliffs (gates launch) |
| `campaign/bench/score_anytime.py` | Task 3 — the row adapter: one JSON object per config |
| `campaign/probes/*.py` | Tasks 1–3 — verification probes, already partly present |

**Task ordering rationale:** Tasks 1–3 are the instrument and must exist *before* the
campaign runs (`LAUNCH-CHECKLIST.md`: launching before the instrument is finished cost
three epochs). Task 4 is the launch gate. The mechanism itself is authored by the
campaign's own `plan`+`build` stages — this plan does **not** hand-write `anytime.go`,
because doing so would forfeit the measured advantage of designing first and leave
`mechanism_plan.json` describing code nobody planned.

---

### Task 1: Frozen burstiness workload specs

**Files:**
- Create: `campaign/apparatus/burstiness/constant.yaml`, `poisson.yaml`, `gamma_cv2.yaml`,
  `gamma_cv4.yaml`, `weibull_cv3_heldout.yaml`
- Create: `campaign/probes/verify_burstiness_specs.py`

**Interfaces:**
- Consumes: BLIS `--workload-spec` (the only route to a non-Poisson arrival process;
  `blis run` has no `--arrival-process` flag). Arrival processes available:
  `constant`, `poisson`, `gamma`, `weibull` with a `CV` knob (`sim/workload/arrival.go:220-259`).
- Produces: five spec paths, and `burstiness_ladder_hash` (sha256 over the four fitted
  specs, held-out excluded) — a `constant_fields` entry every row reports.

- [ ] **Step 1: Write the failing probe**

```python
# campaign/probes/verify_burstiness_specs.py
"""Each spec must load, name the intended process, and produce the intended dispersion.

I = Var(N_T)/E[N_T] is measured from the spec's own sampler via BLIS, NOT asserted from
the CV knob: CV is a property of the inter-arrival distribution, I is a property of the
counting process, and they coincide only for renewal processes. Measuring closes that gap.
"""
import hashlib, pathlib, subprocess, sys
import yaml   # NOTE: python3.11+, plain python3 on this box has no yaml module

SPECS = {
    "constant.yaml":            ("constant", None,  0.0),
    "poisson.yaml":             ("poisson",  1.0,   1.0),
    "gamma_cv2.yaml":           ("gamma",    2.0,   4.0),
    "gamma_cv4.yaml":           ("gamma",    4.0,  16.0),
    "weibull_cv3_heldout.yaml": ("weibull",  3.0,   9.0),
}
BASE = pathlib.Path("campaign/apparatus/burstiness")

def test_specs_exist_and_declare_intended_process():
    for name, (proc, cv, _I) in SPECS.items():
        d = yaml.safe_load((BASE / name).read_text())
        assert d["arrival"]["process"] == proc, f"{name}: process is {d['arrival']['process']}, want {proc}"
        if cv is not None:
            assert abs(d["arrival"]["cv"] - cv) < 1e-9, f"{name}: cv is {d['arrival']['cv']}, want {cv}"

def test_ladder_hash_excludes_heldout():
    fitted = sorted(n for n in SPECS if "heldout" not in n)
    assert len(fitted) == 4, "the fitted ladder is exactly four levels"
    h = hashlib.sha256()
    for n in fitted:
        h.update((BASE / n).read_bytes())
    print("burstiness_ladder_hash:", h.hexdigest())

if __name__ == "__main__":
    test_specs_exist_and_declare_intended_process()
    test_ladder_hash_excludes_heldout()
    print("PASS")
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `python3.11 campaign/probes/verify_burstiness_specs.py`
Expected: FAIL — `FileNotFoundError: campaign/apparatus/burstiness/constant.yaml`

- [ ] **Step 3: Read one committed workload spec to copy its shape**

Run: `ls sim/workload/testdata/*.yaml docs/**/workload*.yaml 2>/dev/null | head`
then `cat` one. Do NOT invent the schema — mirror a committed spec exactly, changing
only the `arrival` block. An invented key fails BLIS's strict YAML parsing (R10).

- [ ] **Step 4: Write the five specs**

Each is the SAME workload (prompt/output distributions, request count) with only the
`arrival` block differing — that is what makes the ladder a controlled comparison.
Mean rate is set per-level by the adapter at run time, not baked into the spec.

- [ ] **Step 5: Run the probe to verify it passes**

Run: `python3.11 campaign/probes/verify_burstiness_specs.py`
Expected: PASS, and it prints `burstiness_ladder_hash: <64 hex>`

- [ ] **Step 6: Commit**

```bash
cd /Users/sri/Documents/Projects/inference-sim/.worktrees/anytime-campaign
pwd && git branch --show-current    # MUST be campaign/anytime-valid
git add campaign/apparatus/burstiness campaign/probes/verify_burstiness_specs.py
git commit -m "feat(apparatus): five frozen burstiness specs (four fitted + held-out weibull)"
```

---

### Task 2: Measure the cliff for each burstiness level

**Files:**
- Create: `campaign/apparatus/BURSTINESS.md`
- Create: `campaign/probes/measure_cliffs.py`

**Interfaces:**
- Consumes: Task 1's five specs.
- Produces: `campaign/apparatus/cliffs.json` — `{level: r_nominal}` — and
  `cliff_table_hash`, the `DS2` invariant's observable. The adapter (Task 3) reads this
  file to anchor every rung.

**Why this task gates launch.** `r_nominal = 20 rps` was measured under **Poisson**
(`LADDER.md`). Bursty traffic saturates earlier, so reusing 20 for every level would put
the gamma ladders' base rungs above their own cliffs — making T1 unfalsifiable there.
`LAUNCH-CHECKLIST.md` item 2: *"A ladder that never tips makes its test vacuous. A base
rung that already tips makes it unfalsifiable."* Epoch 1 died from declaring rather than
measuring an apparatus constant.

- [ ] **Step 1: Write the failing probe**

```python
# campaign/probes/measure_cliffs.py
"""Locate each burstiness level's capacity cliff by the §1.1 backlog-divergence test.

Ground truth is NOT a latency threshold: a regime is super-capacity iff mean E2E keeps
GROWING with the horizon instead of stabilizing. Two horizons per rate; the knee is the
last rate whose growth stays under GROWTH_SUB.
"""
import json, pathlib, subprocess

GROWTH_SUB = 0.25          # <=25% growth n_lo->n_hi reads as sub-capacity
N_LO, N_HI = 800, 3200
SEED = 42
LEVELS = ["constant", "poisson", "gamma_cv2", "gamma_cv4", "weibull_cv3_heldout"]
OUT = pathlib.Path("campaign/apparatus/cliffs.json")

def mean_e2e(spec, rate, n, seed=SEED):
    """Run BLIS once; return mean E2E ms. Raise on failure -- never return a sentinel."""
    cmd = ["./blis", "run", "--model", "meta-llama/llama-3.1-8b-instruct",
           "--workload-spec", f"campaign/apparatus/burstiness/{spec}.yaml",
           "--rate", str(rate), "--num-requests", str(n), "--seed", str(seed),
           "--metrics-path", "/dev/stdout"]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=1800)
    if p.returncode != 0:
        raise RuntimeError(f"blis failed for {spec} rate={rate} n={n}: {p.stderr[-2000:]}")
    return json.loads(p.stdout)["mean_e2e_ms"]     # confirm the exact key in step 2

def find_cliff(spec, rates):
    knee = None
    for r in rates:
        lo, hi = mean_e2e(spec, r, N_LO), mean_e2e(spec, r, N_HI)
        growth = (hi - lo) / lo
        print(f"  {spec:24} rate={r:5.1f}  {lo:9.1f} -> {hi:9.1f}  {growth:+.1%}"
              f"  {'sub' if growth <= GROWTH_SUB else 'SUPER'}")
        if growth <= GROWTH_SUB:
            knee = r
        else:
            break
    if knee is None:
        raise RuntimeError(f"{spec}: every rate probed is already super-capacity -- "
                           f"lower the sweep, do not guess a cliff")
    return knee

if __name__ == "__main__":
    cliffs = {}
    for lvl in LEVELS:
        print(f"=== {lvl} ===")
        cliffs[lvl] = find_cliff(lvl, [8, 10, 12, 14, 16, 18, 20, 22, 24, 26])
    OUT.write_text(json.dumps(cliffs, indent=2, sort_keys=True) + "\n")
    print("\ncliffs:", cliffs)
    assert cliffs["gamma_cv4"] <= cliffs["poisson"], \
        "burstier traffic must not saturate LATER than Poisson -- if it does, the " \
        "specs do not differ in the way intended (check step 2 of Task 1)"
    print("PASS")
```

- [ ] **Step 2: Confirm the metrics key name before running the sweep**

Run: `./blis run --model meta-llama/llama-3.1-8b-instruct --rate 10 --num-requests 50 --metrics-path /dev/stdout | python3 -c "import json,sys; print(sorted(json.load(sys.stdin).keys()))"`
Fix `mean_e2e`'s key to whatever this prints. A wrong key raises `KeyError` — that is
intended; it must not fall back to a sentinel (R1: never silent `continue`).

- [ ] **Step 3: Run the sweep**

Run: `python3.11 campaign/probes/measure_cliffs.py > campaign/apparatus/cliff-sweep.log 2>&1`
then `cat campaign/apparatus/cliff-sweep.log`
Expected: five growth tables, `cliffs.json` written, and the burstier-saturates-earlier
assertion passing.

**Do not pipe this through `grep`** — hazard #2 in `LAUNCH-CHECKLIST.md`: piping a
backgrounded command's output through a filter consumes it and leaves an empty file that
reads as "no result". Redirect the full output, then filter the file.

- [ ] **Step 4: Write `BURSTINESS.md` with the measured tables**

Record, per level: the growth table, the chosen `r_nominal`, the measured dispersion index,
and the rung multipliers (reuse `LADDER.md`'s bands: calibration `0.3–0.6`, gray
`0.7–0.95`, cliff `1.0`, super `1.1–2.0`). State the freeze explicitly: an apparatus
change is an epoch boundary, not an edit.

- [ ] **Step 5: Verify each ladder crosses its own cliff**

Run: `python3.11 -c "
import json; c=json.load(open('campaign/apparatus/cliffs.json'))
for k,v in sorted(c.items()):
    print(f'{k:24} r_nominal={v:5.1f}  base_rung={0.3*v:5.1f}  top_rung={2.0*v:5.1f}')
"`
Expected: for every level, base rung well below and top rung well above its own cliff.

- [ ] **Step 6: Commit**

```bash
pwd && git branch --show-current
git add campaign/apparatus/BURSTINESS.md campaign/apparatus/cliffs.json \
        campaign/apparatus/cliff-sweep.log campaign/probes/measure_cliffs.py
git commit -m "feat(apparatus): measure the capacity cliff for each burstiness level

r_nominal=20 was measured under Poisson only; bursty traffic saturates earlier, so each
level gets its own measured cliff. LAUNCH-CHECKLIST item 2: a ladder that never tips makes
its test vacuous, and a base rung that already tips makes it unfalsifiable."
```

---

### Task 3: The row adapter

**Files:**
- Create: `campaign/bench/score_anytime.py`
- Create: `campaign/probes/verify_adapter_contract.py`

**Interfaces:**
- Consumes: Task 1's specs, Task 2's `cliffs.json`, the repaired
  `detection_delay(records, warmup_frac) -> (delay_us, index, clipped)` from
  `campaign/bench/score_detector.py` (import it; do not re-implement).
- Produces: exactly one JSON object on stdout per invocation, containing **every** key the
  campaign's `response` block reads:
  `obs_to_confident_verdict`, `obs_to_verdict_poisson`, `correctness_bursty`,
  `obs_to_verdict_weibull_heldout`, `fpr_within_budget_num`, `t1_pass_num`,
  `fires_on_all_rungs_num`, `knob_at_grid_edge_num`, `indeterminate_forever_num`,
  `response_interior_num`, `cs_coverage_ok_num`, `calibrated_fpr`,
  `gamma_within_support_num`, `zero_delay_unclipped_num`, `threshold_was_calibrated`,
  `incumbent_files_unmodified`, `model`, `target_fpr`, `burstiness_ladder_hash`,
  `cliff_table_hash`, plus `cfg_resolved` (the manipulation predicates read
  `cfg_resolved.*`, **not** `cfg.*` — that mismatch rejected every row of an earlier epoch).

- [ ] **Step 1: Write the failing contract probe**

```python
# campaign/probes/verify_adapter_contract.py
"""Every key the campaign YAML reads must appear in the adapter's output.

This is the check whose absence cost an epoch twice: predicates read cfg_resolved.* while
the adapter emitted cfg.* (every row REJECTED), and a factor pointer named nothing in the
template (every row of that level ABORTED). Derive the required set FROM THE YAML so the
two cannot drift.
"""
import json, subprocess, sys
import yaml

CAMPAIGN = "campaign/anytime-valid-detector.yaml"

def required_keys():
    d = yaml.safe_load(open(CAMPAIGN))
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
    d = yaml.safe_load(open(CAMPAIGN))
    return {f["manipulation"]["observable"].split(".")[0]
            for f in d["optimization"]["factors"]}

if __name__ == "__main__":
    out = subprocess.run([sys.executable, "campaign/bench/score_anytime.py",
                          "--anytime-config", "at.yaml", "--target-fpr", "0.05",
                          "--adapter-width", "1", "--quick"],
                         capture_output=True, text=True, timeout=3600)
    assert out.returncode == 0, f"adapter failed:\n{out.stderr[-4000:]}"
    got = json.loads(out.stdout)
    missing = sorted(required_keys() - set(got))
    assert not missing, f"adapter omits keys the campaign reads: {missing}"
    for p in manipulation_prefixes():
        assert p in got, f"manipulation predicates read '{p}.*' but it is absent"
    assert got["zero_delay_unclipped_num"] == 0, \
        "a 0us delay with clipped=false is the signature of the metric bug this replaces"
    print("PASS —", len(required_keys()), "required keys present")
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `python3.11 campaign/probes/verify_adapter_contract.py`
Expected: FAIL — `No such file or directory: campaign/bench/score_anytime.py`

- [ ] **Step 3: Write the adapter**

Structure it on `score_detector.py` (same `run_blis`/`run_many`/`calibrate` shape, same
`--adapter-width` **flag** discipline — never an env-var prefix, since `run_command` is
exec'd as argv). Additions specific to this campaign:

1. **Per-level rungs from `cliffs.json`**, never a shared constant.
2. **`obs_to_confident_verdict`** = observations until the wrapper first commits, median
   over super-capacity rungs; and `correctness_bursty` = fraction of bursty rungs whose
   committed verdict matches ground truth.
3. **Bisected FPR calibration** (FPR is monotone in the knob, so the admissible region is
   a suffix — 5 probes rather than 7, verified lossless in the previous epoch).
4. **`response_interior_num`** = 0 when the winning objective sits at either end of its
   attainable range. This is the guard the last epoch lacked.
5. **`incumbent_files_unmodified`** via
   `git diff --quiet feat/peak-rate-detector -- sim/saturation/peak_rate.go sim/saturation/composite.go sim/saturation/backlog_drift.go`.
6. **Incumbent reference rows** — score composite/peak-rate/threshold/backlog-drift through
   the same adapter so the comparison is at matched FPR on identical traffic (checklist
   item 6: never from hand-built streams).

- [ ] **Step 4: Run the probe to verify it passes**

Run: `python3.11 campaign/probes/verify_adapter_contract.py`
Expected: `PASS — N required keys present`

- [ ] **Step 5: Verify parallel == serial, bit for bit**

Run: `python3.11 campaign/bench/score_anytime.py --anytime-config at.yaml --target-fpr 0.05 --adapter-width 1 --quick > /tmp/ser.json && python3.11 campaign/bench/score_anytime.py --anytime-config at.yaml --target-fpr 0.05 --adapter-width 2 --quick > /tmp/par.json && diff /tmp/ser.json /tmp/par.json && echo "BIT-IDENTICAL"`
Expected: `BIT-IDENTICAL`. This is the concrete evidence for the YAML's
`concurrency.load_independent: true` claim.

- [ ] **Step 6: Commit**

```bash
pwd && git branch --show-current
git add campaign/bench/score_anytime.py campaign/probes/verify_adapter_contract.py
git commit -m "feat(bench): row adapter for the anytime-valid campaign

Emits every key the campaign reads, derived FROM the YAML by a contract probe so the two
cannot drift -- the check whose absence rejected every row of one epoch (cfg vs
cfg_resolved) and aborted every row of a level in another (a pointer naming nothing)."
```

---

### Task 4: Launch gate

**Files:**
- Create: `campaign/LAUNCH-EVIDENCE-anytime.md`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: the recorded evidence for every `LAUNCH-CHECKLIST.md` item. An unchecked item
  is a discarded epoch — three of four prior epochs died here.

- [ ] **Step 1: Objective is FINAL**

Run the adapter once; confirm every `*_pass`/`*_num` key the objective consumes is present
(Task 3's probe already asserts this). Paste the output. No test may be added later.

- [ ] **Step 2: Every ladder crosses its cliff**

Paste Task 2's five growth tables and the base/top rung table.

- [ ] **Step 3: Horizon measured, not chosen**

For the anytime detector the horizon is DERIVED, so the frozen quantity is instead the
**cap** (`n = 6000`, matching the incumbent's frozen horizon so the comparison is fair)
and the `min_decided` floor. Record both, and record that reaching the cap without
committing is what `indeterminate_forever` scores.

- [ ] **Step 4: Concurrency bounded by a POSITIVE assertion**

Run a real row; during it, count processes:
`ps -eo command | grep -c '[b]lis run'` — expect ~6 (3 rows × 2 workers).
Record the count and `uptime`'s load average. Verified by counting, never by reading code.

- [ ] **Step 5: Every env var and pointer verified to EXIST**

`ps eww <pid>` on a live row for the env vars; confirm each factor's `pointer` addresses a
key already present in the `at.yaml` template the build authors (a `config_patch` never
creates structure — an absent pointer aborts every row of that level).

- [ ] **Step 6: No synthetic stream underpins a cross-detector claim**

Confirm every incumbent number comes from the simulator at matched FPR. Four false
conclusions in this work came from author-built streams.

- [ ] **Step 7: Pre-flight, BOTH stages**

```bash
nous validate campaign campaign/anytime-valid-detector.yaml --smoke
nous validate campaign campaign/anytime-valid-detector.yaml --smoke --liveness
```
Paste both outputs. Read the `--liveness` effect table and act on it: an axis measuring
zero effect gets narrowed or dropped **before** launch, not after. `--smoke` alone has
already caught three campaign-killing defects in this work.

**All 14 declared `native_test` identifiers must exist and run** — the validator warns
that a declared-but-absent test counts as a FAILED correctness relation and aborts at
`verify`. They are authored by the `build` stage, so this check runs after `build`.

- [ ] **Step 8: Commit the evidence, then launch**

```bash
pwd && git branch --show-current
git add campaign/LAUNCH-EVIDENCE-anytime.md
git commit -m "docs(campaign): launch evidence for every checklist item (epoch 1)"
nous run   # only now
```

---

## Self-Review

**Spec coverage.** Design §3/§3.1/§3.1.1 (wrapper, per-level CS target) → authored by the
campaign's `build` stage, constrained by `factor_nomination` facts 1–6 and pinned by the
14 native tests in the factor relations. §4/§4.1 (verdict alphabet, `Level` trap) →
`INDET-R1/R2/R3`. §5 (burstiness apparatus) → Tasks 1–2. §6/§6.1 (objective, repaired
metric) → Task 3, and the metric fix is already committed at `c7f826c4`. §7 (stages,
factor nomination) → the campaign YAML, validated. §8 (soundness checks) →
`cs_coverage_ok_num`, `gamma_within_support_num`, `response_interior_num`, plus
`BOUND-R1/R2`, `ALPHA-R1`, `DISPWIN-R1/R2/R3`. §9 (risks) → carried in
`guidance.interpretation`.

**One deliberate gap.** This plan does not hand-write `anytime.go`. That is the `plan`+
`build` stages' work, and pre-writing it would forfeit the measured advantage of pricing
the mechanism first (−10.4% vs +3.65%) and leave `mechanism_plan.json` describing code
nobody planned. What this plan owes the build is a complete specification, which is what
`factor_nomination` and the 14 relations are.

**Type consistency.** `detection_delay()` returns a 3-tuple `(delay_us, index, clipped)`
throughout; `score_detector.py` keeps the scalar `detection_delay_us()` shim for
back-compat. `cliffs.json` is `{level: rate}` keyed by the spec basenames in Task 1.
`cfg_resolved` (never `cfg`) is the manipulation-observable prefix in every factor.
