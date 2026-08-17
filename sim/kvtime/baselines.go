// Package kvtime — Baseline scheduler implementations for the memorytime-mirage experiment (iter-2).
//
// This file provides five additional schedulers used as comparison baselines:
//   - FCFSScheduler: no-op FIFO (wraps sim.FCFSScheduler)
//   - DecodeTokenScheduler: Token-WFQ variant charging only decode steps (w_p=0, w_q=1)
//   - RequestRRScheduler: round-robin by completed-request count (not token cost)
//   - KVQuotaScheduler: hard instantaneous cap on per-tenant KV occupancy
//   - HOLWaitScheduler: dispatch tenant with longest head-of-line wait
//
// All schedulers implement sim.InstanceScheduler (OrderQueue interface).
// They live in this package alongside GreedyKVScheduler and WFQScheduler for
// comparison clarity; none modifies any production BLIS files.
package kvtime

import (
	"fmt"
	"sort"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
)

// ─── FCFSScheduler ────────────────────────────────────────────────────────────

// FCFSScheduler is a no-op FIFO scheduler.  It preserves the enqueue order of
// requests, which BLIS uses as arrival order within a simulation tick.
// Used as the incumbent baseline (real vLLM default).
type FCFSScheduler struct{}

// OrderQueue implements sim.InstanceScheduler.
func (f *FCFSScheduler) OrderQueue(_ []*sim.Request, _ int64) {
	// No-op: queue is already in FIFO order.
}

// ─── DecodeTokenScheduler ─────────────────────────────────────────────────────

// DecodeTokenScheduler is a Token-WFQ variant that only charges decode-step tokens
// (w_p=0, w_q=1).  Under D=1, every request is prefill-dominated, so this scheduler
// charges almost nothing per request and degenerates toward FIFO.
//
// This tests the hypothesis: "Token-WFQ's mirage persists even when the token cost
// function zeros out prefill weights" — showing the mirage is robust to cost-function
// parameterisation, not just the WFQ mechanism.
//
// Two-phase init: call SetSimulator after constructing the sim.Simulator.
type DecodeTokenScheduler struct {
	simulator *sim.Simulator

	tenantCounter   map[string]float64
	requestLift     map[string]float64
	prevRunningIDs  map[string]bool
	requestInputLen map[string]float64
}

// NewDecodeTokenScheduler creates a DecodeTokenScheduler (w_p=0, w_q=1).
func NewDecodeTokenScheduler() *DecodeTokenScheduler {
	return &DecodeTokenScheduler{
		tenantCounter:   make(map[string]float64),
		requestLift:     make(map[string]float64),
		prevRunningIDs:  make(map[string]bool),
		requestInputLen: make(map[string]float64),
	}
}

// SetSimulator wires the scheduler to the simulator after construction.
func (d *DecodeTokenScheduler) SetSimulator(simulator *sim.Simulator) {
	d.simulator = simulator
}

// OrderQueue implements sim.InstanceScheduler.
func (d *DecodeTokenScheduler) OrderQueue(requests []*sim.Request, _ int64) {
	if len(requests) == 0 {
		d.chargeRunningBatch()
		return
	}

	// Counter-lift for new arrivals.
	for _, r := range requests {
		if _, seen := d.requestLift[r.ID]; !seen {
			d.requestLift[r.ID] = d.tenantCounter[r.TenantID]
		}
		if _, ok := d.requestInputLen[r.ID]; !ok && len(r.InputTokens) > 0 {
			// RECON-1 fix: same bug as WFQScheduler (token-value vs token-count).
			// For DecodeTokenScheduler this field is dormant because w_p=0
			// (prefill not charged), but fix for hygiene.
			d.requestInputLen[r.ID] = float64(len(r.InputTokens))
		}
	}

	// Charge decode steps only (no prefill charge: w_p=0).
	d.chargeRunningBatch()

	// Sort ascending by (lift, arrival_time).
	sort.SliceStable(requests, func(i, j int) bool {
		li := d.requestLift[requests[i].ID]
		lj := d.requestLift[requests[j].ID]
		if li != lj {
			return li < lj
		}
		return requests[i].ArrivalTime < requests[j].ArrivalTime
	})
}

