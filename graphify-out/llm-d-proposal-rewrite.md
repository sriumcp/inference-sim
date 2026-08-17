# Proposal Rewrite: Scope, Parity, and Alternatives

Drop-in replacements for the **Scope**, **Maintaining Parity with llm-d Updates**, and
**Alternatives** sections, plus one new section (**Where BLIS Sits: The Abstraction
Boundary**). Written to answer the *architectural* and *governance* questions the
reviewers actually raised — not to re-litigate frontend fidelity, which the thread
already conceded.

Guiding shift: stop defending BLIS as "another simulator that happens not to emulate
the frontend," and position it as **the cluster-level, engine-agnostic performance
estimation layer of llm-d** — the one layer whose subject matter (routing, admission,
flow control, autoscaling, placement) lives *between* engines and therefore cannot be
modeled from inside any single engine.

---

## NEW SECTION — Where BLIS Sits: The Abstraction Boundary

*(Place this immediately after the Summary, before Motivation. It sets the frame the
rest of the document defends.)*

llm-d is intentionally engine-agnostic and hardware-agnostic: it decouples routing,
scheduling, admission, and orchestration from any single inference engine or
accelerator. A simulator for llm-d should share that property. A performance model
that lives *inside* vLLM necessarily inherits vLLM's assumptions — its cache layout,
its scheduler internals, its hardware target. BLIS is designed to sit one layer above
those assumptions, where llm-d's own value lives.

Three simulator archetypes answer three different questions. None replaces another:

| Layer | Question it answers | Example |
| --- | --- | --- |
| **Functional replay** | *Did this exact engine reproduce this exact request, bugs and all?* | trace capture/replay (e.g. vllm-vcr) |
| **Engine simulation** | *Is this engine's implementation correct/fast?* | vLLM-native simulator (`vllm#47922`) |
| **Cluster performance estimation** | *Which distributed-systems design wins across many configurations?* | **BLIS** |

The concern raised in review — "will an external simulator lag vLLM's fast-moving
internals?" — is a real risk for the first two layers, because they own engine
mechanism. It applies far less to the third, because BLIS does not own engine
mechanism. It owns the *timing consequences* of that mechanism, exposed through a
small, calibrated interface. The following table states exactly where the line is:

| Concern | Owned by the engine | Modeled by BLIS | How it enters BLIS |
| --- | :---: | :---: | --- |
| Request parsing / tokenizer | ✓ | — | not modeled |
| Frontend / API-surface quirks | ✓ | — | not modeled |
| CUDA/attention kernels | ✓ | — | not modeled |
| KV-cache group layout, hybrid/sliding attention, eviction policy | ✓ | timing/capacity only | enters as a *calibrated step-time and KV-capacity term*, not a reimplemented mechanism |
| Quantization | ✓ | weight-precision only | 3-tier precision detection → weight bandwidth + KV capacity |
| Scheduler / batching semantics | shared contract | ✓ | pluggable `BatchFormation` / `InstanceScheduler` policy behind an interface |
| Routing & scoring | — | ✓ | llm-d-parity scorer framework |
| Admission & flow control | — | ✓ | `AdmissionPolicy`, `SaturationDetector`, gateway `FairnessPolicy` |
| Autoscaling & placement | — | ✓ | `Collector`/`Analyzer`/`Engine`/`Actuator`, `ExpertPlacement` |
| Distributed inference architecture (multi-replica, PD split) | — | ✓ | cluster-level DES |

The rows most likely to change quickly in vLLM (attention variants, cache groups,
eviction) are precisely the rows BLIS does **not** reimplement. They matter to BLIS
only through their effect on step time and memory capacity, which are absorbed by
recalibrating independent latency terms (see *Maintaining Parity* below). This is the
architectural reason BLIS's parity surface is smaller than an engine-internal model's,
not a larger one.

---

## REPLACEMENT — Scope

*(Replaces the existing Scope section. Keeps its two boundaries but reframes the lead
around layer, and adds the engine-agnostic claim honestly — as a real seam with
prioritized, not exhaustive, parity.)*

BLIS is a **stack-level, discrete-event performance estimator** for distributed LLM
inference. Where an engine-level simulator models a single inference process in
isolation, BLIS models the full llm-d cluster: requests flow through llm-d-router —
which applies scoring, admission control, flow control, and routing — before reaching
one or more engine replicas whose batching, KV-cache, and prefill/decode behavior are
modeled in turn. The behaviors that dominate tail latency and throughput (how
admission control shapes queue depth under burst traffic; how routing interacts with
KV-cache locality) *emerge from the interaction* between the router and the engines.
They are invisible to any model that sees only one engine.

Two boundaries define the scope:

**1. Timing, not function.** BLIS models engine behavior through calibrated timing
estimates, not functional execution. Forward-pass latency, scheduling delays, and
KV-cache dynamics are derived from benchmark data and represented as parameterized
policy interfaces, not computed by running the model. BLIS therefore does not
reproduce request parsing, API-surface quirks, or frontend bugs — fidelity it
deliberately trades for the speed and breadth that make configuration search
tractable. Tools that functionally emulate engine operation are complementary, not
alternatives.

