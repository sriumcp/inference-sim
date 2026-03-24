---
theme: default
title: BLIS Walkthrough
info: |
  A discrete-event simulator for LLM inference serving systems
class: text-center
drawings:
  persist: false
transition: slide-left
mdc: true
---

<style>
:root {
  --slidev-slide-container-font-size: 0.82em;
}
h1 {
  margin-bottom: 0.2em !important;
}
h3 {
  margin-top: 0.2em !important;
  margin-bottom: 0.15em !important;
}
.slidev-layout {
  padding-top: 1.2rem !important;
  padding-bottom: 0.8rem !important;
  font-size: 0.9em;
}
.slidev-layout h1 + p,
.slidev-layout h1 + div,
.slidev-layout h1 + ul,
.slidev-layout h1 + table,
.slidev-layout h1 + pre {
  margin-top: 0.15em !important;
}
table {
  font-size: 0.85em;
}
table th, table td {
  padding: 0.25em 0.5em !important;
}
pre, code {
  font-size: 0.85em !important;
}
blockquote {
  margin-top: 0.2em !important;
  margin-bottom: 0.2em !important;
  font-size: 0.88em;
}
li {
  line-height: 1.4 !important;
  margin-bottom: 0.05em !important;
}
</style>

# BLIS Walkthrough

**A discrete-event simulator for LLM inference serving systems**

<div class="abs-br m-6 flex gap-2">
  <a href="https://github.com/inference-sim/inference-sim" target="_blank" class="text-xl slidev-icon-btn">
    <carbon-logo-github />
  </a>
  <a href="https://inference-sim.github.io/inference-sim/latest/" target="_blank" class="text-xl slidev-icon-btn">
    <carbon-document />
  </a>
</div>

---

# 1. The "Why" Behind BLIS

Most inference tools answer *"how fast is one forward pass?"* — BLIS answers *"how does a cluster behave under realistic traffic?"*

|  | **BLIS** | **Vidur** | **LLMServingSim 2.0** |
|---|---|---|---|
| **Scope** | Cluster: N instances, shared-clock DES | Single-instance profiling; multi-GPU TP/PP within one instance | Multi-instance with P/D disaggregation |
| **Latency model** | 4 pluggable backends (analytical → data-driven) | Random forest from GPU profiling data | scikit-learn attention predictor + profiling |
| **Routing** | Composable weighted scoring (llm-d Endpoint Picker parity) | Not modeled | Round-robin, random, custom |
| **Admission control** | Token-bucket rate limiting, pluggable | Not modeled | Not modeled |
| **KV cache** | Block-level with prefix caching + tiered GPU/CPU offload | Not detailed in docs | RadixAttention prefix caching, CXL expansion |
| **Workloads** | YAML DSL + real-server trace capture + replay | Poisson synthetic + Azure CSV trace replay | Configurable request generation |
| **Sim2Real** | Observe → Replay → Calibrate pipeline | Profiling-based validation (<9% error) | Profiling-based |
| **Determinism** | Byte-identical output (partitioned RNG, INV-6) | Seed-based | Seed-based |

**Gaps that motivated BLIS:** No existing simulator modeled the full cluster control plane — admission × routing × scheduling × KV cache as interacting subsystems. Policy interactions produce non-obvious emergent behavior (super-additivity, signal cancellation, regime-dependent dominance) that single-instance simulators cannot observe.

<div class="abs-br m-4 text-xs opacity-60">

