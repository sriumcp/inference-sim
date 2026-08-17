// Package kvtime — GreedyKVScheduler and WFQScheduler implementations.
//
// Both implement sim.InstanceScheduler (OrderQueue interface) and are
// injected into the simulator via sim.NewSimulatorWithScheduler.
//
// GreedyKVScheduler:
//   - At each tick: update Meter, reconcile Buckets, score by θ_r = max(0,B_i)/k_r
//   - Sort descending (most entitled and most block-efficient first)
//
// WFQScheduler (Token-WFQ / VTC baseline, Sheng et al. OSDI 2024):
//   - Maintain per-tenant virtual counter C_i
//   - Counter-lift v_r = C_i when request enters queue
//   - Charge w_p · |P_r| on prefill admission, w_q = 2 per decode step (approximated)
//   - Sort ascending by (v_r, arrival_time)
package kvtime

import (
	"sort"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
)

// ─── GreedyKVScheduler ───────────────────────────────────────────────────────

// GreedyKVScheduler implements sim.InstanceScheduler using KV-time entitlement
// buckets.  At each scheduler tick it:
//  1. Ticks the Meter to record per-tenant KV-time consumed.
//  2. Reconciles the BucketManager to update balances.
//  3. Scores each waiting request as θ_r = max(0, B_i) / k_r.
//  4. Sorts the wait queue descending by θ_r (highest score dispatched first).
//
// Two-phase init: call SetSimulator after constructing the sim.Simulator.
type GreedyKVScheduler struct {
	kvCache   *kv.KVCacheState
	simulator *sim.Simulator // set via SetSimulator after Simulator is created
	meter     *Meter
	buckets   *BucketManager

	// reqToTenant is a persistent cache of request-ID → tenant-ID for all
	// requests ever seen.  Accumulated from both the wait queue and the running
	// batch so that Meter.Tick gets complete attribution.
	reqToTenant map[string]string

	lastTickUs int64
}

// NewGreedyKVScheduler creates a GreedyKVScheduler.  Call SetSimulator before
// the simulation loop starts.
func NewGreedyKVScheduler(kvCache *kv.KVCacheState, meter *Meter, buckets *BucketManager) *GreedyKVScheduler {
	return &GreedyKVScheduler{
		kvCache:     kvCache,
		meter:       meter,
		buckets:     buckets,
		reqToTenant: make(map[string]string, 128),
	}
}

// SetSimulator wires the scheduler to the simulator after construction.
// Must be called once before the simulation loop starts.
func (s *GreedyKVScheduler) SetSimulator(simulator *sim.Simulator) {
	s.simulator = simulator
}

// Meter returns the underlying Meter (for final metrics collection by runner).
func (s *GreedyKVScheduler) Meter() *Meter { return s.meter }

// Buckets returns the underlying BucketManager (for diagnostics).
func (s *GreedyKVScheduler) Buckets() *BucketManager { return s.buckets }

// OrderQueue implements sim.InstanceScheduler.
//
// Called once per scheduler tick from sim.Simulator.scheduleBatch.
// The requests slice contains all requests currently in the wait queue.
// OrderQueue reorders them in-place.
func (s *GreedyKVScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}

	// Step 1: accumulate reqToTenant from wait queue.
	for _, r := range requests {
		if r.TenantID != "" {
			s.reqToTenant[r.ID] = r.TenantID
		}
	}

	// Step 2: also include running batch so the Meter attributes all KV holders.
	if s.simulator != nil && s.simulator.RunningBatch != nil {
		for _, r := range s.simulator.RunningBatch.Requests {
			if r.TenantID != "" {
				s.reqToTenant[r.ID] = r.TenantID
			}
		}
	}

	// Step 3: tick the Meter.
	s.meter.Tick(s.kvCache, s.reqToTenant, clock)

	// Step 4: reconcile bucket balances.
	cumKVTime := s.meter.TenantKVTime()
	s.buckets.Reconcile(cumKVTime, clock, s.lastTickUs)
	s.lastTickUs = clock

	// Step 5: score each waiting request and sort.
	blockSz := float64(s.kvCache.BlockSizeTokens)

	type scored struct {
		req   *sim.Request
		score float64
		idx   int // stable sort tiebreak
	}
	scoredReqs := make([]scored, len(requests))
	for i, r := range requests {
		blocks := s.kvCache.RequestMap[r.ID]
		residentTokens := float64(len(blocks)) * blockSz
		sc := s.buckets.Score(r.TenantID, residentTokens)
		scoredReqs[i] = scored{req: r, score: sc, idx: i}
	}

	sort.SliceStable(scoredReqs, func(i, j int) bool {
		if scoredReqs[i].score != scoredReqs[j].score {
			return scoredReqs[i].score > scoredReqs[j].score // descending
		}
		// Tie-break: earlier arrival first (FIFO within same score).
		return scoredReqs[i].req.ArrivalTime < scoredReqs[j].req.ArrivalTime
	})

	for i, sr := range scoredReqs {
		requests[i] = sr.req
	}
}