**2. Engine-agnostic by construction.** BLIS represents engine mechanics —
scheduling, batching, KV behavior — as calibrated policy interfaces rather than
hard dependencies on one engine. The seam is already real: the scheduling and
batching contracts (`InstanceScheduler`, `BatchFormation`) are single-method
interfaces, and common policies such as FCFS are shared across vLLM, SGLang, and other
engines by construction. This lets BLIS model different engines side by side and study
how each interacts with llm-d's stack-level policies. We are explicit that engine
parity is **prioritized, not exhaustive**: the goal is to model the timing-relevant
behavior that changes cluster-level outcomes, not to replicate every engine internal.
vLLM is the most complete backend today; SGLang and additional engines are on the road
map, built on the same interfaces rather than as forks.

Timing accuracy at these interfaces comes from calibration against real benchmark
runs, not from executing the model. This is sufficient for the three target
use cases — capacity planning, algorithm development, and policy discovery — where the
goal is to compare *relative* performance across many configurations, not to reproduce
the exact latency of any single request on any single engine build.

Speed is a requirement of these use cases, not a convenience:

- Capacity planning sweeps hundreds of candidate configurations.
- Algorithm development iterates over many policy variants against recorded traces.
- AI-driven policy discovery evolves candidates across large search spaces.

None is tractable at wall-clock speed. Discrete-event simulation is the only approach
that decouples simulated time from wall-clock time — advancing event-to-event rather
than executing work in real time — which is what delivers the ~200× speedups these
workflows depend on. Approaches that execute or replay the real engine (trace
capture/replay, CPU forward passes) are bounded by real time: excellent for fidelity,
unsuited to config-search and policy-evolution.

---

## REPLACEMENT — Maintaining Parity with llm-d Updates

*(Replaces the existing section. Answers wseaton's specific list — cache config,
kvcache groups, hybrid attention, eviction, quantization — item-by-item, in terms of
how each enters the model. Makes one honest concession. De-emphasizes the AI-parity
future from a promise to a direction.)*

Not every llm-d or engine change requires a simulator change. Because BLIS models
performance rather than function, only three classes of upstream change are
parity-relevant:

1. a significant change to an architecture or **interface**,
2. a new algorithm that **materially alters latency**, or
3. a change to the **request journey** through the stack.

The large class of changes that do not affect timing — request parsing, API additions,
response formatting, metrics bookkeeping, other frontend behavior — requires no
simulator update. This keeps BLIS's parity surface far smaller than a functional
emulator's, which must track all of it.

**The fast-moving vLLM internals, specifically.** Review raised model-derived cache
config, KV-cache groups, per-layer/hybrid attention, eviction and tiered-cache
policies, and quantization — all of which change quickly in vLLM. BLIS does not
reimplement any of these mechanisms. Each enters the model only through its timing or
capacity consequence:

- **Cache config, KV-cache groups, hybrid/sliding attention, eviction** change the
  *effective step time and KV capacity*. BLIS's latency model is a **sum of
  independent terms** (prefill compute, decode compute, weight bandwidth, TP
  all-reduce, MoE dispatch/reduce, per-layer and batch terms — each with its own
  calibration coefficient). A change to one contributor is recalibrated in isolation,
  without touching the others or the mechanism itself.
- **Quantization** enters through three-tier weight-precision detection feeding the
  weight-bandwidth and KV-capacity terms — the storage-precision axis is already
  decoupled from the compute/KV dtype.

**Two architectural properties keep the cost of tracking these low:**

- **Pluggable policy interfaces.** BLIS exposes the same control- and data-plane
  contracts as llm-d — scoring, admission, flow control, routing, scheduling — so a new
  algorithm is added as a policy template *behind an existing interface*, not as a
  rewrite.
- **A decomposable latency model.** Because the latency estimate is a sum of
  independent, individually-calibrated terms, a change affecting one contributor is
  recalibrated in isolation.

**How parity is enforced today — the differentiator.** Most simulators have no
disciplined parity process; BLIS ships one, built around *code-proofs*. Because AI
agents can paraphrase or hallucinate behavior from memory, every parity change must be
grounded in the actual reference source: a cross-repo feature issue template requires
GitHub permalinks to the real llm-d-router / vLLM code, the behaviors to preserve, the
intentional deviations, and the target commit — and the agent must verify each
permalink resolves before proceeding. An issue-review skill then checks evidence
quality, scope, and coverage; an implement-issue skill turns validated issues into PRs
under test-driven development and the simulator's invariants, converging through
automated self-review. Every parity claim is thus pinned to verifiable reference code
at a commit, not to an agent's recollection.

**An honest boundary.** For the newest, fastest-moving engine internals, BLIS's timing
terms will sometimes trail the engine until recalibrated. We accept this deliberately:
the target use cases compare *relative* performance across configurations and policies,
where a small absolute lag on a bleeding-edge attention kernel does not change which
configuration or policy wins. BLIS is built to help evaluate design and deployment
choices before they land — not to be the source of truth for the exact latency of the
latest engine build. When day-one, mechanism-exact fidelity is the goal, an
engine-native simulator or trace replay is the right tool.