[Docs](https://inference-sim.github.io/inference-sim/latest/) · [Architecture](https://inference-sim.github.io/inference-sim/latest/concepts/architecture/)

</div>

---

# 2. Performance & Validation

### Sim2Real pipeline: Observe → Replay → Calibrate

```bash
blis observe --server-url http://localhost:8000 --model qwen/qwen3-14b \   # record real latencies
  --workload-spec workload.yaml --trace-header trace.yaml --trace-data trace.csv
blis replay  --trace-header trace.yaml --trace-data trace.csv \             # replay through DES
  --model qwen/qwen3-14b --results-path results.json
blis calibrate --trace-header trace.yaml --trace-data trace.csv \           # compare real vs sim
  --sim-results results.json --report calibration.json
```

- **Observe** dispatches workload to real vLLM/TGI/SGLang servers (streaming SSE + chat completions), records per-request TTFT/E2E/token counts as TraceV2
- **Replay** runs the exact same trace through the DES — same arrival times, same token counts
- **Calibrate** produces per-metric MAPE, Pearson R, percentile comparison, and quality rating (excellent <10% MAPE / good <20% / fair <35%)
- Network RTT correction and warmup exclusion auto-read from the trace header

### Strategy Evolution: 30 iterations, 1000+ experiments

Systematic hypothesis-driven optimization across routing and scheduling — produced 14 routing principles and 16 scheduling principles. Key finding: `PA:3,QD:2` is staleness-immune and performs identically across all snapshot refresh intervals and KV pressure levels.

<div class="abs-br m-4 text-xs opacity-60">

[Sim2Real Guide](https://inference-sim.github.io/inference-sim/latest/guide/observe-replay-calibrate/) · [Strategy Evolution](https://inference-sim.github.io/inference-sim/latest/methodology/strategy-evolution/)

</div>

---

# 3. Integration & Real-World Environments

### What BLIS already models from llm-d

- **Weighted scoring router** with prefix-affinity, queue-depth, KV-utilization scorers — default `PA:3, QD:2, KV:2` matches llm-d Endpoint Picker
- **Example EPP configs** ship in-repo: `epp-estimate-prefix.yaml` and `epp-precise-prefix.yaml` map 1:1 to llm-d EPP configurations
- **PD disaggregation** landed — prefill/decode instance pools with KV transfer modeling ([PR #804](https://github.com/inference-sim/inference-sim/pull/804), [PR #805](https://github.com/inference-sim/inference-sim/pull/805))
- **`blis observe`** integrates with real servers via OpenAI-compatible completions and chat APIs

### As an instance-simulator inside a live llm-d cluster?

BLIS is an **offline, CPU-only DES** — not a real-time sidecar. Key gaps for live integration:

| Gap | What it means |
|-----|---------------|
| No real-time event loop | DES advances simulated clock as fast as CPU allows, not wall-clock aligned |
| No serving interface | CLI tool — no gRPC/HTTP server to receive routed requests |
| Simulated KV blocks | In-memory block accounting, not real GPU memory management |
| Step-level batching | Discrete batch steps vs. vLLM's iteration-level continuous batching |

**However:** BLIS's policy implementations (scoring, admission, routing) can inform llm-d policy design via simulation-validated parameters — test configurations offline before deploying.

<div class="abs-br m-4 text-xs opacity-60">

[Architecture](https://inference-sim.github.io/inference-sim/latest/concepts/architecture/) · [PR #804](https://github.com/inference-sim/inference-sim/pull/804)

</div>

---

# 4. Latency Models

Four pluggable backends — from zero-config analytical to data-driven:

| Mode | Approach | GPU Step Accuracy | MoE | Data needed |
|------|----------|-------------------|-----|-------------|
| **Roofline** (default) | Analytical FLOPs/bandwidth | Good | No | HF `config.json` + `--hardware` |
| **Blackbox** | Per-model α/β regression | Highest (per-model) | If trained | Coefficients in `defaults.yaml` |
| **Cross-Model** | Physics-informed features | Good (7 global params) | Yes (binary) | HF `config.json` + `--hardware` |
| **Trained-Roofline** | Roofline × learned corrections | **7% MAPE** (10 params) | Yes | HF `config.json` + `--hardware` |

### The blackbox 6-parameter model

```
GPU step:  StepTime = β₀ + β₁ × cache_miss_tokens + β₂ × decode_tokens
CPU side:  QueueingTime = α₀ + α₁ × input_length;   OutputProcessing = α₂
```

### Is this high-fidelity? Where can it improve?

- **GPU step time**: Yes — trained-roofline achieves 7% MAPE across 137K real vLLM requests (4 architectures, 7B–70B)
- **CPU overhead (α₀)**: Weak — 93% MAPE because it's a single constant for a highly variable quantity. Per-deployment calibration recommended for TTFT-sensitive analysis
- **Chunked prefill**: Coefficients fitted on single-step prefill data; overestimates early-chunk step times
- **DP/EP scheduling overhead**: Not yet modeled (TP only)

<div class="abs-br m-4 text-xs opacity-60">

[Latency Models Guide](https://inference-sim.github.io/inference-sim/latest/guide/latency-models/)

</div>

---

# 5. Workloads

### Can you feed recorded workloads? Yes — three paths:

| Path | How | Result |
|------|-----|--------|
| `blis observe` | Dispatch to real server, record timing | TraceV2 (YAML header + CSV data) |
| `blis replay` | Feed TraceV2 into DES | Exact arrival times and token counts replayed |
| `blis run --trace-output` | Export any synthetic run | TraceV2 for later replay or sharing |

### Beyond fixed distributions — workload-spec YAML DSL

```yaml
clients:
  - id: "chat-user"
    rate_fraction: 0.7
    slo_class: "critical"                    # 5 SLO classes: critical/standard/sheddable/batch/background
    prefix_group: "system-prompt"            # shared prefix → prefix cache reuse
    prefix_length: 512
    arrival: { process: poisson }            # also: gamma (bursty), weibull, constant
    input_distribution: { type: gaussian, params: { mean: 256, std_dev: 128, min: 2, max: 4096 } }
    output_distribution: { type: exponential, params: { mean: 128 } }
  - id: "reasoning-session"
    rate_fraction: 0.3
    slo_class: "standard"
    reasoning:                               # multi-turn with context accumulation
      multi_turn: { max_rounds: 4, think_time_us: 5000000, context_growth: accumulate }
```

Compose multiple specs: `blis compose --from chat.yaml --from batch.yaml`. Convert from ServeGen or inference-perf: `blis convert servegen --path data/`.

<div class="abs-br m-4 text-xs opacity-60">

[Workloads Guide](https://inference-sim.github.io/inference-sim/latest/guide/workloads/) · [Spec Schema](https://inference-sim.github.io/inference-sim/latest/reference/workload-spec/)

</div>

---

# 6. Extensibility

### Four extension types — each with a step-by-step recipe

| Type | Example | Touch points |
|------|---------|-------------|
| **Policy template** | New routing algorithm, admission policy, scheduler | 2 files: implement interface + register in `bundle.go` factory |
| **Scorer** | New dimension for weighted routing (e.g., predicted-latency) | 2 files: `scorerFunc` + register in `routing_scorers.go` |
| **Subsystem module** | New module with its own interface (e.g., autoscaler) | 3–5 files: interface + event wiring + config |
| **Backend swap** | New latency model backend | 2 files: implement `LatencyModel` interface + register |

### Recommended methodology

1. Read the [Design Guidelines](https://inference-sim.github.io/inference-sim/latest/contributing/templates/design-guidelines/) — DES foundations, module contracts, extension friction
2. Follow BDD/TDD: write behavioral contracts (Gherkin-style) → tests → implementation
3. Use [Extension Recipes](https://inference-sim.github.io/inference-sim/latest/contributing/extension-recipes/) for exact file lists and registration steps
4. Check [Antipattern Rules R1–R23](https://inference-sim.github.io/inference-sim/latest/contributing/standards/rules/) — each traces to a real bug
5. Maintain [11 system invariants](https://inference-sim.github.io/inference-sim/latest/contributing/standards/invariants/) (request conservation, clock monotonicity, causality, determinism, etc.)

Every policy axis is a single-method Go interface — add a new variant without touching existing implementations.

<div class="abs-br m-4 text-xs opacity-60">

[Extension Recipes](https://inference-sim.github.io/inference-sim/latest/contributing/extension-recipes/) · [Design Guidelines](https://inference-sim.github.io/inference-sim/latest/contributing/templates/design-guidelines/)

</div>

---
layout: center
class: text-center
---

# Links & Resources

|  |  |
|---|---|
| GitHub | [github.com/inference-sim/inference-sim](https://github.com/inference-sim/inference-sim) |
| Docs | [inference-sim.github.io/inference-sim/latest](https://inference-sim.github.io/inference-sim/latest/) |
| Architecture | [Cluster Architecture](https://inference-sim.github.io/inference-sim/latest/concepts/architecture/) |
| Latency Models | [Latency Models Guide](https://inference-sim.github.io/inference-sim/latest/guide/latency-models/) |
| Workloads | [Workload Specifications](https://inference-sim.github.io/inference-sim/latest/guide/workloads/) |
| Sim2Real | [Observe / Replay / Calibrate](https://inference-sim.github.io/inference-sim/latest/guide/observe-replay-calibrate/) |
| Extension Recipes | [How to Extend BLIS](https://inference-sim.github.io/inference-sim/latest/contributing/extension-recipes/) |
| Design Guidelines | [DES Foundations & Module Contracts](https://inference-sim.github.io/inference-sim/latest/contributing/templates/design-guidelines/) |
| Strategy Evolution | [Research Methodology](https://inference-sim.github.io/inference-sim/latest/methodology/strategy-evolution/) |
| PD Disaggregation | [PR #804](https://github.com/inference-sim/inference-sim/pull/804), [PR #805](https://github.com/inference-sim/inference-sim/pull/805) |
