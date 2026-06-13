#!/usr/bin/env bash
# leakcheck.sh — detect Go heap/RSS leaks in blis by checking whether peak RSS
# scales with TOTAL request count (N) while CONCURRENT state is held fixed.
#
# Methodology: a correct discrete-event sim holds memory ~ proportional to
# concurrent work (in-flight + queued), NOT cumulative work. So for fixed
# rate/concurrency, peak RSS should be roughly FLAT as N grows 10x. A family
# whose RSS grows ~linearly with N is retaining per-request/per-event objects
# that should be transient -> leak suspect.
#
# Peak RSS via /usr/bin/time -l (macOS) "maximum resident set size" (bytes).
# Growth ratio = RSS(N_big) / RSS(N_small). Flag when ratio >= FLAG_RATIO.
set -u

BLIS=./blis
MODEL=qwen/qwen3-14b
MOE_MODEL=Qwen/Qwen3-30B-A3B          # MoE for DP/EP families
N_SMALL=3000
N_BIG=30000                           # 10x
FLAG_RATIO=2.8                        # >= this multiplier on a 10x N increase = suspect
SEED=42

rss_bytes() { # $1=logfile -> prints maximum resident set size in bytes
  grep -E "maximum resident set size" "$1" | awk '{print $1}'
}

run_one() { # $1=N  $2..=blis args ; prints peak RSS bytes (or empty on failure)
  local n="$1"; shift
  local errf; errf=$(mktemp)
  /usr/bin/time -l "$BLIS" run --model "$MODEL" --seed "$SEED" \
    --num-requests "$n" "$@" >/dev/null 2>"$errf"
  local rc=$?
  if [ $rc -ne 0 ]; then
    echo "    FAILED (rc=$rc): $(grep -iE 'fatal|error|panic' "$errf" | head -1)" >&2
    rm -f "$errf"; echo ""; return 1
  fi
  rss_bytes "$errf"
  rm -f "$errf"
}

# $1 = family label ; $2.. = blis args (applied to BOTH N runs)
family() {
  local label="$1"; shift
  printf '%-46s' "$label"
  local rs rb
  rs=$(run_one "$N_SMALL" "$@") || { echo "  -> run error"; return; }
  rb=$(run_one "$N_BIG"   "$@") || { echo "  -> run error"; return; }
  if [ -z "$rs" ] || [ -z "$rb" ] || [ "$rs" -eq 0 ]; then
    echo "  -> no RSS captured"; return
  fi
  # ratio = rb/rs to 2 decimals
  local ratio
  ratio=$(awk -v a="$rb" -v b="$rs" 'BEGIN{printf "%.2f", a/b}')
  local smb bmb
  smb=$(awk -v x="$rs" 'BEGIN{printf "%.0f", x/1048576}')
  bmb=$(awk -v x="$rb" 'BEGIN{printf "%.0f", x/1048576}')
  local flag=""
  awk -v r="$ratio" -v t="$FLAG_RATIO" 'BEGIN{exit !(r>=t)}' && flag="  <<< LEAK SUSPECT"
  printf 'N=%-6s %4sMiB  N=%-6s %4sMiB  ratio=%s%s\n' \
    "$N_SMALL" "$smb" "$N_BIG" "$bmb" "$ratio" "$flag"
}

echo "=== blis leak sweep (RSS vs N, fixed concurrency) ==="
echo "small N=$N_SMALL  big N=$N_BIG (10x)  flag ratio>=$FLAG_RATIO"
echo

echo "--- open-loop arrival (rate fixed; N scales) ---"
family "baseline single-instance"            --rate 200
family "high rate (saturating queue)"        --rate 2000
family "summarization workload"              --rate 200 --workload summarization
family "contentgen workload"                 --rate 200 --workload contentgen
family "multidoc workload"                   --rate 200 --workload multidoc
family "chatbot workload"                    --rate 200 --workload chatbot
family "long outputs (heavy decode)"         --rate 200 --output-tokens 2000 --output-tokens-max 4000
family "long prompts (heavy prefill)"        --rate 200 --prompt-tokens 4000 --prompt-tokens-max 6000

echo
echo "--- closed-loop (concurrency fixed; N scales) ---"
family "closed-loop concurrency=64"          --concurrency 64
family "closed-loop +think-time"             --concurrency 64 --think-time-ms 500

echo
echo "--- cluster / routing ---"
family "cluster 8 instances round-robin"     --rate 800 --num-instances 8
family "cluster 8 weighted scorers"          --rate 800 --num-instances 8 --routing-policy weighted
family "cluster 8 least-loaded"              --rate 800 --num-instances 8 --routing-policy least-loaded

echo
echo "--- flow control / gateway queue ---"
family "flow-control utilization"            --rate 2000 --flow-control --saturation-detector utilization
family "flow-control + TTL"                  --rate 2000 --flow-control --saturation-detector utilization --request-ttl 5000000
family "flow-control + queue-shedding"       --rate 2000 --flow-control --saturation-detector utilization --max-gateway-queue-depth 500 --queue-shedding
family "flow-control + in-flight-eviction"   --rate 2000 --flow-control --saturation-detector utilization --in-flight-eviction

echo
echo "--- PD disaggregation ---"
family "PD prefill=2 decode=2"               --rate 400 --num-instances 4 --prefill-instances 2 --decode-instances 2 --pd-decider always

echo
echo "--- KV tiering / offload ---"
family "KV CPU offload tier"                 --rate 400 --total-kv-blocks 2000 --kv-cpu-blocks 700 --kv-offload-threshold 0.7

echo
echo "--- saturation detectors (post-hoc) ---"
family "post-hoc composite"                  --rate 2000 --post-hoc-detector composite
family "post-hoc threshold"                  --rate 2000 --post-hoc-detector threshold

echo
echo "--- CONTROL: trace export RETAINS all requests by design (expect growth) ---"
family "trace-output (expected linear)"      --rate 200 --trace-output /tmp/blis_leaktrace

echo
echo "Done. 'LEAK SUSPECT' = RSS grew ~linearly with N -> investigate with BLIS_MEMPROFILE."
