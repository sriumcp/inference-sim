# The parallel-identity check as specified could not pass

**Status:** corrected in Task 3 (`campaign/probes/verify_parallel_identity.py`)
**Severity:** a check that fails for the wrong reason, whose obvious "fix" deletes evidence
**Class:** a verification step that contradicts a requirement in the same plan

## What was wrong

`PLAN-anytime-valid.md` Task 3 Step 5:

```
python3.11 campaign/bench/score_anytime.py ... --adapter-width 1 > /tmp/ser.json &&
python3.11 campaign/bench/score_anytime.py ... --adapter-width 2 > /tmp/par.json &&
diff /tmp/ser.json /tmp/par.json && echo "BIT-IDENTICAL"
```

Expected: an empty diff. But the same plan requires the adapter to RECORD its resolved
adapter width in the output — it is the provenance for
`findings/PARALLELISM-OVERSUBSCRIPTION.md`, where an assumed-and-unrecorded width put a
10-core box at load average 122. So `run_meta.adapter_width` is 1 in one run and 2 in the
other **by construction**, and the whole-object diff fails on the one field that is
supposed to differ.

Measured, both runs `--quick --incumbent-only`:

```
1c1
< ... "run_meta": {"adapter_width": 1, ...} ...
> ... "run_meta": {"adapter_width": 2, ...} ...
```

Every other field — all 20 required response keys, all five dispersion indices, the
calibration trials, the incumbent reference block — is byte-identical.

## Why the obvious fix is worse than the bug

The tempting repair is to stop recording the width so the diff comes out empty. That
deletes the provenance for the exact failure mode that motivated recording it, and leaves
a real width regression invisible. The check would then "pass" while proving less than
before.

## The correction

`verify_parallel_identity.py` asserts the identity over every key EXCEPT
`run_meta.adapter_width`, and separately asserts that field **actually changed**:

```python
assert got_ser != got_par, "--adapter-width did not change the resolved width -- the knob
                            is INERT, so a bit-identical diff proves nothing"
```

The second assertion is the load-bearing half. Without it, an adapter that ignored
`--adapter-width` entirely would run serially both times, the diff would be trivially
empty, and the probe would report BIT-IDENTICAL having tested nothing — the same
inert-knob shape as `RATE-FLAG-IGNORED-UNDER-SPEC.md`, where `--rate` existed, was
accepted, and changed nothing.

On a real diff the probe prints the differing keys with both values, so a genuine
concurrency defect is diagnosed rather than merely reported.

## Result

```
BIT-IDENTICAL — every measured field matches at width 1 and 2;
only run_meta.adapter_width differs, as it records the knob
```

This is the evidence for the campaign's `concurrency.load_independent: true`. It holds
because BLIS reads no wall clock — simulated time is accumulated from trained latency
coefficients — so a co-scheduled neighbour changes how LONG a run takes but not what it
prints.
