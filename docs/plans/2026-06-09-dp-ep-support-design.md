# Proposal: First-class Data Parallelism (DP) and Expert Parallelism (EP) in the latency model

> **Format note.** This document is written to be posted as a GitHub **Discussion**
> in `inference-sim/inference-sim`, then split into one **tracking issue** plus a
> set of **sub-issues** (see [§12 Work breakdown](#12-work-breakdown-tracking-issue--sub-issues)).
> It is intentionally self-contained: a newcomer (human or AI agent) should be
> able to read it top-to-bottom and start work without external context. Code
> sketches are *illustrative* — final signatures are decided in the sub-issues.

---

> **Naming.** *BLIS* is the name of this repository's trained-physics latency
> model (the `sim/latency` package in `inference-sim`). "BLIS" and "the latency
> model" are used interchangeably below.
>
> **`EP = TP × DP` convention.** Throughout this document `EP = TP × DP`. vLLM
> actually flattens *three* axes — `EP = TP × DP × PCP` (PCP = "pipeline/context
> parallel"; see [§4](#4-verified-facts-from-vllm)) — but PCP = 1 in every
> deployment this model targets, so we write `EP = TP × DP` everywhere and treat
> the PCP factor as a documented follow-on. If you ever set PCP > 1, every `EP`
> below becomes `TP × DP × PCP`.

## 1. TL;DR

BLIS's trained-physics latency model only understands **tensor parallelism (TP)**.
It divides every cost by the TP degree and has no concept of **data parallelism
(DP)** or **expert parallelism (EP)**. But the way vLLM actually serves
Mixture-of-Experts (MoE) models — DeepSeek, Qwen-MoE, Mixtral, Llama-4 — is
**DP-attention + EP-MoE**, where the expert-parallel degree is `EP = TP × DP`.

As a result, for any MoE deployment with `DP > 1`, BLIS today:

1. **Over-estimates expert weight load by a factor of `DP`** (experts are spread
   over `TP×DP` GPUs, but the model divides by `TP`).
2. **Ignores the MoE all-to-all communication entirely** (the dominant
   inter-GPU cost in EP).
3. **Ignores shared experts** (always-active FFN in DeepSeek / Qwen-MoE).

This proposal adds DP and EP to the latency model and its calibration. The core
idea: model the DP+EP deployment as **one logical engine over a single global
batch**, where `(TP, DP)` only change the per-term divisors and add an
all-to-all term — no new scheduler machinery. Expert placement (which decides
per-GPU load) goes **behind an interface** so we can later model skewed routing
and EPLB-style rebalancing. Every claim below is verified against vLLM source
(citations in [§4](#4-verified-facts-from-vllm)).

---

## 2. Audience primer: TP vs DP vs EP

If you already know these, skip to [§3](#3-the-problem).

A large model is served across many GPUs. There are three independent ways to
split the work:

- **Tensor Parallelism (TP).** Each *layer's weight matrices* are sliced across
  TP GPUs. Every GPU holds a slice of every layer and processes *every* token,
  but only computes its slice. GPUs must communicate (an **all-reduce**) within
  each layer to recombine partial results. TP shrinks per-GPU compute, memory,
  and KV cache by the TP degree — at the cost of all-reduce traffic.

- **Data Parallelism (DP).** The model is *replicated*; each DP replica handles a
  *different subset of requests*. Classic DP needs no inter-replica
  communication. In vLLM's MoE deployments, DP is used for the **attention**
  part: each DP rank owns a disjoint set of sequences (and their KV cache).

- **Expert Parallelism (EP).** Specific to MoE. An MoE layer has `N` experts
  (independent FFNs); each token is routed to its top-`k` experts. With EP, the
  `N` experts are *distributed* across GPUs — each GPU stores only `N/EP`
  experts. Because a token's chosen experts may live on other GPUs, tokens are
  shuffled across GPUs by an **all-to-all** (a "dispatch"), processed, then
  shuffled back (a "combine").

**The key fact about vLLM MoE:** attention runs data-parallel (DP) while the MoE
layer runs expert-parallel over *all* the GPUs, so:

```
EP = TP × DP
```

A token's journey through one MoE transformer layer on a `TP=2, DP=4` (8-GPU)
deployment:

```
          DP group 0          DP group 1     ...    DP group 3
        ┌───────────┐       ┌───────────┐         ┌───────────┐
seqs ──▶│ attention │  ...  │ attention │   ...   │ attention │   (DP: each group
        │  TP=2     │       │  TP=2     │         │  TP=2     │    a disjoint batch;
        └─────┬─────┘       └─────┬─────┘         └─────┬─────┘    TP=2 within group)
              │                   │                     │
              └─────────── all-to-all dispatch ─────────┘          (EP=8: every GPU
                          (route tokens to             )            holds N/8 experts)
              ┌───────────────────┴───────────────────┐
              │   each GPU computes its local experts  │
              └───────────────────┬───────────────────┘
              ┌─────────── all-to-all combine ─────────┐
              │      (return expert outputs to owner)   │
```

---

## 2.5. Symbol table (verified against `trained_physics_model.go`)

Every symbol used in the formulas below, with its name in the code and its
meaning. The β coefficients are the fitted *correction* coefficients; the code
stores them in a single slice `m.Beta[...]` (0-indexed, so `β₄` is `m.Beta[3]`).
This table is verified against the `StepTime` docstring and body
(`sim/latency/trained_physics_model.go`).

**Model / batch quantities**

| Symbol | Code | Meaning |
|--------|------|---------|
| `L` | `m.numLayers` | number of transformer layers |
| `nMoE` | `m.numMoELayers` | number of MoE layers (0 for dense) |
| `B` | batch size | number of requests in the step |
| `d` | `m.hiddenDim` | hidden dimension |
| `dH` | `m.headDim` (`d/numHeads`) | per-head dimension |
| `dKV` | `m.dKV` (`kvHeads·dH`) | KV width (differs from `d` under GQA) |
| `dFFMoE` | `MoEExpertFFNDim` | per-expert FFN intermediate dim |
| `numExperts` (`N`) | `m.numExperts` | total routed experts in an MoE layer |
| `kEff` (top-k) | `m.kEff` | experts each token is routed to (`num_experts_per_tok`) |
| `nEff` | computed `:245-249` | *effective* experts loaded per step = `min(N, max(kEff, B·kEff))` |
| `bpp` | `m.weightBPP` | bytes per weight parameter (e.g. 2 for BF16) |
| `tp` | `m.tp` | tensor-parallel degree (the only parallelism field today) |

**Hardware / rate quantities**

| Symbol | Code | Meaning |
|--------|------|---------|
| `flopsPeakUs` | `m.flopsPeakUs` | peak compute rate (FLOPs per µs) |
| `bwHbmUs` | `m.bwHbmUs` | HBM bandwidth (bytes per µs) |
| `bwComm` | (new) | inter-GPU comm bandwidth for the all-to-all term ([§7](#7-the-all-to-all-term)) |

**Fitted β coefficients** (dimensionless corrections unless noted; from the
`StepTime` docstring):

| Symbol | Code | Meaning |
|--------|------|---------|
| `β₁` | `Beta[0]` | prefill-compute correction (~1.0) |
| `β₂` | `Beta[1]` | decode-compute correction (~0 when memory-bound) |
| `β₃` | `Beta[2]` | weight-loading correction (~1.0–1.5) |
| `β₄` | `Beta[3]` | **TP all-reduce** correction (~0.3–0.8) — absorbs comm/HBM ratio |
| `β₅` | `Beta[4]` | per-layer overhead (µs/layer) |
| `β₆` | `Beta[5]` | per-request overhead (µs/request) |
| `β₇` | `Beta[6]` | per-step constant overhead (µs/step) |
| `β₈` | `Beta[7]` | **MoE-layer overhead**, architecture-aware: non-zero only for *interleaved* MoE, zero for uniform MoE and dense ([§3](#3-the-problem)) |
| `β_EP` | (new) | **all-to-all** efficiency, sibling of `β₄` ([§7](#7-the-all-to-all-term)) |

> **Note on `{c, β_C, β_M}`.** Some calibration discussions refer to a "reduced
> step head" written `{c, β_C, β_M}`. That is calibration-side shorthand, **not**
> names that appear in the code — the code's coefficients are `β₁…β₈` above. When
> [§11](#11-calibration)/[#E](#sub-issue-e--calibration-parallelism-sweep-corner--β_ep-fitting)
> say "keep the reduced head," they mean: keep whichever subset of `β₁…β₈` the
> current calibration already fits, and *add* `β_EP` as a new sibling of `β₄`. No
> existing coefficient is renamed.

---

## 3. The problem

BLIS's latency model (`sim/latency/trained_physics_model.go`,
function `StepTime`) carries one parallelism field, `tp` (set from
`sim/config.go` `ModelHardwareConfig.TP`). Every compute, KV, and weight term is
divided by `tp`. A repo-wide search finds **no** `DP`, `EP`, `DataParallel`, or
`ExpertParallel` anywhere in `sim/` or `cmd/`.

This is correct for **dense, TP-only** serving. It is wrong for **MoE under
DP+EP** in three concrete ways:

| # | Bug | Where | Effect |
|---|-----|-------|--------|
| B1 | Expert weights divided by `tp`, not `EP=tp·DP` | MoE weight term in `StepTime` | per-GPU MoE weight load too high by factor `DP` (and weight load dominates MoE decode) |
| B2 | No all-to-all term | (absent) | the dominant EP inter-GPU cost is modeled as zero for uniform MoE |
| B3 | No shared-expert term | (absent) | DeepSeek/Qwen shared FFN (runs every token, every step) is missing |

A note on B2: a code comment in `StepTime` claims the all-to-all is "captured by
β₈". It is not. `β₈` is a *per-layer constant* that only activates for
**interleaved** architectures (Llama-4 Scout style) and is identically zero for
**uniform** MoE (Mixtral, DeepSeek, Qwen). All-to-all volume scales with tokens
and EP degree; a constant cannot represent it. `β₈` is kept, but re-scoped to its
real meaning: interleave kernel-switch overhead.

---

## 4. Verified facts (from vLLM)

Every modeling decision below rests on a fact checked against vLLM source
(`/Users/sri/Documents/Projects/vllm`). This table is the trust anchor — if you
doubt a divisor, read the cited file.

| Fact | Evidence (vLLM) |
|------|-----------------|
| `EP = TP × DP` (vLLM: `TP × DP × PCP`, and `PCP = 1` for us — see Naming note) | `vllm/model_executor/layers/fused_moe/config.py:907,1023` (`flatten_tp_size = dp_size·pcp_size·tp_size`, then `ep_size = tp_size`). The variable `tp_size` is *reassigned* to the flattened product `dp·pcp·tp` before `ep_size = tp_size`, so `EP` is that whole product. With `PCP = 1` it is exactly `TP × DP`. |
| Each DP rank owns a **disjoint** set of sequences and their KV cache | `vllm/forward_context.py:104-115` (per-rank token batch: `num_tokens_across_dp_cpu[dp_rank] == batchsize`) + `vllm/v1/engine/core.py:906-917` (each DP rank is its **own `EngineCore` process** — `set_process_title("EngineCore", f"DP{dp_rank}")` — with its own scheduler and KV cache). |
| Aggregate KV capacity scales with DP | follows from the above: `DP` independent engines, each with a full per-GPU KV budget → aggregate KV is `DP ×` a single rank's. |
| Attention weights are TP-sharded but **DP-replicated** | `vllm/model_executor/layers/linear.py:432,909` (QKV = ColumnParallel within each DP group) |
| Routed-expert weights sharded over EP; each GPU owns `N/EP` experts | `vllm/model_executor/models/llama4.py:126` (`n_local_physical_experts = n_physical_experts // ep_size`) |
| Non-EP fallback: expert weights **tensor**-sharded over `DP·TP` (not expert-distributed), uses all-reduce | `vllm/config/parallel.py:108`, `fused_moe/config.py:1005-1018` |
| MoE = **two** collectives per layer: all-to-all **dispatch** + all-to-all **combine** | `fused_moe/deepep_ht_prepare_finalize.py:143,348`; `fused_moe/pplx_prepare_finalize.py:217,325` |
| Dispatch/combine volume = `tokens × top_k × hidden` (each token sent to its `k` experts) | `fused_moe/all2all_utils.py:100-109`, `fused_moe/pplx_prepare_finalize.py:217-227` |
| Per-GPU expert compute ≈ `global_routed_tokens / EP` under balanced routing | `vllm/distributed/eplb/eplb_state.py:118-124` |
| Routing is **not** balanced in practice — vLLM ships EPLB + redundant experts | `vllm/distributed/eplb/eplb_state.py:78-116` |
| Before MoE, all DP ranks **pad to the max token count** across ranks; idle ranks run a dummy forward | `vllm/v1/worker/dp_utils.py:79-91`, `vllm/v1/engine/core.py:1366-1368` |
| **Shared experts** (DeepSeek/Qwen) run every token, every step, additively | `fused_moe/shared_fused_moe.py:59-96`, `fused_moe/layer.py:1843-1847` |
| Interleaved models treat dense vs MoE layers differently (dense FFN = TP+replicated, MoE = EP) | `vllm/model_executor/models/llama4.py:345-362` |

---

## 5. Core design decision

**Model the DP+EP deployment as ONE logical engine over a single global batch.**
`(TP, DP)` enter only (a) the per-term divisors in `StepTime` and (b) the
KV-capacity calculation. No per-DP-rank scheduler queues are introduced.

**Why this works.** Under balanced routing, every GPU does equal work, so the
step time of the whole engine equals the per-GPU work computed from the global
token population with physically-correct divisors. We do not need to simulate
each DP rank separately to get the step time right.

**Why not full per-rank lockstep (the rejected alternative).** Modeling each DP
rank's own queue, batch, and a synchronization barrier would capture load
imbalance and straggler effects, but at a large cost: per-rank scheduler state
and a sync event. We instead expose imbalance through a single `imbalanceFactor`
hook ([§9](#9-load-imbalance--the-saturation-assumption)) that is exact at
saturation (the regime calibration targets) and dial-able later.

**Scoping consequence.** DP for **dense** models stays what it already is —
independent replicas (`DeploymentConfig.NumInstances` + router). Engine-internal
DP+EP activates only for **MoE with expert parallelism enabled**, which is
exactly vLLM's DP-attention use case. (For a dense model, the global-batch
framing with divisor `/(tp·DP)` is mathematically identical to `DP` independent
replicas each serving `1/DP` of the load — so there is nothing new to build for
dense DP.)

---

## 6. The divisor map (the heart of correctness)

For each cost term, what does per-GPU work divide by? Let `EP = TP·DP`.

| Term | Divisor | Extra communication | Intuition |
|------|---------|---------------------|-----------|
| Attention compute (prefill & decode) | `/(tp·DP)` | — | sequences split across DP, heads split across TP |
| Attention KV read/write | `/(tp·DP)` | — | each DP rank holds `1/DP` of sequences |
| Attention weight load | `/tp` | — | weights **replicated** on every DP group |
| TP all-reduce (`β₄` term) | volume `/DP` | per DP group | each TP group all-reduces only its local tokens (~global/DP); DP groups run in parallel |
| Routed-expert compute | `/EP` | — | experts spread over all `tp·DP` GPUs |
| Routed-expert weight load | `/EP` | — | each GPU holds `N/EP` experts |
| Shared-expert compute | `/(tp·DP)` | — | always active; replicated/TP-sharded like dense FFN |
| Shared-expert weight load | `/tp` | — | replicated across DP |
| Dense-FFN compute (interleaved models) | `/(tp·DP)` | — | data + tensor parallel, like attention |
| Dense-FFN weight (interleaved models) | `/tp` | — | replicated across DP |
| All-to-all (`β_EP` term), **EP on** | — | dispatch + combine | volume in [§7](#7-the-all-to-all-term) |
| Expert comm, **EP off** (fallback) | weight `/(tp·DP)` | all-reduce | experts tensor-sharded; comm folds into the `β₄`-style term |

**The one asymmetry to remember:** attention / dense / shared *compute* divides by
`DP` (sequences are split), but their *weights* do **not** (weights are
replicated on every DP group). Only **routed-expert** weights divide by the full
`EP`. Today's code, lacking DP, conflates these and under-shards expert weights
by `DP` (bug B1).

**Why the TP all-reduce volume divides by `DP`.** A TP all-reduce recombines
partial results *within one TP group*. With `DP` groups, the global token
population is split `DP` ways, so each TP group only all-reduces its own
`~global/DP` tokens, and the `DP` groups run that collective **in parallel** (no
cross-group traffic). So the per-group all-reduce volume — and thus the time, on
the critical path — is `1/DP` of what a single group handling all tokens would
move. This is *on top of* the existing `(tp-1)/tp` factor (the fraction of each
group's data that actually crosses GPUs), which is unchanged.

**Implementation caveat (read before coding §C).** In `StepTime`, attention
compute is *not* written as `…/tp`; it is sharded through `hPerGPU = numHeads/tp`.
TP shards heads, but **DP does not** — so the DP factor on attention compute must
come from the per-rank *sequence/token* split, not from scaling `hPerGPU` by `DP`.
Likewise the routed-expert terms use a batch-dependent `nEff = min(numExperts,
max(kEff, B·kEff))`, not a plain `/tp`, so EP changes the *expert-count ceiling*,
not just a divisor. The divisors in the table above are physically correct; the
code paths that realize them are term-specific (see [#C](#sub-issue-c--steptime-divisor-refactor--shared-expert--all-to-all)).

---

## 6.5. A fully worked example (`TP=2, DP=2`, uniform MoE)

To make the divisor map concrete — and to give [#C](#sub-issue-c--steptime-divisor-refactor--shared-expert--all-to-all)'s
"hand-computed step-time check" a worked reference — here is one MoE layer on an
8-GPU `TP=2, DP=2` deployment (`EP = TP·DP = 4`). Take a global step batch of
`B = 100` requests, all decoding, with `globalTokens = 100` decode tokens (one
per request), a uniform MoE with `N = 8` experts, top-`k` `kEff = 2`, hidden
`d = 4096`, per-expert FFN `dFFMoE = 14336`, `bpp = 2` (BF16).

**Routed-expert weight bytes per GPU** (the term with bug B1 today):

```
correct (EP=4):  nMoE · (numExperts/EP) · 3 · d · dFFMoE · bpp
                 = nMoE · (8/4)          · 3 · 4096 · 14336 · 2   per GPU
today (÷tp=2):   nMoE · nEff             · 3 · d · dFFMoE · bpp / 2
```

Under EP each GPU holds `N/EP = 2` experts. Today's code divides the
`nEff`-based term by `tp = 2` only, so it loads weights as if each GPU held
`nEff/2` experts — for this batch `nEff = min(8, max(2, 100·2)) = 8`, i.e. it
charges **8 experts ÷ 2 = 4 experts' worth** of weight per GPU instead of the
correct **2**. That is the factor-`DP` (= 2×) over-estimate of bug B1.

**Routed-expert compute tokens per GPU:**

```
PerGPUComputeTokens = globalTokens · kEff / EP = 100 · 2 / 4 = 50 token-expert pairs
```

**All-to-all volume** ([§7](#7-the-all-to-all-term)), `imbalanceFactor = 1`:

```
allToAllBytes = globalTokens · kEff · d · bpp · (EP-1)/EP · 2
              = 100 · 2 · 4096 · 2 · (3/4) · 2
              = 2,457,600 bytes  per MoE layer
tEP           = (2,457,600 / bwComm) · nMoE
```

**Attention KV read per GPU** (decode), currently `dKV/tp`, now `/(tp·DP)`:
each of the 4 DP+TP shards reads `1/(tp·DP) = 1/4` of the global KV traffic.

**Attention weights per GPU:** `/tp = /2` only — replicated across the 2 DP
groups, so DP does **not** divide them.

Plugging these per-GPU quantities into the `StepTime` formula
([§2.5](#25-symbol-table-verified-against-trained_physics_modelgo)) and summing
gives the engine step time. The golden test in [#C](#sub-issue-c--steptime-divisor-refactor--shared-expert--all-to-all)
should encode exactly one such worked arithmetic example so the numbers are
checkable by hand.

---

## 7. The all-to-all term

Per MoE layer, dispatch and combine each move `tokens × top_k × hidden` elements
(verified). Modeled cost:

```
allToAllBytes = globalTokens · kEff · hidden · bytesPerCommElem · (EP-1)/EP · 2 · imbalanceFactor
tEP           = (allToAllBytes / bwComm) · numMoELayers
```

- `kEff` = `num_experts_per_tok` (top-k). `× 2` = dispatch + combine.
- `(EP-1)/EP` = fraction of data that actually leaves a GPU (mirrors the existing
  TP all-reduce factor `(tp-1)/tp`).
- `bwComm` = NVLink bandwidth when `EP ≤ gpusPerNode`, inter-node fabric
  otherwise. **Initially**, `β_EP` (a fitted coefficient) absorbs the comm/HBM
  bandwidth ratio, exactly as `β₄` does for TP all-reduce today; topology-aware
  `bwComm` is a follow-on.
- `imbalanceFactor` = 1.0 in the balanced model (see [§9](#9-load-imbalance--the-saturation-assumption)).

When EP is **off** (vLLM's non-EP fallback), there is no all-to-all; the expert
FFN is tensor-sharded and uses an all-reduce instead — so `tEP = 0` and the
expert communication folds into the existing `β₄`-style all-reduce term.

**Interaction with `β₈` (must re-fit, not re-label).** In today's code `β₈`
(`Beta[7]`) is gated on `hasInterleavedMoE` and is **identically zero for uniform
MoE** (Mixtral/DeepSeek/Qwen) — so for those models a new `tEP` term is the *only*
all-to-all contribution and there is no conflict. But for **interleaved** models
(Llama-4 Scout) `β₈` is currently *non-zero and absorbing the all-to-all cost*
(its comment literally reads "captured by β₈"). Adding `tEP` on top of the
**existing fitted `β₈` value** would double-count. Re-scoping `β₈` to "interleave
kernel-switch overhead" is therefore **not a relabel of the old number** — `β₈`
must be **re-fit from scratch in the same calibration pass that fits `β_EP`**
([§11](#11-calibration)/[#E](#sub-issue-e--calibration-parallelism-sweep-corner--β_ep-fitting)),
on a dataset where `tEP` is already carrying the all-to-all. The old `β₈` value
is discarded.

---

## 8. Expert placement behind an interface

Per-GPU MoE load is **not** hard-coded to the balanced formula. vLLM ships a
whole EPLB subsystem with redundant experts because real routing is skewed, so
balanced placement must be *one swappable strategy*. This is the seam for future
work on expert distribution, replication, and rebalancing.

Illustrative contract (final shape decided in the sub-issue):

```go
// ExpertPlacement maps a step's routed-token population onto per-GPU MoE cost,
// returning the load of the BUSIEST GPU — a collective runs at the speed of its
// slowest participant. Future strategies model skewed routing, redundant /
// replicated experts (EPLB), and DP token padding.
type ExpertPlacement interface {
    Resolve(globalTokens float64, kEff, numExperts, ep int) ExpertLoad
}

type ExpertLoad struct {
    PerGPUComputeTokens float64 // tokens·activations the max-loaded GPU computes
    PerGPUExpertCount   float64 // resident experts per GPU → weight bytes
    AllToAllTokens      float64 // dispatch+combine volume (token·top_k measure)
}
```

`BalancedPlacement` (first implementation):

```
PerGPUComputeTokens = globalTokens · kEff / EP
PerGPUExpertCount   = numExperts / EP
AllToAllTokens      = globalTokens · kEff · (EP-1)/EP · 2
```

Because the contract returns the *busiest* GPU, the "step = max over GPUs"
physics emerges automatically once a later strategy introduces imbalance.

---

## 9. Load imbalance & the saturation assumption

Two real effects make the engine slower than the perfectly-balanced ideal, and
they are the **same physics** — a collective runs at the speed of its heaviest
participant:

- **DP token padding** (verified): before the MoE all-to-all, every DP rank pads
  to the *maximum* token count across ranks. So MoE + all-to-all cost is sized by
  `DP × max_rank_tokens`, not `Σ rank_tokens`.
- **Expert skew** (EPLB): hot experts make some expert-GPUs heavier than others.

`BalancedPlacement` sets `imbalanceFactor = 1.0`, i.e. `max = mean`. This is:

- **Exact at saturation** — all DP ranks uniformly full, which is precisely the
  operating point the μ* (capacity) calibration targets.
- **Optimistic at low / bursty load** — ranks diverge and the padding penalty is
  unmodeled.

This is a deliberate, documented limitation. `imbalanceFactor (≥ 1)` is the
single hook through which both effects can later be modeled, without per-rank
queues.

---

## 10. Configuration surface

- **`ModelHardwareConfig`** (`sim/config.go`): add `DP int` (default 1) and
  `EnableExpertParallel bool` (mirrors vLLM `--enable-expert-parallel`). Derive
  `EP = TP·DP` when EP is enabled.
- **`ModelConfig`** (`sim/model_hardware_config.go`): a shared-expert FFN-size
  field **already exists** — `SharedExpertFFNDim` (`:17`, JSON
  `shared_expert_intermediate_size`, 0 = none) — and is already consumed by the
  KV-capacity weight path (`kv_capacity.go` `computeModelWeightBytes`). The gap is
  that **`StepTime` ignores it**. So the work here is to *use* the existing field
  in the latency path, not to add config plumbing. (Optionally add an explicit
  `NumSharedExperts` count if a model expresses shared size as
  `moe_intermediate_size × n_shared_experts` rather than a direct dim; check the
  target `config.json` first — `SharedExpertFFNDim` already holds the total dim.)
- **`DeploymentConfig`**: no new pool concept — a logical DP+EP engine is one
  instance whose latency model knows `(TP, DP, EP)`.
- **`HardwareCalib`**: source `gpusPerNode` and the comm bandwidth(s) for `bwComm`.

**Backward-compatibility invariant (INV: BC-DP1).** With `DP = 1` and EP
disabled, every term reduces to its current value (`tp·DP = tp`, `EP = tp`,
`tEP = 0`, no shared experts for existing configs). Existing simulations must be
**byte-identical** — enforced by a golden test.

---

## 11. Calibration

Keep the existing fitted coefficients `β₁…β₈` (the calibration "reduced head" —
see the note in [§2.5](#25-symbol-table-verified-against-trained_physics_modelgo))
and add **`β_EP`** (all-to-all efficiency) as a sibling of the existing `β₄` (TP
all-reduce, `Beta[3]`). Multi-GPU DP+EP hardware is available, so `β_EP` is fit
**empirically** and validated against measured capacity μ*.

**New calibration corner — "parallelism sweep":** fixed MoE workload, sweep
`(TP, DP) ∈ {(1,1), (2,1), (1,2), (2,2), (4,1), (1,4), (2,4)}`.

- `(TP>1, DP=1)` isolates `β₄` (pure all-reduce, no all-to-all).
- `(TP=1, DP>1)` isolates `β_EP` (pure all-to-all, no all-reduce).
- Mixed points cross-check; a joint non-negative least squares (NNLS) solve
  recovers both coefficients.
- **Run at saturation** so `imbalanceFactor = 1` holds during fitting.
- Sweep **three model classes** so all terms are exercised:
  - a pure-routed MoE (**Mixtral**),
  - a shared-expert MoE (**DeepSeek** or **Qwen-MoE**) → exercises the shared term,
  - an interleaved MoE (**Llama-4 Scout**) → keeps `β₈` covered.

**Validation gate.** Simulated μ* ≈ measured μ* at ≥ 2 `(TP, DP)` operating
points per model class.

---

## 12. Work breakdown (tracking issue + sub-issues)

> Copy [§1–§11](#1-tldr) into a **Discussion**. Create one **tracking issue**
> with the checklist below; create each **sub-issue** with the linked spec.
> Sub-issues are ordered; dependencies are stated. Each is independently
> reviewable and testable.

### Tracking issue: "Add DP + EP support to the latency model"

- [ ] **#A — Config plumbing for DP/EP/shared-experts** (no behavior change)
- [ ] **#B — `ExpertPlacement` interface + `BalancedPlacement`**
- [ ] **#C — `StepTime` divisor refactor + shared-expert + all-to-all terms**
- [ ] **#D — KV-capacity DP scaling**
- [ ] **#E — Calibration: parallelism-sweep corner + `β_EP` fitting**
- [ ] **#F — End-to-end μ\* validation on real MoE models**

Dependency graph: `#A → #B → #C → #D`; `#C → #E → #F`.

---

### Sub-issue #A — Config plumbing for DP/EP/shared experts

**Goal.** Add the inputs the model needs; change nothing about behavior yet.

**Scope.**
- Add `DP int` (default 1) and `EnableExpertParallel bool` to `ModelHardwareConfig`
  (`sim/config.go`); update the canonical constructor `NewModelHardwareConfig`.
- Derive `EP = TP·DP` at construction (helper or stored field).
- Shared-expert size is **already** in `ModelConfig` as `SharedExpertFFNDim`
  (`sim/model_hardware_config.go:17`); no new field is needed unless a target
  model expresses shared size only as a per-expert count (then add
  `NumSharedExperts` and derive the dim). Verify the relevant `config.json`
  populates `SharedExpertFFNDim` correctly.
- Surface `gpusPerNode` + comm bandwidth via `HardwareCalib`.

**Acceptance criteria.**
- New fields default such that existing configs are unchanged.
- DeepSeek/Qwen `config.json` parses the shared-expert dim into the existing
  `SharedExpertFFNDim` (unit test with a real config fixture).
- `EP` derivation unit-tested for several `(TP, DP)` pairs.

**Depends on:** none. **Risk:** low (additive).

---

### Sub-issue #B — `ExpertPlacement` interface + `BalancedPlacement`

**Goal.** Introduce the seam that decides per-GPU MoE load, with a balanced
default.

**Scope.**
- Define the `ExpertPlacement` interface and `ExpertLoad` result ([§8](#8-expert-placement-behind-an-interface)).
- Implement `BalancedPlacement` with the three formulas in [§8](#8-expert-placement-behind-an-interface).
- Wire a placement strategy into model construction (default = balanced).

**Acceptance criteria.**
- Unit tests assert `BalancedPlacement.Resolve` returns hand-computed
  `PerGPUComputeTokens`, `PerGPUExpertCount`, `AllToAllTokens` for several
  `(globalTokens, kEff, numExperts, ep)` tuples, including `ep=1` (degenerate).
- Interface documented with the "busiest GPU" contract.

**Depends on:** #A. **Risk:** low.

---

### Sub-issue #C — `StepTime` divisor refactor + shared-expert + all-to-all

**Goal.** Make the step-time formula physically correct for DP+EP.

**Scope (apply the divisor map in [§6](#6-the-divisor-map-the-heart-of-correctness)).**
This is **not** a blanket `/tp → /(tp·DP)` find-replace — the DP factor enters
different terms through different mechanisms in `StepTime`
(`sim/latency/trained_physics_model.go`). Touch each term deliberately:

- **Attention compute** (`prefillAttnFlops` `:181`, decode `flopsAttn` `:222`) is
  *not* divided by a literal `/tp` — it is sharded through
  `hPerGPU = numHeads/tp` (`:173`). DP does **not** shard heads (DP replicates the
  model), so **do not divide attention by `hPerGPU·DP`**. The DP effect on
  attention is that each rank processes `1/DP` of the *sequences*; since the
  step-time model already works from the global token population, the per-rank
  sequence split is captured by feeding the term per-rank token counts, not by
  scaling `hPerGPU`. Decide explicitly in this sub-issue whether attention
  compute/KV scale with DP via the *token-count* path; do not reach for `hPerGPU`.
- **Attention KV read/write** (`bytesPfKv` `:213`, `bytesDcKv` `:238`, currently
  `dKV/tp`): each DP rank holds `1/DP` of the sequences → `/(tp·DP)`.
- **Attention weights** (`bytesAttn` `:251`, `/tp`): **unchanged** — replicated on
  every DP group.
- **Routed-expert terms** (compute `:201`/`:226` use `kEff`; weight `:256` uses
  `nEff = min(numExperts, max(kEff, B·kEff))` `:245-249`): replace the **whole**
  `nEff`/`kEff` expressions with `ExpertPlacement` outputs — not a divisor tweak.
  Under EP the `min(numExperts, …)` ceiling itself becomes `numExperts/EP`
  (each GPU holds only `N/EP` experts), so dividing the existing `nEff` by `EP`
  is **wrong**; `BalancedPlacement` must compute the per-GPU expert count and
  token load directly ([§8](#8-expert-placement-behind-an-interface)).
- **Dense-FFN compute** (interleaved, `:204`/`:229`, currently `/tp`): `/(tp·DP)`.
  Dense-FFN **weight** (`:259`, `/tp`): unchanged (replicated).
- **Shared-expert** compute (`/(tp·DP)`) and weight (`/tp`) when shared experts are
  present — wire the **existing** `SharedExpertFFNDim` field
  (`sim/model_hardware_config.go:17`, already parsed and already used by
  `kv_capacity.go`) into `StepTime`, which currently ignores it.
- **TP all-reduce** (`tTp` `:278-284`): divide the volume by `DP` — each TP group
  all-reduces only its local `~global/DP` tokens; keep the `(tp-1)/tp` factor.
- Add the `tEP` all-to-all term ([§7](#7-the-all-to-all-term)) gated on
  `EnableExpertParallel && isMoE`, scaled by `numMoELayers`. When EP is off, route
  expert comm through the all-reduce term and keep `tEP = 0`.
- Re-scope `β₈` (`Beta[7]`, gated by `hasInterleavedMoE` `:314-319`) to interleave
  kernel-switch overhead only — and re-fit it (see [#E](#sub-issue-e--calibration-parallelism-sweep-corner--β_ep-fitting)),
  do not keep the old value.

**Acceptance criteria.**
- **Golden test (INV: BC-DP1):** `DP=1`, EP off → output byte-identical to
  current model across the existing test matrix.
- New tests: for a known MoE config at `(TP=2, DP=2)`, the routed-expert weight
  term is `1/DP` of the `DP=1` value; `tEP > 0`; shared-expert term present for a
  shared-expert model and absent otherwise.
- Hand-computed step-time check for one fully-worked `(TP=2, DP=2)` example
  (document the arithmetic in the test).

**Depends on:** #B. **Risk:** medium (touches the hot path — keep the single-pass,
zero-alloc structure).

---

### Sub-issue #D — KV-capacity DP scaling

**Goal.** Reflect that DP multiplies usable KV blocks.

**Why.** `CalculateKVBlocks` (`sim/latency/kv_capacity.go:136`) today sizes the KV
budget for a single TP group: `totalAvailableGiB = MemoryGiB · util · tp`
(`:185`), and a comment at `:191` states the activation memory is a "per-replica
constant (dp=1 in BLIS)". A DP+EP engine is `DP` such replicas, each with its own
full KV budget on its own GPUs (each DP rank is a separate `EngineCore` — see the
KV-cache fact in [§4](#4-verified-facts-from-vllm)). So the *aggregate* number of
usable KV blocks is `DP ×` a single rank's. Because requests are split disjointly
across DP ranks ([§2](#2-audience-primer-tp-vs-dp-vs-ep)), the global engine can
hold `DP×` as many concurrent sequences — which is exactly what raises the
capacity μ* the calibration validates.

**Scope.**
- In `sim/latency/kv_capacity.go`, multiply the final usable KV-block count by
  `DP` when the engine is DP+EP. (Equivalently, treat the budget as
  `MemoryGiB · util · tp · DP`.) Do **not** change the per-GPU weight/activation
  overhead accounting — those are per-replica and unchanged; only the block
  *count* scales.

**Acceptance criteria.**
- Unit test: capacity at `DP=2` is `2×` the `DP=1` capacity, all else equal.
- `DP=1` capacity unchanged (regression).
- The `dp=1` comment at `:191` is updated to reflect the new `DP` handling.

**Depends on:** #A. **Risk:** low.

---

### Sub-issue #E — Calibration: parallelism-sweep corner + `β_EP` fitting

**Goal.** Fit `β_EP` empirically and keep the existing `β₁…β₈` head (see the
`{c, β_C, β_M}` note in [§2.5](#25-symbol-table-verified-against-trained_physics_modelgo)).

**Scope.**
- Add `β_EP` to the coefficient set and the design matrix.
- Add the parallelism-sweep corner ([§11](#11-calibration)) to the calibration harness
  (sweep `(TP, DP)`, record at saturation).
- Joint NNLS recovering `β₄`, `β_EP`, **and a re-fit `β₈`** — the interleaved-MoE
  rows must include the `tEP` term so `β₈` fits only the *residual* interleave
  overhead, not the all-to-all (see [§7](#7-the-all-to-all-term)). Do **not** carry
  the old `β₈` value forward.

**Acceptance criteria.**
- The harness can drive DP+EP vLLM runs and ingest their measurements.
- Fitted `β_EP ≥ 0`; residuals reported; `β₄`/`β_EP`/`β₈` separated (condition
  number of the design matrix reported — Scout rows must exercise both `tEP` and
  `β₈` so they are identifiable).
- Re-fit on a TP-only dataset reproduces today's `β₄` (regression). Note that
  `β₈` will **change** for interleaved models (it no longer carries all-to-all);
  the regression target is end-to-end μ\*/step-time fidelity, not the old `β₈`
  value.

**Depends on:** #C. **Risk:** medium (needs multi-GPU data).

---

### Sub-issue #F — End-to-end μ\* validation on real MoE models

**Goal.** Prove fidelity, not just mechanics.

**Scope.**
- Run measured-vs-simulated μ* at ≥ 2 `(TP, DP)` operating points each for
  Mixtral, a shared-expert MoE (DeepSeek/Qwen), and Llama-4 Scout.

**Acceptance criteria.**
- Simulated μ* within the project's accepted error band of measured μ* at every
  tested point.
- A short results doc (table per model class) committed under `results/` or
  `docs/guide/`.

**Depends on:** #E. **Risk:** medium (hardware/time).

---

## 13. Decisions & trade-offs (for reviewers)

| Decision | Alternatives considered | Why this one | What breaks if wrong |
|----------|------------------------|--------------|----------------------|
| One logical engine over a global batch | Per-DP-rank queues + sync barrier | No scheduler rework; exact at saturation | Mis-estimates low-load latency (padding) — mitigated by `imbalanceFactor` |
| Expert load behind an interface | Hard-code balanced formula | Real routing is skewed (EPLB); enables future work | None now; would block EPLB modeling later |
| `β_EP` absorbs comm/HBM ratio initially | Topology-aware `bwComm` from day 1 | Matches existing `β₄` treatment; simpler fit | Inter-node EP (EP>gpusPerNode) under-modeled until follow-on |
| Engine-internal DP+EP only for MoE; dense DP = replicas | Unify dense + MoE DP | Dense DP is already correct as replicas; smaller blast radius | Nothing — dense global-batch framing is equivalent |
| Keep `β₈` as interleave overhead | Delete it / reuse for all-to-all | It models a real, separate effect; all-to-all is now `tEP` | Double-counting if `tEP` and `β₈` both claimed all-to-all |

---

## 14. Glossary

- **TP / DP / EP** — tensor / data / expert parallel degree. `EP = TP·DP`.
- **top-k / `kEff`** — experts each token is routed to (`num_experts_per_tok`).
- **dispatch / combine** — the two all-to-all collectives bracketing expert compute.
- **shared expert** — an FFN run for *every* token in addition to routed experts.
- **EPLB** — Expert-Parallelism Load Balancer (vLLM); rebalances hot experts via
  redundant copies.
- **μ\*** — the system's maximum sustainable request rate (capacity), the
  calibration's validation target.
- **`imbalanceFactor`** — `≥1` multiplier on MoE/all-to-all terms; `1.0` =
  balanced (exact at saturation).