**Direction.** We plan to augment this with an automated parity-discovery workflow
that watches tracked engines for changes, classifies each by whether it touches an
interface, the request journey, or latency-relevant behavior, discards the
performance-irrelevant majority, and files code-proof-backed issues into the existing
review/implement pipeline. We present this as a direction, not a dependency: the
shipped code-proof workflow already carries parity today.

---

## REPLACEMENT — Alternatives

*(Replaces the existing Alternatives section. Adds the layer framing, an explicit
capability table, and — critically — an explicit endorsement of the native
simulators so the proposal reads as "complementary layers," not "replacement.")*

The reviewers rightly observe that the simulator space is plural, and that llm-d need
not endorse "one simulator to rule them all." We agree. The right framing is not one
tool versus another but **which layer of the stack each tool models**. Three
archetypes answer three different questions:

| Capability | Trace replay | Engine-native sim | **BLIS** |
| --- | :---: | :---: | :---: |
| Reproduce frontend bugs / API quirks | ✓ | ✓ | — |
| Mechanism-exact scheduler / kernel behavior | ✓ | ✓ | configurable abstraction |
| Evaluate routing / scoring algorithms | limited | limited | ✓ |
| Evaluate admission & flow control | limited | limited | ✓ |
| Evaluate autoscaling & placement | limited | limited | ✓ |
| Multi-engine (vLLM, SGLang, …) side by side | no | no | ✓ (prioritized) |
| Multi-hardware / accelerator-agnostic | limited | limited | ✓ |
| Compare hypothetical architectures | no | difficult | ✓ |
| Sweep hundreds–thousands of what-if configs | no (real-time bound) | no (real-time bound) | ✓ |

None of these is "better." Each is optimized for a different question.

**Engine-native simulation** (e.g. `vllm#47922`) runs through the real
scheduler/KV-cache path and gets engine internals right by construction. It is the
correct tool for validating an engine's implementation and for behavior that depends on
mechanism-exact internals. **We think this work is valuable and should continue — it
answers a question BLIS does not.**

**Functional replay** (e.g. vllm-vcr) captures and replays real sessions with byte- and
timing-identical fidelity, preserving prefill/decode contention, tail latencies, and
even frontend bugs, with durable trace storage for "run on GPUs once" CI. It is the
correct tool when the goal is faithful reproduction of a captured run. It is bounded by
wall-clock time and models a single engine instance, so it does not serve
configuration search or cluster-level policy evaluation.

**BLIS** models the layer where llm-d's own value lives: how llm-d-router's scoring,
admission, and flow control interact with batching, KV-cache, and prefill/decode
placement across many replicas — at ~200× real time, so hundreds of configurations and
large policy search spaces are tractable. This is the layer no engine-internal model
can reach, because the behavior emerges *between* engines.

The two rejected alternatives remain rejected: relying solely on real-cluster
evaluation is prohibitively slow, costly, and non-deterministic for large-scale search;
and building a new cluster-level simulator from scratch would discard a year of
validated work. DynoSim demonstrates that production ecosystems now treat simulation as
first-class — but it models the Dynamo stack, not llm-d-router scoring, llm-d's
admission/flow-control semantics, or llm-d component interactions. The right response to
peer ecosystems investing in simulation is for llm-d to do the same, **on its own,
engine-agnostic terms** — which is exactly the layer BLIS occupies.

**Long-term vision.** Not "BLIS instead of engine-native simulators," but a layered
llm-d simulation stack in which engine-native simulation and trace replay validate
*implementation fidelity*, and BLIS evaluates *distributed-systems design*. Each
strengthens llm-d; together they cover the space no single tool can.

---

## Notes for the authors (not for the doc)

- **Do NOT re-run the "BLIS is not a functional emulator" argument** in the next reply.
  wseaton already conceded it ("Great, this helps"). Repeating it reads as not
  listening. The new sections make the point structurally (via the boundary table)
  without re-asserting it.
- The **abstraction-boundary table** is the single highest-leverage addition. It turns
  "trust us on parity" into a contract reviewers can inspect. Every row I annotated with
  "how it enters BLIS" is defensible against your actual code (`sim/latency/`,
  `sim/scheduler.go`, `sim/batch_formation.go`, `sim/cluster/`). Keep those annotations —
  they are what make it credible rather than hand-wavy.
- The **honest-lag concession** in the Parity section is deliberate and load-bearing.
  Conceding the one thing you genuinely can't guarantee (day-one bleeding-edge fidelity)
  makes every other claim more believable. Don't cut it to look stronger; it makes you
  weaker.
- Verify one phrasing against your team before posting: I wrote engine parity as
  "prioritized, not exhaustive." That matches your stated position, but make sure
  co-authors are aligned that BLIS will *deliberately* not replicate every engine
  internal — some reviewers may read that as a weakness unless it's framed (as here) as
  a design choice tied to the use case.
