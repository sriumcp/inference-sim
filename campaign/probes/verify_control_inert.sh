#!/bin/bash
# Oracle 2(c) substitute: the mechanism must be INERT at its control level.
#
# nousko could not arm its own version of this check ("control must be unchanged across the
# build"), because run_command could not accept the control configuration before anytime.go
# existed -- recorded as pre_unavailable in baseline_equivalence.json. So this stands in for
# it, and it must be run BY HAND after the build lands.
#
# The property: absent an `anytime:` config block the wrapper must never construct, so BLIS's
# stdout must be BYTE-IDENTICAL to the pre-build binary's (INV-6). This is the campaign's DS3
# claim and the WRAPPED-R2 relation, checked here independently of both.
set -euo pipefail
cd /Users/sri/Documents/Projects/inference-sim/.worktrees/anytime-campaign

echo "=== 1. the three incumbent detector files must be untouched ==="
if git diff --quiet feat/peak-rate-detector -- \
     sim/saturation/peak_rate.go sim/saturation/composite.go sim/saturation/backlog_drift.go; then
  echo "   PASS: zero lines changed vs PR #1620"
else
  echo "   FAIL: an incumbent detector file was modified"; git diff --stat feat/peak-rate-detector -- sim/saturation/; exit 1
fi

echo "=== 2. build the post-build binary ==="
go build -o /tmp/blis-postbuild main.go && echo "   built"

echo "=== 3. reference binary from the PRE-build tree (PR #1620 head) ==="
REF=$(mktemp -d)
git worktree add -q --detach "$REF" feat/peak-rate-detector
( cd "$REF" && go build -o /tmp/blis-prebuild main.go ) && echo "   built"

echo "=== 4. default run: stdout must be BYTE-IDENTICAL (no anytime: block) ==="
ARGS="run --model meta-llama/llama-3.1-8b-instruct --workload-spec \
campaign/apparatus/burstiness/poisson.yaml --num-requests 200 --seed 42"
/tmp/blis-prebuild  $ARGS > /tmp/pre.out  2>/dev/null
/tmp/blis-postbuild $ARGS > /tmp/post.out 2>/dev/null
if diff -q /tmp/pre.out /tmp/post.out >/dev/null; then
  echo "   PASS: byte-identical -- the mechanism is inert at its control level (INV-6)"
else
  echo "   FAIL: default output CHANGED. The wrapper is not inert."; diff /tmp/pre.out /tmp/post.out | head -20; git worktree remove --force "$REF"; exit 1
fi

echo "=== 5. same with every incumbent detector selected ==="
for d in composite threshold backlog-drift peak-rate; do
  /tmp/blis-prebuild  $ARGS --detectors $d > /tmp/pre.$d.out  2>/dev/null
  /tmp/blis-postbuild $ARGS --detectors $d > /tmp/post.$d.out 2>/dev/null
  diff -q /tmp/pre.$d.out /tmp/post.$d.out >/dev/null \
    && echo "   PASS $d byte-identical" \
    || { echo "   FAIL $d CHANGED"; diff /tmp/pre.$d.out /tmp/post.$d.out | head; git worktree remove --force "$REF"; exit 1; }
done

git worktree remove --force "$REF"
echo "=== ALL PASS: oracle 2(c) satisfied by hand ==="