// AllowAdmission implements sim.AdmissionAwareScheduler.
//
// Returns false when the request's tenant is overdrawn (B_i ≤ 0). The
// FormBatch dequeue loop calls this for each peeked request before
// invoking AllocateKVBlocks; a false return keeps the request at the
// front of the wait queue and the loop breaks. The bucket is credited at
// rate ω_i·K per tick by Reconcile, so a held tenant resumes admission
// once its balance climbs back above zero.
//
// This is the entitlement-enforcement path that distinguishes GreedyKV
// from pure ordering-only schedulers. Without it, OrderQueue's score-based
// sort is decorative whenever the KV cache is not physically saturated —
// the dequeue loop simply admits everything that fits, and overdrawn
// tenants' requests still enter the running batch because there is no
// admission gate. Combined with OrderQueue's "score=0 for overdrawn" sort
// (which places overdrawn-tenant requests at the back of the queue), the
// dequeue loop's break-on-veto contract is correct: all admittable
// requests have already been peeked when AllowAdmission first returns
// false.
//
// Overdraft semantics: a tenant whose bucket reached the negative floor
// (set by BucketManager via HSeconds; 0 means strict) is held until
// Reconcile credits the balance back above zero. With HSeconds=0 the
// predicate reduces to "B_i > 0".
func (s *GreedyKVScheduler) AllowAdmission(req *sim.Request, _ int64) bool {
	if req == nil || s.buckets == nil {
		return true
	}
	if req.TenantID == "" {
		// Untenanted requests are not subject to entitlement gating; admit.
		return true
	}
	return !s.buckets.IsOverdrawn(req.TenantID)
}

// KVtime now implements paper §6's full unified admission/continuation/preemption
// rule. AllowAdmission (above) is the entitlement gate; ChooseVictims (below)
// is the density-ordered eviction half. Together they discharge the
// work-conservation hypothesis of thm:service per paper §6 line 644.
//
// The score θ_r = max(0, B_i) / k_r used by both OrderQueue and ChooseVictims
// is paper §6 line 639's formula
//
//     θ_r(t) = [Φ(B_τ(r)(t)) · ρ_r(t) − R_r(t)·𝟙[r ∉ S^−(t)]] / k_r(t)
//
// with the substitutions Φ(B) = max(0, B), ρ_r = 1 (no per-request SLO
// weighting in this experiment set), and R_r = 0 (the simulator does not
// model resume cost — there is no R_r field on Request and preemptForTokens
// resets ProgressIndex rather than tracking partial progress; setting R_r=0
// is a faithful representation of the simulator's behavior). With R_r = 0,
// the indicator 𝟙[r ∉ S^−] vanishes from scoring, so candidate and incumbent
// scores are directly comparable.