// chargeRunningBatch charges only decode steps (w_p=0, w_q=1).
func (d *DecodeTokenScheduler) chargeRunningBatch() {
	if d.simulator == nil || d.simulator.RunningBatch == nil {
		d.prevRunningIDs = make(map[string]bool)
		return
	}

	currRunningIDs := make(map[string]bool, len(d.simulator.RunningBatch.Requests))
	for _, r := range d.simulator.RunningBatch.Requests {
		currRunningIDs[r.ID] = true
	}

	for _, r := range d.simulator.RunningBatch.Requests {
		if d.prevRunningIDs[r.ID] {
			// Continuing decode step: charge w_q=1.
			d.tenantCounter[r.TenantID] += 1.0
		}
		// Newly admitted (first tick): charge nothing (w_p=0).
	}

	d.prevRunningIDs = currRunningIDs
}

// ─── RequestRRScheduler ───────────────────────────────────────────────────────

// RequestRRScheduler implements per-tenant round-robin ordered by completed-request
// count (not token cost).  The tenant with fewer completions dispatches first.
//
// Semantics: maintain a per-tenant completion counter C_i.  On each OrderQueue
// call, sort requests so that the tenant with lower C_i dispatches first.
// When a request completes, C_i is incremented (via SetCompletionCallback).
//
// This is the "raw fairness by headcount" baseline — it equalizes number of
// requests served, ignoring their cost.
type RequestRRScheduler struct {
	tenantCount map[string]int64
}

// NewRequestRRScheduler creates a RequestRRScheduler.
func NewRequestRRScheduler() *RequestRRScheduler {
	return &RequestRRScheduler{
		tenantCount: make(map[string]int64),
	}
}

// RecordCompletion increments the completion counter for a tenant.
// Call this from the OnRequestDone hook.
func (r *RequestRRScheduler) RecordCompletion(tenantID string) {
	r.tenantCount[tenantID]++
}

// OrderQueue implements sim.InstanceScheduler.
func (r *RequestRRScheduler) OrderQueue(requests []*sim.Request, _ int64) {
	if len(requests) == 0 {
		return
	}

	sort.SliceStable(requests, func(i, j int) bool {
		ci := r.tenantCount[requests[i].TenantID]
		cj := r.tenantCount[requests[j].TenantID]
		if ci != cj {
			return ci < cj // fewer completions dispatches first
		}
		return requests[i].ArrivalTime < requests[j].ArrivalTime
	})
}

// ─── KVQuotaScheduler ─────────────────────────────────────────────────────────

// KVQuotaScheduler enforces a hard instantaneous per-tenant KV quota:
//
//	k_i(t) ≤ ω_i · K
//
// When a tenant's current KV occupancy exceeds its quota, ALL of its requests
// are pushed to the back of the queue (they will not be scheduled this tick).
// Within-quota tenants use FCFS ordering.
//
// This is the "hard quota" baseline — it enforces a static share at each instant,
// without any bucket depth or temporal averaging.
//
// Two-phase init: call SetSimulator after constructing the sim.Simulator.
type KVQuotaScheduler struct {
	simulator     *sim.Simulator
	kvCache       *kv.KVCacheState
	tenantOmega   map[string]float64
	totalKVBlocks int64
	reqToTenant   map[string]string
}

// NewKVQuotaScheduler creates a KVQuotaScheduler.
//
//   - kvCache: the live KV cache state (for reading current occupancy).
//   - tenantOmega: per-tenant omega_i (entitlement share, fraction of K).
//   - totalKVBlocks: K (total block capacity).
func NewKVQuotaScheduler(kvCache *kv.KVCacheState, tenantOmega map[string]float64, totalKVBlocks int64) *KVQuotaScheduler {
	return &KVQuotaScheduler{
		kvCache:       kvCache,
		tenantOmega:   tenantOmega,
		totalKVBlocks: totalKVBlocks,
		reqToTenant:   make(map[string]string, 128),
	}
}

// SetSimulator wires the scheduler to the simulator after construction.
func (q *KVQuotaScheduler) SetSimulator(simulator *sim.Simulator) {
	q.simulator = simulator
}

