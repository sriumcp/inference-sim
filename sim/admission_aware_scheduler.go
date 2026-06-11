// admission_aware_scheduler.go defines an OPTIONAL extension to the
// InstanceScheduler interface that lets a scheduler refuse to admit specific
// requests to the running batch even when the KV cache has free physical
// space.
//
// Motivation
// ----------
// The default InstanceScheduler.OrderQueue contract only permits in-place
// reordering of the wait queue. FormBatch then dequeues from the front and
// admits whatever fits — physical capacity is the sole admission gate. That
// rule is correct for ordering-only schedulers (FCFS, SJF, priority) but
// degenerates to a no-op for schedulers that need to enforce per-tenant
// entitlement, quota, or budget constraints when the KV cache is not
// physically saturated. In that regime the wait queue is empty/singleton,
// and reordering an empty queue cannot affect outcomes.
//
// AdmissionAwareScheduler closes that gap. A scheduler that opts into this
// interface is consulted by FormBatch's dequeue loop for each peeked
// request; returning false leaves the request at the front of the wait
// queue for re-evaluation on the next tick (after scheduler state advances —
// e.g. bucket reconciliation in the GreedyKV case).
//
// Head-of-line blocking is avoided by the scheduler's ordering policy: a
// well-designed AdmissionAwareScheduler sorts admittable requests to the
// front of the queue and inadmissible ones (e.g. overdrawn tenants) to the
// back. When AllowAdmission first returns false, all admittable requests
// have already been peeked and admitted, so a `break` in the dequeue loop
// is correct.
//
// Compatibility: this interface is purely additive. Schedulers that do not
// implement it experience no behavior change — FormBatch falls back to its
// physical-capacity-only admission rule via a type assertion.
package sim

// AdmissionAwareScheduler extends InstanceScheduler with an admission veto.
type AdmissionAwareScheduler interface {
	InstanceScheduler

	// AllowAdmission reports whether the given request may be admitted to
	// the running batch this tick. False keeps the request at the head of
	// the wait queue for re-evaluation on the next tick.
	//
	// Implementations must be deterministic for a given (req, clock) pair
	// within a single FormBatch call so that the dequeue loop's break-on-
	// first-veto contract holds.
	AllowAdmission(req *Request, clock int64) bool
}

// PreemptionAwareScheduler extends AdmissionAwareScheduler with proactive
// scheduler-driven preemption — the eviction half of paper §6's unified
// admission/continuation/preemption rule.
//
// Motivation. AdmissionAwareScheduler can only veto admissions; it cannot
// make room. When the KV cache is full and a high-density waiting request
// arrives, ordering-only schedulers must wait for an organic completion to
// free space. Paper §6 (Remark, line 263) defines the entitlement
// scheduler over the candidate set R(t) = waiting ∪ resident ∪ resumable,
// scoring each by ŵ_r = Φ(B_τ(r)(t))·ρ_r(t) − R_r(t)·𝟏[r ∉ S^−(t)].
// "Scheduling and preemption solve the same optimization problem at
// different times."
//
// This interface exposes the eviction half. Schedulers that implement it
// are consulted by FormBatch's Phase 2 when AllocateKVBlocks fails for an
// admitted candidate. The scheduler returns a list of indices into
// runningBatch to evict (in order); FormBatch evicts them and retries
// allocation. An empty/nil return means "do not preempt for this
// candidate" (e.g., the candidate's score is below all incumbents').
//
// Compatibility: purely additive. Schedulers that do not implement it fall
// back to FormBatch's break-on-allocation-failure behavior.
type PreemptionAwareScheduler interface {
	AdmissionAwareScheduler

	// ChooseVictims returns running-batch indices to evict (in eviction
	// order) so that candidate can be admitted. Returning nil/empty means
	// no eviction is appropriate; FormBatch breaks the dequeue loop.
	//
	// The candidate has already passed AllowAdmission. running is a
	// snapshot of the running-batch slice; the scheduler must not retain
	// the slice or mutate it. Indices must be unique and in [0, len(running)).
	ChooseVictims(candidate *Request, running []*Request, clock int64) []int
}

// EnqueueValidatorScheduler extends InstanceScheduler with an enqueue-time
// serveability check, allowing a scheduler to permanently reject a request
// whose KV requirement structurally cannot be satisfied (e.g., a prompt whose
// block count exceeds a per-tenant cap that no completion can possibly free).
//
// This is distinct from AllowAdmission, which is a transient veto that keeps
// the request at the head of the wait queue for re-evaluation. A scheduler
// returning false from IsServeable is signaling PERMANENT inadmissibility —
// the request will never fit, regardless of cache state.
//
// Motivation. Paper §22.3's sup-collapse meter (kv-quota) requires
// k_i(t) ≤ ω_i·K at all times. A request whose prefill block count exceeds
// ω_i·K cannot satisfy the invariant under any completion sequence. With
// AllowAdmission returning false for such cases, the BLIS dequeue loop's
// break-on-veto contract creates a head-of-queue stall that blocks all
// subsequent admissions indefinitely. IsServeable lets the scheduler
// classify these requests as permanently unservable so EnqueueRequest
// can drop them at arrival via the existing DroppedUnservable accounting
// path (alongside MaxOutputLen, MaxModelLen, and KV-capacity guards).
//
// The reason string accompanies the drop log message for observability.
// Schedulers that have no structural-unservability concept (e.g. KVtime
// with β > 0 tolerates oversized requests via burst credit) simply do not
// implement this interface.
type EnqueueValidatorScheduler interface {
	InstanceScheduler

	// IsServeable reports whether the request can ever be admitted under
	// this scheduler's policy. Returning (false, reason) causes
	// EnqueueRequest to drop the request and increment DroppedUnservable.
	// Returning (true, _) admits the request to the wait queue normally.
	IsServeable(req *Request) (bool, string)
}