// ChooseVictims implements sim.PreemptionAwareScheduler (paper §6 line 644).
//
// Strict density-ordered eviction: when the KV cache is full and a waiting
// candidate has higher score than some incumbent, evict the lowest-scoring
// incumbents until the candidate fits. The score is θ_r = max(0,B_i)/k_r.
//
// Work-conservation invariants enforced (paper §6 line 644):
//   - The candidate's score is computed with k_r = its prefill token count
//     (len(InputTokens)), i.e. the KV space it would need to occupy at
//     admission. This matches what AllocateKVBlocks reserves at admission.
//   - Eviction targets are chosen by score, NOT by bucket sign — an incumbent
//     within budget can still be evicted IF a strictly higher-density waiter
//     needs its slot. Conversely, an over-budget incumbent is NOT evicted
//     when capacity could otherwise sit unused (the AllowAdmission path is
//     the gate, not ChooseVictims).
//   - Strict `<` not `≤`: same-score incumbents are preserved per paper's
//     "strictly higher-scoring request" wording.
//   - Equal-score tiebreak: latest arrival first (proxy for "least KV
//     invested"; matches BLIS default's FCFS-tail eviction convention).
//
// Cross-tenant eviction is intentionally not filtered. Same-tenant eviction
// is rare in practice — within a single tenant, candidate and incumbent share
// B_i so the score difference reduces to inverse-k_r ordering, and that only
// fires when the candidate's prefill is dramatically smaller than the
// incumbent's current footprint. When it does fire, it's correct: the
// scheduler is reclaiming capacity from a larger same-tenant request to admit
// a smaller, more efficient one.
func (s *GreedyKVScheduler) ChooseVictims(candidate *sim.Request,
	running []*sim.Request,
	_ int64) []int {
	if candidate == nil || len(running) == 0 || s.buckets == nil {
		return nil
	}
	blockSz := float64(s.kvCache.BlockSizeTokens)

	// Candidate score: use prefill token count as k_r at admission.
	// Score returns 0 for overdrawn tenants (B_i ≤ 0); such candidates
	// should already have been rejected by AllowAdmission, so a 0 here is
	// defensive — bail out without evicting anyone.
	candTokens := float64(len(candidate.InputTokens))
	if candTokens <= 0 {
		return nil
	}
	candScore := s.buckets.Score(candidate.TenantID, candTokens)
	if candScore <= 0 {
		return nil
	}

	// k_r=0 incumbents (in RequestMap with zero blocks) are extremely rare in
	// practice — admission has always been followed by AllocateKVBlocks, which
	// assigns at least one block. For defense-in-depth: BucketManager.Score
	// returns the raw balance (not bal/0) for k_r=0 (see bucket.go:215-217).
	// That value, by construction, exceeds any positive-k_r candidate's
	// density score, so the strict-< comparator below correctly excludes
	// such incumbents from eviction. This is the desired behavior — a
	// freshly-admitted incumbent that has not yet allocated its KV should
	// not be the first thing we evict.
	type indexed struct {
		idx     int
		score   float64
		arrival int64
	}
	scored := make([]indexed, 0, len(running))
	for i, r := range running {
		if r == nil {
			continue
		}
		residentTokens := float64(len(s.kvCache.RequestMap[r.ID])) * blockSz
		sc := s.buckets.Score(r.TenantID, residentTokens)
		if sc < candScore { // STRICT — paper line 644 "strictly higher"
			scored = append(scored, indexed{i, sc, r.ArrivalTime})
		}
	}
	if len(scored) == 0 {
		// R19 note: returning nil here (no incumbent strictly out-scored by
		// candidate) does NOT livelock. Each running request has bounded
		// MaxOutputLen, so an incumbent will eventually complete and free
		// space organically; the candidate then admits via AllowAdmission
		// on the next tick. INV-8 work-conservation is preserved by FormBatch.
		return nil
	}

	// Lowest score first; tiebreak by latest arrival (least KV invested),
	// then by `idx` (deterministic running-batch position) so that two
	// requests admitted at the same tick with the same score produce a
	// stable victim ordering across runs (INV-6 determinism). All input
	// sources are deterministic — iteration over `running` is a slice
	// walk, not a map iteration — so this comparator alone suffices.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score < scored[j].score
		}
		if scored[i].arrival != scored[j].arrival {
			return scored[i].arrival > scored[j].arrival
		}
		return scored[i].idx > scored[j].idx
	})
	victims := make([]int, 0, len(scored))
	for _, x := range scored {
		victims = append(victims, x.idx)
	}
	return victims
}

// ─── WFQScheduler ────────────────────────────────────────────────────────────

// WFQScheduler implements sim.InstanceScheduler as Token-WFQ / VTC
// (Sheng et al. OSDI 2024, Section 4.2).
//
// Virtual counter semantics:
//   - C_i: per-tenant virtual clock, accumulates work done by tenant i.
//   - v_r:  "lift" of request r = C_i at the moment r enters the wait queue.
//     Requests are dispatched in ascending order of v_r (lowest first).
//   - When r is admitted (transitions queue → running): C_i += w_p · |P_r|.
//     For decode, charge w_q per completed step.
//
// Prefill charging is detected by watching which request IDs appear in the
// running batch for the first time between successive OrderQueue calls.
// Decode-step charging uses a coarse approximation: credit w_q once per tick
// per running request (since OrderQueue is called roughly once per decode step).
//
// Two-phase init: call SetSimulator after constructing the sim.Simulator.
type WFQScheduler struct {
	simulator *sim.Simulator

	// tenantCounter is C_i: virtual clock per tenant.
	tenantCounter map[string]float64

	// requestLift is v_r: virtual time at which request r first appeared in queue.
	requestLift map[string]float64

	// prevRunningIDs tracks request IDs that were in RunningBatch at the last tick.
	// Used to detect newly-admitted requests (to charge prefill) and
	// requests completing decode steps (to charge w_q).
	prevRunningIDs map[string]bool

	// requestInputLen caches the input token count per request-ID for charging.
	requestInputLen map[string]float64

	wPrefill float64 // w_p = 1.0 (normalised)
	wDecode  float64 // w_q = 2.0 (per step, per Sheng et al.)
}

