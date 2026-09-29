# Design: DP + EP support in the trained-physics latency backend (scoped, #A–#D)

> **Status:** design, pending review.
> **Scope owner decision record** for discussion
> [#1415](https://github.com/inference-sim/inference-sim/discussions/1415).
> The verbatim upstream proposal (§1–§14, all six sub-issues) lives alongside this
> file at `docs/plans/2026-06-09-dp-ep-support-design.md`. **That** document is the
> background/specification; **this** document is the scoped, decision-bearing design
> for the work we are doing now, and it overrides the proposal wherever they differ.

## 1. What this effort is (and is not)

The trained-physics latency model (`sim/latency/trained_physics_model.go`) models
only **tensor parallelism (TP)** — it divides every cost by `TP` and has no notion
of **data parallelism (DP)** or **expert parallelism (EP)**. For MoE deployments
served the way vLLM serves them, this misses two distinct MoE parallel modes:

1. **MoE tensor-parallel fallback (`--enable-expert-parallel=false`).** vLLM
   flattens the MoE tensor-parallel group across `TP·DP` ranks for MoE layers,
   while attention still runs as `DP` independent TP groups.
2. **Expert-parallel MoE (`--enable-expert-parallel=true`).** vLLM uses the same
   flattened `TP·DP` MoE group as an expert-parallel group (`EP = TP·DP` for PCP=1).

The current model therefore produces three errors: (B1) routed-expert weight and
compute are not scoped to the vLLM MoE group, especially when `DP>1`; (B2) MoE
dispatch/combine communication for DP/EP serving is unmodeled; (B3) shared experts
are parsed but ignored by `StepTime`.

**This effort delivers sub-issues #A–#D of the proposal — the modeling machinery —
and defers calibration.** Concretely:

- **In scope:** config plumbing (#A), an `ExpertPlacement` interface + balanced
  default (#B), the `StepTime` divisor/shared-expert/all-to-all refactor (#C), and
  KV-capacity DP scaling (#D).
- **Out of scope (deferred):** #E (empirical fitting of `β_EP` and re-fit of `β₈`)
  and #F (end-to-end μ\* validation on real multi-GPU MoE runs). The new
  coefficient `β_EP` ships with a **physics-derived default**, not a fitted value.
- **Separate future effort (its own design doc):** `blis distill-model-config` — a
  command that distills *model-architecture features only* from any HF `config.json`
  into a versioned, testable, extensible artifact. See [§8](#8-documented-follow-ons).

**Correctness bottom line.** After this effort the model covers both vLLM MoE
parallel modes (EP off and EP on) and is physically correct **at the saturation
operating point** the model targets (where the balanced-load assumption is exact).
Bugs B1, B2, B3 are fixed for parsed architectures. Because `β_EP` is defaulted
rather than fitted, and shared-expert stream overlap is modeled conservatively,
absolute MoE latency *magnitudes* are physics-estimated and are validated only
once #E/#F land.

## 2. Verified facts the design rests on

Two classes of facts were checked directly rather than assumed.

### 2.1 vLLM serving semantics (from the proposal §4, trusted)

For MoE models vLLM always builds an MoE/EP communication group over
`TP·DP·PCP` ranks; for this effort PCP is fixed at 1. Each DP rank is a separate
`EngineCore` owning a disjoint set of sequences and a full KV budget. Attention
weights are TP-sharded and DP-replicated. MoE layers differ by mode:

- **EP off:** vLLM flattens MoE tensor parallelism across `TP·DP`; routed experts
  are tensor-sharded over the flattened group. At `DP=1` the MoE output is **reduced**
  over the group; at `DP>1` vLLM instead **dispatches/combines** hidden states across
  the MoE group (the DP-aware fused-MoE path) — the dispatch/combine replaces the
  all-reduce, they do not both run (§5 taxonomy).
- **EP on:** vLLM maps the same flattened group to expert parallelism
  (`EP = TP·DP`), each GPU owns full routed experts for its expert partition, and
  dispatch/combine communication runs over the MoE group.

Shared experts (DeepSeek/Qwen/Llama-4) run for every token. vLLM can overlap
shared-expert execution on a separate CUDA stream for eligible token counts; this
effort models the conservative additive cost and documents overlap calibration as
part of #E/#F.

### 2.2 How real model `config.json` files map into BLIS (verified 2026-06)

Checked against upstream `config.json` for **Mixtral-8x7B**, **DeepSeek-V2-Lite**,
**Qwen3-30B-A3B**, **Llama-4 Scout**, against the parsing layer in
`sim/latency/config.go` (~`:240–312`):

| Model | routed-expert count source | shared-expert source | Result in BLIS |
|---|---|---|---|
| Mixtral-8x7B | `num_local_experts: 8` | none | ✅ correct (no shared expert) |
| DeepSeek-V2-Lite | `n_routed_experts: 64` (alias chain) | `n_shared_experts: 2 × moe_intermediate_size: 1408 = 2816` | ✅ correct |
| Qwen3-30B-A3B | `num_experts: 128` (alias chain) | none | ✅ correct |
| Llama-4 Scout | `num_local_experts: 16` | reuses `intermediate_size`, **no config field** | ⚠️ shared expert missed (`SharedExpertFFNDim = 0`) |

**Key finding:** the parsing layer already resolves the routed-expert count via an
alias chain (`num_local_experts → num_routed_experts → n_routed_experts →
num_experts`) and already derives the shared-expert dim from `n_shared_experts ×
moe_intermediate_size`. So DeepSeek and Qwen3-MoE are **not** silently treated as
dense, and DeepSeek's shared expert **is** already captured — the proposal's "no
shared-expert plumbing needed" claim is true for DeepSeek but **false for Scout**.

**Documented gaps (not fixed in this effort):**
- **Scout shared expert** is missed (no config field exposes its dim). The new
  shared-expert term is therefore a documented no-op for Scout.
- **Dense-layer placement** inside otherwise-MoE models (`first_k_dense_replace`
  for DeepSeek, `decoder_sparse_step`/`mlp_only_layers` for Qwen3) is parsed by
  nobody, so those models over-count MoE layers by a small amount (e.g. 1/27 for
  DeepSeek-V2-Lite). This must be included in the fixture tests as a known gap
  (`numMoELayers` expected to differ from vLLM for those fixtures) until the
  distill effort lands; expert-count parsing alone is not a full parity proof.

## 3. Sub-issue #A — Config plumbing

**`ModelHardwareConfig` (`sim/config.go`):**
- Add `DP int` (default 1) and `EnableExpertParallel bool` (mirrors vLLM
  `--enable-expert-parallel`).
- Update the canonical constructor `NewModelHardwareConfig` (R4 — single
  construction site).
- Add explicit helpers rather than overloading TP:
  - `EffectiveDP() int`: returns `max(1, DP)`.
  - `EffectiveMoEGroupSize() int`: for MoE returns `TP·DP`; for dense returns `TP`.
    This mirrors vLLM's flattened MoE group and is used by both EP-on and EP-off
    MoE paths.
  - `EffectiveEP() int`: returns `TP·DP` only when `EnableExpertParallel && isMoE`,
    else `1`. This helper is only an EP-mode predicate/size, not the MoE sharding
    divisor for EP-off fallback.

**Validation (construction-time):**
- `DP >= 1`.
- **Allow `DP > 1` for MoE models with either EP setting.** This is required for
  vLLM parity: EP-off MoE uses the flattened `TP·DP` tensor-parallel fallback;
  EP-on MoE uses the same flattened group as `EP`.
- **Reject `DP > 1` for dense models.** Dense data parallelism is mathematically
  equivalent to `DP` independent router replicas, so expressing it through the
  latency divisors would be a second, redundant path. Dense DP stays the existing
  router-replica mechanism.

**No new `HardwareCalib` fields.** The all-to-all term is normalized over the
existing `bwHbmUs`, and the defaulted `β_EP` absorbs the comm/HBM bandwidth ratio
(exactly as `β₄` does for the TP all-reduce today). A topology-aware `bwComm`
plus `gpusPerNode` is a documented follow-on, not part of this effort.

**`ModelConfig` (`sim/model_hardware_config.go`):** no new field. `SharedExpertFFNDim`
already exists and is already populated by the alias chain (§2.2). The work is to
*use* it in `StepTime` (#C), which currently ignores it.

**Acceptance test.** A table-driven test over the four real `config.json` fixtures
asserting `(NumLocalExperts, NumExpertsPerTok, MoEExpertFFNDim, SharedExpertFFNDim,
EffectiveMoEGroupSize, EffectiveEP)` for several `(TP, DP, EnableExpertParallel)`
pairs. The table also records known dense-layer placement gaps (`numMoELayers`
expected by BLIS today vs vLLM) so the fixture does not over-claim parity.

## 4. Sub-issue #B — `ExpertPlacement` interface + `BalancedPlacement`

**Location: `sim` core** (`sim/expert_placement.go`), not `sim/latency/`. Expert
placement is a model-*deployment* concept — the same domain as `ModelConfig` and
the parallelism degrees, which already live in core. It is the seam for future
EPLB / skewed-routing / redundant-expert modeling, and those future consumers
(scheduler, KV-capacity) must be able to reach it without importing `latency/`
(which would invert the dependency direction). The latency model already imports
`sim`, so it consumes `sim.ExpertPlacement` for free.

```go
// ExpertPlacement maps a step's routed-token population onto per-GPU MoE cost,
// returning the load of the BUSIEST GPU (a collective runs at its slowest
// participant). Single-method, pure query (R13/R14).
type ExpertPlacement interface {
    Resolve(globalTokens, kEff float64, numExperts, moeGroupSize, dp int) ExpertLoad
}

type ExpertLoad struct {
    PerGPUComputeTokens float64 // token·activations the max-loaded GPU computes
    PerGPUExpertCount   float64 // full-expert-equivalent weight bytes per GPU
    PerGPUCommTokens    float64 // dispatch+combine volume for busiest GPU (token·top_k)
}
```

`BalancedPlacement` (the default, `imbalanceFactor = 1`):

```
PerGPUComputeTokens = globalTokens · kEff / moeGroupSize
PerGPUExpertCount   = numExperts / moeGroupSize
PerGPUCommTokens    = (globalTokens / dp) · kEff · (moeGroupSize-1)/moeGroupSize · 2
```

`PerGPUCommTokens` is intentionally divided by `dp`: the latency term is paid by
the busiest source GPU, and each DP rank owns only `~globalTokens/dp` sequence
tokens. This is not an aggregate cluster byte count. (`dp` is passed in so the
placement strategy can reason about per-rank vs aggregate volume; `moeGroupSize`
is `TP·DP` for MoE.)

`TrainedPhysicsModel` holds a `sim.ExpertPlacement` field defaulting to
`sim.BalancedPlacement{}`. Because the contract returns the *busiest* GPU, the
"step = max over GPUs" physics emerges automatically when a future strategy
introduces imbalance.

**Acceptance test.** Hand-computed `ExpertLoad` for several
`(globalTokens, kEff, numExperts, moeGroupSize, dp)` tuples, including the
degenerate `moeGroupSize = 1`.

## 5. Sub-issue #C — `StepTime` refactor (the heart)

Apply the proposal's §6 divisor map **term by term** (not a blanket
`/tp → /(tp·DP)`). Keep the single-pass, zero-allocation structure. Compute
`dp := float64(m.dp)`, `moeGroup := m.EffectiveMoEGroupSize()`, and
`ep := m.EffectiveEP()` once; call
`m.placement.Resolve(globalTokens, kEff, numExperts, moeGroup, dp)` once per step
for MoE models.

**Terms that gain a `/DP`** (sequences split across DP ranks):
- Attention KV read/write (`bytesPfKv`, `bytesDcKv`): `dKV/tp → dKV/(tp·dp)`.
- Attention/decode projection compute (the `/tp` token-population terms): also `/dp`.
  The `hPerGPU` head-sharded attention FLOPs are **not** touched — DP does not
  shard heads; the DP effect rides the token-count path.
- Dense-FFN compute (interleaved models): `/tp → /(tp·dp)`.
- Shared-expert compute: `/(tp·dp)`.

**Terms that stay `/tp`** (weights replicated across DP groups): attention weights,
dense-FFN weight, shared-expert weight — **unchanged**.

**Routed-expert terms → replaced by `ExpertLoad`** (not a divisor tweak):
- Weight bytes use `PerGPUExpertCount` (= `numExperts/moeGroup`) in place of
  `nEff/tp`. This is the **B1** fix. **Applied unconditionally** — all MoE weight
  loading routes through `ExpertPlacement`, including `DP=1`/EP-off. In EP-on mode
  this is resident full experts per GPU; in EP-off mode it is full-expert-equivalent
  bytes for tensor-sharded expert weights over the flattened MoE group. This is the
  saturation-point behavior this latency model targets, and it **intentionally
  changes existing MoE step-time output** versus today's batch-dependent
  `nEff = min(N, max(k, B·k))/tp`.
- Compute uses `PerGPUComputeTokens`.

**Shared-expert term (new, B3):** gated on `SharedExpertFFNDim > 0`; compute
`/(tp·dp)`, weight `/tp`, scaled by MoE-layer count. (Documented no-op for Scout.)

**Convention (basis vs. coefficient).** To avoid double-applying a `β`, every
`t*` quantity below is defined as a **raw basis** (units of µs *before* its
coefficient). The coefficient is applied **once**, only in the final step-time sum
(§"Step-time formula"). So where this section writes `tMoEDispatch = …`, that is the
basis; the sum contributes `m.Beta[10] · tMoEDispatch`. Same for the `β₄`-class
terms below.

**TP / MoE communication taxonomy (verified against vLLM).** vLLM splits MoE
communication on the **`DP>1` boundary** into two *dispatch/combine* paths, and
falls back to an *all-reduce* only at `DP=1`. There are **three** mutually exclusive
regimes, keyed on the forward path actually taken:

1. **`DP=1` (any TP, EP-off or EP-on) → MoE-FFN all-reduce.** `forward_native`'s
   `reduce_output` all-reduces over `tp_size or ep_size` (`fused_moe/layer.py:1561`,
   gated `tp_size>1 or ep_size>1` and `not use_dp_chunking`). This is `tMoEReduce`.
2. **`DP>1`, EP-off → naive dispatch/combine.** The non-modular path takes
   `do_naive_dispatch_combine = dp_size > 1 and not FusedMoEModularMethod`
   (`fused_moe/layer.py:1792`), then `get_ep_group().dispatch(...)` /
   `.combine(...)`. This gate is **EP-independent** — `use_ep` only conditions a
   post-quant allgather optimization, not the dispatch itself. The `combine()`
   *replaces* the all-reduce (the mutual-exclusion mechanism is described below).
   This is `tMoEDispatch`.
3. **`DP>1`, EP-on → dispatch/combine (chunked all2all *or* naive).**
   `use_all2all_kernels = dp_size > 1 and use_ep` (`fused_moe/config.py:867`). The
   **chunked** all2all path (`forward_impl_chunked`) is taken only for the
   low-latency/batched backends (pplx / deepep_ll / mori / flashinfer-cutlass with
   `VLLM_ENABLE_MOE_DP_CHUNK`, `layer.py:744–750`); high-throughput / naive EP-on
   backends fall through to the **same `do_naive_dispatch_combine` path as regime 2**
   (`dp_size>1 and not modular`). Either way the comm is dispatch/combine over the
   flattened group → `tMoEDispatch`.

Regimes 2 and 3 are both DP>1 dispatch/combine over the flattened group, so a single
`tMoEDispatch` term gated on `DP>1` covers both; `tMoEReduce` covers only regime 1.
**Mutual-exclusion mechanism.** The all-reduce (regime 1) and dispatch/combine
(regimes 2–3) are kept from co-occurring by vLLM's `reduce_results` flag and
`must_reduce_shared_expert_outputs()` (`layer.py:1542–1545`), not solely by the
`not use_dp_chunking` structural guard on `reduce_output` (`layer.py:1564`) — when a
combine kernel already reduces across ranks, `reduce_results` is false so the
all-reduce is skipped. The design's conclusion (MoE-FFN comm charged exactly once on
the `DP` boundary) holds regardless of which gate enforces it. The truth table
(**PCP=1**, MoE model) the design encodes — for the general case substitute
`moeGroup = TP·DP·PCP` throughout:

| TP | DP | EP | `moeGroup` | MoE-FFN dispatch (`DP>1`) | MoE-FFN reduction (`DP=1`) |
|----|----|----|-----------|---------------------------|----------------------------|
| 1  | 1  | –  | 1         | no                        | no (single GPU)            |
| >1 | 1  | off| TP        | no                        | **yes** (over TP)          |
| >1 | 1  | on | TP        | **no** (DP=1)             | **yes** (over `ep_size=TP`)|
| ≥1 | >1 | off| TP·DP     | **yes** (naive, EP-indep) | no (dispatch *is* the comm)|
| ≥1 | >1 | on | TP·DP     | **yes** (all2all)         | no (comm *is* the all2all) |

Let `V(units, group) = units · totalTokens · hidden · 2 · 2 · (group-1)/group /
bwHbmUs` be the existing per-unit ring-all-reduce basis helper (the `·2·2` = BF16
bytes × ring phases, unchanged from today). The single monolithic `tTp` today
(`allReduceUnits = 2·numDenseLayers + numMoELayers`) is split into the terms below.

**Reduction-class terms (coefficient `β₄`, `Beta[3]`):**

- **Attention all-reduce (`tTpAttention`).** One unit per layer (`numLayers` units),
  over the **attention TP group of size `tp`**, scaled by `1/dp` (each DP rank
  all-reduces only its local `~totalTokens/dp` tokens; DP groups run in parallel):
  `tTpAttention = V(numLayers, tp) / dp`. Attention does **not** flatten across DP.
- **Dense-FFN all-reduce (`tTpDenseFFN`).** The 2nd unit on dense/interleaved-dense
  layers only (`numDenseLayers` units), TP-only, same `1/dp` split:
  `tTpDenseFFN = V(numDenseLayers, tp) / dp`.
- **MoE-FFN reduction (`tMoEReduce`).** The MoE-FFN all-reduce in regime 1 only —
  `DP=1`, `moeGroup = TP > 1` (rows 2–3 above). Reduced over the attention/MoE TP
  group of size `tp` (= `moeGroup` at `DP=1`):
  `tMoEReduce = V(numMoELayers, tp)`. Active gate:
  ```
  gateReduce = isMoE && numMoELayers > 0 && DP == 1 && tp > 1
  ```
  This covers both EP-off and EP-on at `DP=1, TP>1` (vLLM reduces over
  `tp_size or ep_size`, both `= TP` here). `0` at any `DP>1` (dispatch path) and at
  `TP=1` single-GPU.

**Dispatch/combine term (coefficient `β_EP`, `Beta[10]`) — bug B2:**

`PerGPUCommTokens` from `ExpertLoad` already carries the per-source-rank top-k and
the `(moeGroup-1)/moeGroup · 2` (dispatch+combine) factors (§4), so the basis byte
volume is just `PerGPUCommTokens · hidden · bpp` — do **not** re-multiply by `kEff`:
```
tMoEDispatch = (PerGPUCommTokens · hidden · bpp / bwHbmUs) · numMoELayers
gateDispatch = isMoE && DP > 1
```
The sum contributes `m.Beta[10] · tMoEDispatch` when `gateDispatch` holds. The gate
is `DP > 1` — **independent of EP** — because the dispatch/combine path is gated on
`dp_size > 1`, not on `use_ep`: the naive path
(`do_naive_dispatch_combine = dp_size > 1 and not modular`, `layer.py:1792`) fires for
**both** EP-off and EP-on-non-chunked configs, and the chunked all2all
(`use_all2all_kernels = dp_size > 1 and use_ep`, `config.py:867`) is the EP-on
low-latency variant of the *same* dispatch/combine. All variants move hidden states
over the flattened `moeGroup = TP·DP` (= `TP·DP·PCP`; PCP=1 here), so one
`tMoEDispatch` term — whose `PerGPUCommTokens` already reflects `moeGroup` (§4) —
covers every DP>1 cell. `gateReduce` (`DP==1`) and `gateDispatch` (`DP>1`) partition
the MoE-FFN comm on the `DP` boundary with no overlap and no gap, so the MoE-FFN comm
is charged exactly once in every cell.

**INV BC-DP1 constraint on the split.** At `DP=1` for a **dense** model:
`tTpAttention + tTpDenseFFN = V(numLayers, tp) + V(numDenseLayers, tp)
= V(2·numDenseLayers + numMoELayers, tp)` (since `numMoELayers=0`) — **exactly**
today's monolithic `tTp`. `tMoEReduce = 0` (not MoE), `tMoEDispatch = 0` (DP=1). The
split is value-preserving at the byte-identity boundary; the golden test holds.

**Coefficient wiring.** `β_EP` is the 11th coefficient → `Beta[10]`. The `Beta`
slice grows from length 10 to 11. Existing index map is unchanged
(`0=β₁ … 3=β₄ … 7=β₈, 8=β₉ prefill-kv split, 9=β₁₀ decode-kv split`).

The default-to-`β₄` behavior is **not passive** — it requires an explicit change to
the slice-build block in `NewTrainedPhysicsModel`
(`sim/latency/trained_physics_model.go:369–370`), which today is:
```go
betaSlice := make([]float64, 10)
copy(betaSlice, coeffs.BetaCoeffs[:min(10, len(coeffs.BetaCoeffs))])
```
A naive grow to `make(..., 11)` would leave `Beta[10] = 0` for the common
(7–10-coeff) callers, **silently disabling MoE dispatch/combine comm**. The required
change is three lines:
```go
betaSlice := make([]float64, 11)
copy(betaSlice, coeffs.BetaCoeffs[:min(11, len(coeffs.BetaCoeffs))])
if len(coeffs.BetaCoeffs) < 11 {
    betaSlice[10] = betaSlice[3] // β_EP defaults to β₄ when not explicitly provided
}
```
so MoE dispatch/combine is active out of the box and tracks the currently configured
TP communication correction. `defaults.yaml` is also extended to include the explicit
11th value for clarity (a provided 11th value overrides the β₄ default). **`β₈` is
kept as-is** (its re-fit is deferred to #E). A unit test asserts that a 10-coeff
caller yields `Beta[10] == Beta[3]` and an 11-coeff caller yields the provided value.

**Behavior-change boundary (important):**
- **Dense at `DP=1`** is **byte-identical** to today — **INV BC-DP1**, golden test.
- **MoE** output **intentionally changes** even at `DP=1`/EP-off, for two reasons:
  (1) **B1** — expert weight now uses `numExperts/moeGroup` instead of the
  batch-dependent `nEff/tp`; (2) **MoE-FFN comm** — at `DP=1, TP>1`, `tMoEReduce` now
  charges the MoE FFN all-reduce (`V(numMoELayers, tp)`), which today's code drops
  entirely (it deferred MoE-FFN comm to `β₈`, which is `0` for uniform MoE). This adds
  one `V(numMoELayers, tp)` unit of all-reduce that was previously
  unmodeled — a deliberate fidelity gain, not a regression. Existing MoE tests
  assert structural properties (layer counts, `β₈` gating, step-time > 0) and
  relative comparisons, **not** absolute golden step-times, so no brittle snapshot
  breaks; their reasoning comments are updated.

**Tests.**
- Golden (INV BC-DP1): dense `DP=1`, EP off → byte-identical across the existing matrix.
- At `(TP=2, DP=2)`, EP off: routed-expert weight/compute use `moeGroup=4`;
  attention TP all-reduce remains scoped to local `TP=2`; `tMoEDispatch` is active
  (naive dispatch/combine over `moeGroup=4`, EP-independent); `tMoEReduce` is absent.
- At `(TP=2, DP=2)`, EP on: routed-expert weight/compute use `EP=4`; `tMoEDispatch`
  is active (modular all2all); `tMoEReduce` is absent. The two `(TP=2, DP=2)` cells
  (EP off vs. on) produce **identical** MoE-FFN comm cost — the gate is `DP>1`, not EP.
- At `(TP=2, DP=1)`, EP off: `tMoEReduce` is active (all-reduce over `tp=2`);
  `tMoEDispatch` is absent — the `DP=1`/`DP>1` boundary between the two terms.
- Shared-expert term present for a DeepSeek-class config, absent for Mixtral; a
  separate test records Llama-4 Scout as a known parser gap until distill-model-config.
- One fully-worked `(TP=2, DP=2)` hand-arithmetic example (proposal §6.5) encoded
  as a golden with the arithmetic in a comment.

**Risk:** medium — the only hot-path change. Mitigation: preserve single-pass /
zero-alloc; the term-by-term table above is the review checklist.

## 6. Sub-issue #D — KV-capacity DP scaling

`CalculateKVBlocks` (`sim/latency/kv_capacity.go`) sizes the KV budget for a single
TP group. Each DP rank is a separate `EngineCore` with its own full KV budget on
its own GPUs, and requests are split disjointly across ranks, so aggregate usable
KV blocks scale by `DP`.

- Multiply the final usable KV-block **count** by `DP` when
  `isMoE && DP > 1`, regardless of `EnableExpertParallel`. This matches both vLLM
  MoE modes: EP-off flattened MoE tensor parallelism and EP-on expert parallelism
  both have one KV cache budget per DP `EngineCore`. Equivalently, aggregate
  budget = `MemoryGiB · util · TP · DP`, while per-GPU KV bytes remain divided only
  by attention `TP`.
- **Do not** change per-GPU weight/activation overhead — those are per-replica and
  unchanged; only the block count scales. Update the `dp=1` comment.
- Plumbing: extend `CalculateKVBlocks` (`sim/latency/kv_capacity.go:136`) with an
  explicit positional `dp int` immediately after `tp int`, so all construction sites
  fail to compile until updated; do not multiply at scattered call sites. **Call-site
  inventory (6 production sites)** that must thread `dp`:
  - whole-instance: `cmd/root.go:616` (run), and the run/replay model build.
  - **per-pool prefill/decode (4 sites)**: `cmd/root.go:1203,1237` and
    `cmd/replay.go:319,353`, which today pass `poolPrefillTP`/`poolDecodeTP`. Because
    per-pool DP is **out of scope** (§6.5), these pass the **global** `dp` (every pool
    shares it). They pass per-pool *TP* but global *DP* — call this out in the diff so
    a reviewer does not mistake the asymmetry for a bug.
  - The `dp` value is gated: pass `1` when the backend is roofline (see §6.5
    roofline-coherence), so KV scaling and step-time stay consistent.

**Tests.** For MoE, `DP=2` capacity is exactly `2×` the `DP=1` capacity for both
EP-off and EP-on, all else equal; dense `DP>1` is rejected before capacity sizing;
`DP=1` capacity unchanged (regression); per-pool TP with global DP scales each pool's
capacity by the global `DP`.

**Risk:** low.

## 6.5. CLI wiring (`run` / `replay` / `observe`)

DP/EP are **simulator-prediction** inputs, so they enter through the same path
`--tp` already uses and touch only the commands that build a latency model.

**`run` and `replay` — two new flags, mirroring `--tp`:**
- `--dp int` (default `1`) — data-parallel degree.
- `--enable-expert-parallel` bool (default `false`) — mirrors vLLM
  `--enable-expert-parallel`.

These are package-level vars in `cmd/root.go` (next to `tensorParallelism`),
validated there (`DP >= 1`; **reject `DP > 1` for dense models** per §3; **reject
`DP > 1` (and `--enable-expert-parallel`) when the backend is not `trained-physics`** —
see roofline-coherence below), then passed into the **canonical constructor
`NewModelHardwareConfig`** whose signature gains `DP`/`EnableExpertParallel` in #A.

**Constructor call-site reality (R4).** `NewModelHardwareConfig` is the single
canonical constructor, but the positional signature change touches **every** call
site, not just the two production paths. The two that carry the new flags are
`run` (`cmd/root.go:1632`) and `replay` (`cmd/replay.go:465`). The others must be
updated to compile and pass `DP=1`, EP off: `cmd/blis-kvtime/main.go:913` and ~30
test sites. This is the intended R4 "fail-to-compile until updated" behavior — the
diff is mechanical for the non-flag sites but must be exhaustive.

**Roofline-coherence (F4).** DP/EP affect step time **only** under the
`trained-physics` backend; `rooflineStepTime` (`sim/latency/roofline.go:291`) is
DP-blind (takes `tp`, not `dp`). But `CalculateKVBlocks` is backend-agnostic, so an
unguarded `--dp 2` under `--latency-model roofline` would scale KV capacity by DP
while leaving step time DP-blind — an internally inconsistent simulation. **Resolution:
reject `DP > 1` / `--enable-expert-parallel` at validation unless the backend is
`trained-physics`** (`logrus.Fatalf`, never silent degradation). Equivalently the
`dp` threaded into `CalculateKVBlocks` is forced to `1` for non-trained-physics
backends (§6), so the two never diverge. Adding DP to the roofline step-time model is
a documented follow-on, not part of this effort.

**Run/replay parity (INV-13).** Because `run` and `replay` reconstruct the model
through the *same* constructor call, a trace exported by `run --dp D
--enable-expert-parallel` and replayed with the identical flags produces identical
per-request metrics. The new flags are added to replay's flag set and validation in
lockstep with run; a replay that omits them defaults to `DP=1`, EP off (today's
behavior). No new fields are serialized into the trace — DP/EP are deployment
inputs supplied at replay time, exactly like `--tp`.

**Per-pool (disaggregation) overrides — out of scope.** `run`/`replay` support
`--prefill-tp`/`--decode-tp` via `PoolOverrides` (`sim/cluster/resolve.go`).
Per-pool `--prefill-dp`/`--decode-dp` are **not** added in this effort
(disaggregated MoE-EP is niche); global `--dp`/`--enable-expert-parallel` apply to
all pools. Adding per-pool DP later is additive (one `*int`/`*bool` field on
`PoolOverrides`) and does not change this design.

**`observe` — unchanged.** `observe` dispatches to a real inference server and
records measured timing; it builds **no** `ModelHardwareConfig` and **no** latency
model (verified: only `replay`/`run` construct one). DP/EP have no meaning for
observe — the deployment is whatever the real server runs — so `observe` gains no
new flags and its behavior is untouched.

## 7. Invariants

- **INV BC-DP1 (new).** With `DP=1` and EP disabled, every term reduces to its
  current value for **dense** models; existing dense simulations are byte-identical.
  Enforced by a golden test. (MoE output is intentionally *not* covered by this
  invariant — B1 is a deliberate fidelity fix.)
- **INV BC-ROOFLINE (new).** DP/EP have an effect **only** under the
  `trained-physics` backend. `DP > 1` or `--enable-expert-parallel` with any other
  backend is rejected at validation, so KV capacity (which scales by DP) and step
  time (DP-blind under roofline) never diverge into an inconsistent simulation.
  Enforced by a validation test (`run --latency-model roofline --dp 2` fatals).
- **INV-13 (run/replay parity) — preserved by construction, must be tested.**
  `run` and `replay` build the model through the same `NewModelHardwareConfig` call
  (§6.5), so identical `--dp`/`--enable-expert-parallel` flags yield identical
  metrics. A parity test exports a trace with DP/EP set on `run` and replays it. Note
  the per-pool KV path passes per-pool TP but **global** DP (§6); the parity test
  covers a pooled (`--prefill-tp`/`--decode-tp`) config with global `--dp` set.
- Other existing invariants (INV-1 … INV-12) are unaffected: no new events, no
  scheduler changes, no per-DP-rank queues — DP+EP is modeled as one logical engine
  over a global batch (proposal §5).

## 8. Documented follow-ons

- **#E / #F (deferred):** fit `β_EP` and re-fit `β₈` from a multi-GPU parallelism
  sweep at saturation; validate simulated μ\* against measured μ\* for Mixtral, a
  shared-expert MoE, and Scout.
- **Topology-aware `bwComm`:** replace the `bwHbmUs`-normalized all-to-all with a
  real inter-GPU bandwidth (NVLink vs inter-node) once `gpusPerNode` is sourced.
- **Load imbalance:** the `imbalanceFactor (≥1)` hook (proposal §9) for DP token
  padding and expert skew; exact at saturation today, dial-able later.
- **`blis distill-model-config` (separate design doc):** distill *model-architecture
  features only* (layers, heads, experts, shared experts, FFN dims, interleave /
  dense placement) from any HF `config.json` into a versioned, testable artifact.
  Would unify the duplicate alias-parsers in `sim/latency/config.go` and
  `sim/latency/kv_capacity.go` and cleanly absorb the Scout shared-expert and
  dense-layer-placement gaps. Hardware calibration and parallelism (TP/DP/EP) stay
  flag-driven, not part of the artifact.

## 9. Dependency order

`#A → #B → #C → #D`. CLI wiring (§6.5) rides on #A (it just exposes the new
`NewModelHardwareConfig` inputs) and lands with #C so the flags affect a complete
model. Calibration (#E → #F) depends on #C and is out of scope here.

## 10. Decisions log (for reviewers)

| Decision | Chosen | Why |
|---|---|---|
| Effort scope | #A–#D, defer #E/#F | Deliver modeling now; calibration needs multi-GPU data |
| `β_EP` default | defaults to current `β₄` (`Beta[3]`) when omitted — via **explicit** `betaSlice[10]=betaSlice[3]` injection (§5), not passive zero-fill | Communication active out of the box; fit later |
| `β₈` | kept as-is | Re-fit belongs to the #E calibration pass |
| All-to-all normalizer | existing `bwHbmUs` + defaulted `β_EP` | No new hardware field; matches `β₄` treatment |
| `β_EP` wiring | extend `Beta` slice 10→11; bump `copy` cap and add β₄ default-injection | Keeps the single-slice convention |
| `ExpertPlacement` home | `sim` core | Deployment concept; reachable without importing latency |
| MoE `DP>1`, EP off | supported | vLLM flattens MoE tensor parallelism across `TP·DP` |
| MoE `DP>1`, EP on | supported | vLLM maps the same flattened group to `EP=TP·DP` |
| B1 fix | applied unconditionally | Physically correct; one code path |
| Dense `DP>1` | rejected at construction | Equivalent to router replicas; avoid redundant path |
| DP/EP under non-trained-physics backend | rejected at validation (INV BC-ROOFLINE) | Roofline step time is DP-blind; KV scales by DP → reject to avoid an inconsistent sim |
| CLI flags | `--dp`, `--enable-expert-parallel` on `run`+`replay` only | Same path as `--tp`; observe builds no latency model |
| Per-pool DP override | deferred; global DP threads into per-pool KV (per-pool TP, global DP) | Disaggregated MoE-EP is niche; additive later |
| MoE byte-identity | not guaranteed | B1 is a deliberate fidelity fix; tests are structural |
| Scout shared expert / dense placement | documented gaps | Orthogonal to DP/EP; candidates for distill effort |