// OrderQueue implements sim.InstanceScheduler.
func (q *KVQuotaScheduler) OrderQueue(requests []*sim.Request, _ int64) {
	if len(requests) == 0 {
		return
	}

	// Build reqToTenant from wait queue + running batch.
	for _, r := range requests {
		if r.TenantID != "" {
			q.reqToTenant[r.ID] = r.TenantID
		}
	}
	if q.simulator != nil && q.simulator.RunningBatch != nil {
		for _, r := range q.simulator.RunningBatch.Requests {
			if r.TenantID != "" {
				q.reqToTenant[r.ID] = r.TenantID
			}
		}
	}

	// Compute current per-tenant block occupancy from KV cache.
	tenantBlocks := make(map[string]int64)
	for reqID, blockIDs := range q.kvCache.RequestMap {
		tenant := q.reqToTenant[reqID]
		if tenant != "" {
			tenantBlocks[tenant] += int64(len(blockIDs))
		}
	}

	// Classify tenants as over-quota or within-quota.
	overQuota := make(map[string]bool)
	for tenant, blocks := range tenantBlocks {
		omega := q.tenantOmega[tenant]
		if omega <= 0 {
			omega = 0.5 // default equal share for 2 tenants
		}
		quota := int64(omega * float64(q.totalKVBlocks))
		if blocks > quota {
			overQuota[tenant] = true
		}
	}

	// Sort: within-quota requests first (FCFS within), over-quota requests last.
	sort.SliceStable(requests, func(i, j int) bool {
		oi := overQuota[requests[i].TenantID]
		oj := overQuota[requests[j].TenantID]
		if oi != oj {
			return !oi // within-quota (false) < over-quota (true)
		}
		return requests[i].ArrivalTime < requests[j].ArrivalTime
	})
}

// AllowAdmission implements sim.AdmissionAwareScheduler.
//
// HARD instantaneous cap: refuses admission when admitting this request would
// push the tenant's resident block count above ω_i · K. Required for
// kv-quota to actually enforce the instantaneous quota. Without this method,
// kv-quota's OrderQueue merely re-prioritizes; FormBatch's physical-capacity-
// only admission rule then admits over-quota requests anyway when K has free
// blocks (defeating the entire baseline). With this method, kv-quota becomes
// a true hard cap, and is the kv-time-bucket's β=0 corner case.
//
// Block estimate: the request's prefill block requirement,
// ⌈len(InputTokens) / blockSize⌉. Decode tokens add blocks during execution
// (typically one block per blockSize output tokens), but at admission time
// we use the prefill estimate — this matches what FormBatch.AllocateKVBlocks
// physically reserves at the moment of admission.
//
// Combined with OrderQueue's "over-quota tenants to back" sort, the
// FormBatch dequeue loop's break-on-veto contract is correct: when the
// peeked request would push the tenant over its ω·K cap, all admittable
// in-quota requests have already been peeked.
func (q *KVQuotaScheduler) AllowAdmission(req *sim.Request, _ int64) bool {
	if req == nil {
		return true
	}
	if req.TenantID == "" {
		return true // untenanted requests not subject to per-tenant gating
	}
	// Compute current per-tenant resident-block count by walking the live
	// runningBatch (which has Request objects with TenantID) and looking up
	// each request's blocks in kvCache.RequestMap. Walking the runningBatch
	// directly — rather than the OrderQueue-time reqToTenant cache — is
	// essential because FormBatch Phase 2 calls AllowAdmission once per
	// dequeued request, and each successful admission is appended to the
	// runningBatch BEFORE the next AllowAdmission call. The reqToTenant
	// cache, refreshed only once per OrderQueue tick, would miss those
	// newly-admitted entries and let the cap leak.
	var currentBlocks int64
	if q.simulator != nil && q.simulator.RunningBatch != nil {
		for _, r := range q.simulator.RunningBatch.Requests {
			if r != nil && r.TenantID == req.TenantID {
				currentBlocks += int64(len(q.kvCache.RequestMap[r.ID]))
			}
		}
	}
	// Estimate this request's prefill block requirement at admission time.
	blockSize := q.kvCache.BlockSizeTokens
	if blockSize <= 0 {
		blockSize = 16
	}
	newBlocks := (int64(len(req.InputTokens)) + blockSize - 1) / blockSize
	// Per-tenant cap.
	omega := q.tenantOmega[req.TenantID]
	if omega <= 0 {
		omega = 0.5
	}
	quota := int64(omega * float64(q.totalKVBlocks))
	return currentBlocks+newBlocks <= quota
}