// NewWFQScheduler creates a WFQScheduler with standard VTC weights (w_p=1, w_q=2).
// Call SetSimulator before the simulation loop starts.
func NewWFQScheduler() *WFQScheduler {
	return &WFQScheduler{
		tenantCounter:   make(map[string]float64),
		requestLift:     make(map[string]float64),
		prevRunningIDs:  make(map[string]bool),
		requestInputLen: make(map[string]float64),
		wPrefill:        1.0,
		wDecode:         2.0,
	}
}

// SetSimulator wires the scheduler to the simulator after construction.
func (w *WFQScheduler) SetSimulator(simulator *sim.Simulator) {
	w.simulator = simulator
}

// OrderQueue implements sim.InstanceScheduler.
func (w *WFQScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		// Still charge for decode steps in the running batch even when queue is empty.
		w.chargeRunningBatch()
		return
	}

	// Step 1: counter-lift for any new arrivals in the wait queue.
	for _, r := range requests {
		if _, seen := w.requestLift[r.ID]; !seen {
			// New entrant: lift = current virtual counter for this tenant.
			w.requestLift[r.ID] = w.tenantCounter[r.TenantID]
		}
		// Cache input token count for later prefill charging.
		// (RECON-1 fix: was r.InputTokens[0] which is the first token *value*,
		// not the length — a random integer in [0, MaxTokenID) per
		// GenerateRandomTokenIDs. The intended quantity is len(r.InputTokens),
		// the number of input tokens. The bug made VTC's prefill cost
		// effectively random and degenerated VTC's behavior toward FCFS.)
		if _, ok := w.requestInputLen[r.ID]; !ok && len(r.InputTokens) > 0 {
			w.requestInputLen[r.ID] = float64(len(r.InputTokens))
		}
	}

	// Step 2: detect newly-admitted requests (left queue, now in running batch) and charge prefill.
	w.chargeRunningBatch()

	// Step 3: sort wait queue ascending by (lift, arrival_time).
	sort.SliceStable(requests, func(i, j int) bool {
		li := w.requestLift[requests[i].ID]
		lj := w.requestLift[requests[j].ID]
		if li != lj {
			return li < lj
		}
		return requests[i].ArrivalTime < requests[j].ArrivalTime
	})
}

// chargeRunningBatch charges the virtual counter for requests currently running:
//   - Newly-admitted requests (first tick in running batch): charge w_p · |P_r|.
//   - Continuing decode requests (seen in previous tick): charge w_q.
func (w *WFQScheduler) chargeRunningBatch() {
	if w.simulator == nil || w.simulator.RunningBatch == nil {
		w.prevRunningIDs = make(map[string]bool)
		return
	}

	currRunningIDs := make(map[string]bool, len(w.simulator.RunningBatch.Requests))
	for _, r := range w.simulator.RunningBatch.Requests {
		currRunningIDs[r.ID] = true
	}

	for _, r := range w.simulator.RunningBatch.Requests {
		if !w.prevRunningIDs[r.ID] {
			// Newly admitted this tick: charge prefill cost.
			inputLen := w.requestInputLen[r.ID]
			if inputLen == 0 && len(r.InputTokens) > 0 {
				inputLen = float64(len(r.InputTokens)) // RECON-1 fix
				w.requestInputLen[r.ID] = inputLen
			}
			w.tenantCounter[r.TenantID] += w.wPrefill * inputLen
		} else {
			// Continuing decode step: charge w_q per step.
			w.tenantCounter[r.TenantID] += w.wDecode
		}
	}

	w.prevRunningIDs = currRunningIDs
}

// TenantCounters returns a snapshot of all virtual counters (for metrics output).
func (w *WFQScheduler) TenantCounters() map[string]float64 {
	out := make(map[string]float64, len(w.tenantCounter))
	for k, v := range w.tenantCounter {
		out[k] = v
	}
	return out
}
