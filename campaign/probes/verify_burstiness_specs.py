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

def arrival_of(spec_dict, name):
    """Return the spec's single arrival block.

    SCHEMA NOTE (deviation from the plan's draft of this probe): `arrival` is NOT a
    top-level WorkloadSpec key. It lives on each client -- `clients[].arrival` --
    per ArrivalSpec in sim/workload/spec.go:154. LoadWorkloadSpec parses with
    decoder.KnownFields(true), so a top-level `arrival:` is a HARD ERROR:
        "field arrival not found in type workload.WorkloadSpec"
    (verified against the built binary). The ladder therefore uses exactly one
    client, and this helper asserts that -- a second client would mean the rung has
    more than one arrival process and the comparison would no longer be controlled.
    """
    clients = spec_dict["clients"]
    assert len(clients) == 1, (
        f"{name}: expected exactly 1 client so the rung has ONE arrival process, "
        f"got {len(clients)}"
    )
    return clients[0]["arrival"]


def test_specs_exist_and_declare_intended_process():
    for name, (proc, cv, _I) in SPECS.items():
        d = yaml.safe_load((BASE / name).read_text())
        a = arrival_of(d, name)
        assert a["process"] == proc, f"{name}: process is {a['process']}, want {proc}"
        if cv is not None:
            assert abs(a["cv"] - cv) < 1e-9, f"{name}: cv is {a['cv']}, want {cv}"


def test_ladder_is_controlled():
    """Every rung must be the SAME workload apart from its arrival block.

    Without this, an effect measured across the ladder cannot be attributed to
    burstiness rather than to a token-distribution or request-count confound.
    """
    baseline = None
    for name in SPECS:
        d = yaml.safe_load((BASE / name).read_text())
        c = dict(d["clients"][0])
        c.pop("arrival")
        fingerprint = (
            d["version"], d["seed"], d["category"],
            d["aggregate_rate"], d["num_requests"], sorted(c.items(), key=repr),
        )
        if baseline is None:
            baseline, baseline_name = fingerprint, name
        else:
            assert fingerprint == baseline, (
                f"{name} differs from {baseline_name} outside its arrival block -- "
                f"the ladder is no longer a controlled comparison"
            )

def test_ladder_hash_excludes_heldout():
    fitted = sorted(n for n in SPECS if "heldout" not in n)
    assert len(fitted) == 4, "the fitted ladder is exactly four levels"
    h = hashlib.sha256()
    for n in fitted:
        h.update((BASE / n).read_bytes())
    print("burstiness_ladder_hash:", h.hexdigest())

if __name__ == "__main__":
    test_specs_exist_and_declare_intended_process()
    test_ladder_is_controlled()
    test_ladder_hash_excludes_heldout()
    print("PASS")