// IsServeable implements sim.EnqueueValidatorScheduler (paper §22.3
// structural-serveability under sup-collapse meter).
//
// Paper §22.3 requires k_i(t) ≤ ω_i·K at all times. A request whose
// prefill block count alone exceeds ω_i·K cannot satisfy this invariant
// under any completion sequence — regardless of cache state, the request
// is permanently inadmissible. Returning false here drops the request at
// EnqueueRequest time (incrementing DroppedUnservable), avoiding the
// head-of-queue stall that would otherwise occur when AllowAdmission
// repeatedly vetoes such a request inside FormBatch's break-on-veto
// dequeue loop.
//
// Empirical demonstration: at K=2048, ω=0.45 (per-tenant cap = 921
// blocks), W4's lognormal-tail prompts can reach ≈982 blocks. Without
// this guard, a single such request lands at the head of the queue and
// blocks all subsequent admissions, producing a 105K rejection cycle and
// 164s wall time for a 30s sim. With this guard, oversized requests are
// dropped cleanly into DroppedUnservable; the rest of the workload runs
// at normal speed. See campaign RESEARCH_NOTES.md (RN-1) for full
// diagnostic.
//
// Paper-faithfulness: this is the strict reading of §22.3 ("can't fit
// within cap, can't serve"). KVtime (paper §6) handles the same
// situation via β > 0 burst credit — its AllowAdmission only checks
// IsOverdrawn(), not size-vs-cap, so a tenant with positive balance
// can admit an oversized request and repay over time. The contrast
// between kv-quota dropping these requests vs. KVtime serving them is
// precisely paper §6's argument for time-integrated entitlement over
// instantaneous caps.
func (q *KVQuotaScheduler) IsServeable(req *sim.Request) (bool, string) {
	if req == nil {
		return true, ""
	}
	if req.TenantID == "" {
		// Untenanted requests aren't subject to per-tenant caps.
		return true, ""
	}
	if q.kvCache == nil {
		return true, "" // can't validate without cache reference; defer to AllowAdmission
	}
	blockSize := q.kvCache.BlockSizeTokens
	if blockSize <= 0 {
		blockSize = 16
	}
	newBlocks := (int64(len(req.InputTokens)) + blockSize - 1) / blockSize
	omega := q.tenantOmega[req.TenantID]
	if omega <= 0 {
		omega = 0.5
	}
	quota := int64(omega * float64(q.totalKVBlocks))
	if newBlocks > quota {
		return false, fmt.Sprintf("kv-quota: prefill needs %d blocks but tenant %s cap is %d (ω=%.3f, K=%d)",
			newBlocks, req.TenantID, quota, omega, q.totalKVBlocks)
	}
	return true, ""
}

// ChooseVictims implements sim.PreemptionAwareScheduler (paper §22.3
// continuous-cap re-enforcement).
//
// Cap-restoration semantics: a tenant's resident-block count k_i(t) can drift
// above its cap ω_i·K between AllowAdmission ticks because decode growth adds
// blocks to already-running requests. ChooseVictims fires when a waiter
// arrives and the cache is full; it returns the FCFS-tails of any tenants
// currently above their cap, in latest-arrival-first order. FormBatch evicts
// incrementally and retries allocation, so it stops as soon as the candidate
// fits.
//
// Work-conservation invariants (paper §22.3, line 644 reading by analogy):
//   - The candidate's tenant going-over-cap-on-admission is handled by
//     AllowAdmission rejecting upstream — ChooseVictims never sees that case.
//   - Within-cap incumbents are NEVER evicted, even if their tenant has the
//     largest resident set. The cap is a per-tenant invariant, not a
//     priority signal.
//   - Eviction order within a violating tenant: latest arrival first
//     (most recently admitted, least KV invested) — same FCFS-tail
//     convention as BLIS default Phase 1.
//
// Note: untenanted requests in the running batch (TenantID == "") are ignored
// by both the cap check and victim selection. They're outside the per-tenant
// quota model.
func (q *KVQuotaScheduler) ChooseVictims(candidate *sim.Request,
	running []*sim.Request,
	_ int64) []int {
	if candidate == nil || len(running) == 0 || q.kvCache == nil {
		return nil
	}

	// Per-tenant resident block counts (current state, not post-admission).
	// Iteration order of `currentBlocks` is intentionally NOT used to produce
	// output — see overCap construction below.
	currentBlocks := make(map[string]int64)
	for _, r := range running {
		if r == nil || r.TenantID == "" {
			continue
		}
		currentBlocks[r.TenantID] += int64(len(q.kvCache.RequestMap[r.ID]))
	}

	// Identify over-cap tenants as a SET, not a list. Map iteration here
	// populates an unordered set membership table; the set is used only as
	// a lookup downstream, so iteration order does not leak into output.
	// (R2 / INV-6: any map iteration that determines output ordering must
	//  sort keys first; this construction sidesteps the rule by ensuring
	//  the map iteration's product is order-independent.)
	overCap := make(map[string]bool, len(currentBlocks))
	for tenant, blocks := range currentBlocks {
		omega := q.tenantOmega[tenant]
		if omega <= 0 {
			omega = 0.5 // matches AllowAdmission fallback
		}
		quota := int64(omega * float64(q.totalKVBlocks))
		if blocks > quota {
			overCap[tenant] = true
		}
	}
	if len(overCap) == 0 {
		// R19 note: no tenant is over its cap, so the cache is full of
		// within-cap residents. Returning nil signals FormBatch to break
		// the dequeue loop; the candidate waits for organic completion.
		// This is starvation-bounded by incumbent MaxOutputLen, not livelock.
		return nil
	}

	// Single deterministic pass over `running` (a slice — iteration order
	// is fixed by the simulator). For each over-cap tenant's request, record
	// the running-batch index and arrival time.
	type indexed struct {
		idx     int
		arrival int64
	}
	candidates := make([]indexed, 0)
	for i, r := range running {
		if r == nil {
			continue
		}
		if overCap[r.TenantID] {
			candidates = append(candidates, indexed{i, r.ArrivalTime})
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// FCFS-tail order: latest arrival first. Tiebreak on `idx` (deterministic
	// running-batch position) so that two requests admitted at the same tick
	// produce a stable ordering regardless of how they were collected.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].arrival != candidates[j].arrival {
			return candidates[i].arrival > candidates[j].arrival
		}
		return candidates[i].idx > candidates[j].idx
	})
	victims := make([]int, 0, len(candidates))
	for _, x := range candidates {
		victims = append(victims, x.idx)
	}
	return victims
}

// ─── HOLWaitScheduler ─────────────────────────────────────────────────────────

// HOLWaitScheduler dispatches requests from the tenant whose head-of-line (HOL)
// request has been waiting the longest (largest now - arrival_time).
//
// For each tenant, the "HOL wait" is the wait time of the oldest queued request.
// Tenants are ordered by HOL wait descending (longest wait first).
// Within the same HOL wait, fall back to arrival_time FCFS.
//
// This is the "maximize fairness by waiting time" baseline — it directly addresses
// head-of-line blocking by scheduling the most-starved tenant first.
type HOLWaitScheduler struct{}

// NewHOLWaitScheduler creates a HOLWaitScheduler.
func NewHOLWaitScheduler() *HOLWaitScheduler {
	return &HOLWaitScheduler{}
}

// OrderQueue implements sim.InstanceScheduler.
func (h *HOLWaitScheduler) OrderQueue(requests []*sim.Request, clock int64) {
	if len(requests) == 0 {
		return
	}

	// Find the oldest (max wait) request per tenant.
	tenantHOLArrival := make(map[string]int64)
	for _, r := range requests {
		if prev, seen := tenantHOLArrival[r.TenantID]; !seen || r.ArrivalTime < prev {
			tenantHOLArrival[r.TenantID] = r.ArrivalTime
		}
	}

	// HOL wait = clock - HOL arrival.  Larger wait → dispatches first.
	sort.SliceStable(requests, func(i, j int) bool {
		holI := clock - tenantHOLArrival[requests[i].TenantID]
		holJ := clock - tenantHOLArrival[requests[j].TenantID]
		if holI != holJ {
			return holI > holJ // longer wait dispatches first
		}
		return requests[i].ArrivalTime < requests[j].ArrivalTime
	})
}
