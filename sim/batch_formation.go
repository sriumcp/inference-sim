package sim

import (
	"fmt"
	"math"

	"github.com/sirupsen/logrus"

	"github.com/inference-sim/inference-sim/sim/internal/util"
)

// BatchFormation encapsulates the batch composition strategy for a simulation step.
// Implementations handle KV allocation and preemption decisions internally
// but do NOT schedule events or record metrics — those are kernel concerns
// handled by the Simulator after FormBatch returns.
type BatchFormation interface {
	FormBatch(ctx BatchContext) BatchResult
}

// BatchContext provides the inputs for batch formation.
// The BatchFormation implementation may mutate WaitQ (dequeue/prepend) and
// KVCache (allocate/release) during FormBatch. ComputedTokens must be updated
// by the implementation: for each request that receives new tokens, set or
// increment ComputedTokens[req.ID] to the total computed tokens (including
// cached). Phase 2 of Step() reads this map to advance ProgressIndex.
type BatchContext struct {
	RunningBatch          *Batch
	WaitQ                 *WaitQueue
	KVCache               KVStore
	MaxScheduledTokens    int64
	MaxRunningReqs        int64
	PrefillTokenThreshold int64
	MaxModelLen           int64 // 0 = unlimited (proactive cap disabled)
	Now                   int64
	StepCount             int
	ComputedTokens        map[string]int64

	// CreditGate is an optional admission gate for enforcement policies.
	// Returns false when the request's tenant should not be scheduled (throttled).
	// When non-nil, Phase 2 stops dequeuing from the WaitQ head when the gate
	// returns false, relying on WaitQ ordering to place non-throttled requests first.
	// nil = no gate (all requests are schedulable by this check).
	CreditGate func(req *Request) bool

	// ThrottleNotify is called when Phase 2 encounters a request that fails the CreditGate.
	// Used to record throttle start time and update per-request tracking.
	// nil = no notification.
	ThrottleNotify func(req *Request, now int64)

	// Tracker exposes per-tenant externality state for EA-aware
	// preemption (both hard victim-selection and soft per-step decode
	// weighting). When nil, EA-aware policies degrade gracefully:
	//   - hard preemption: PreemptionEAAware falls back to PreemptionFCFS
	//     victim selection
	//   - soft preemption: per-tenant weighting becomes a no-op
	// Pre-existing PreemptionFCFS / PreemptionPriority paths ignore this
	// field, so adding it does not change their behavior.
	Tracker TenantExternalityTracker
}

// ScheduledRequest carries metadata about a newly scheduled request.
type ScheduledRequest struct {
	Request *Request
}

// PreemptedRequest carries metadata about a preempted request.
type PreemptedRequest struct {
	Request *Request
}

// BatchResult describes the outcome of batch formation.
type BatchResult struct {
	RunningBatch       *Batch
	NewlyScheduled     []ScheduledRequest
	Preempted          []PreemptedRequest
	PreemptionHappened bool
}

// PreemptionPolicy controls how preemption selects a victim from the running batch.
type PreemptionPolicy string

const (
	// PreemptionFCFS evicts the last request in the running batch (tail).
	// Matches vLLM's FCFS scheduling mode (self.running.pop()).
	PreemptionFCFS PreemptionPolicy = "fcfs"

	// PreemptionPriority evicts the least-urgent request based on Request.Priority (vLLM convention).
	// Selects max(Priority) with max(ArrivalTime) tiebreak — direct parity with
	// vLLM scheduler.py:1086: max(self.running, key=lambda r: (r.priority, r.arrival_time)).
	PreemptionPriority PreemptionPolicy = "priority"

	// PreemptionEAAware evicts the running request whose tenant has the
	// HIGHEST accumulated externality per alpha unit
	// (tenantCumExternality[tid] / alphaForSLO(slo_class)). This is the
	// hard-preemption analog of ea-wfq's queue ordering: the same
	// fairness signal that delays a tenant in the wait queue also chooses
	// it as the eviction victim when KV pressure forces a preemption.
	//
	// Composition with admission/scheduling:
	//   - Admission (EAAwareTokenBucket) bounds aggressor INJECTION rate.
	//   - Scheduling (ea-wfq) orders the wait queue so cooperators dispatch first.
	//   - Hard preemption (this) frees KV blocks held by aggressor decode
	//     when admission and scheduling didn't catch the request in time
	//     (e.g., aggressor admitted before kvUtil crossed 0.9).
	//
	// Falls back to PreemptionFCFS when BatchContext.Tracker is nil
	// (no externality state available). Documented degraded behavior; not
	// a panic — we want runs without a tracker to still complete.
	PreemptionEAAware PreemptionPolicy = "ea-aware"
)

// VLLMBatchFormation implements the vLLM FCFS + chunked-prefill + preemption strategy.
type VLLMBatchFormation struct {
	preemptionPolicy PreemptionPolicy

	// softPreemptionWeight controls EA-aware per-step decode-token
	// scaling. 0 (default) disables soft preemption — every running
	// request gets its natural share of the per-step token budget,
	// matching pre-existing behavior.
	//
	// When > 0, each request's per-step token allocation is multiplied
	// by 1 / (1 + softPreemptionWeight * cumExternality[tid] /
	// alphaForSLO(class)). High-externality tenants progress more
	// slowly per tick (without being evicted), freeing KV bandwidth for
	// cooperators. The mechanism complements hard preemption (eviction)
	// by acting at a finer time scale: hard preemption is binary, soft
	// preemption is continuous.
	softPreemptionWeight float64
}

func (v *VLLMBatchFormation) FormBatch(ctx BatchContext) BatchResult {
	if ctx.RunningBatch == nil {
		ctx.RunningBatch = &Batch{}
	}

	result := BatchResult{
		RunningBatch: ctx.RunningBatch,
	}

	tokenBudget := ctx.MaxScheduledTokens

	// Zero NumNewTokens for all running requests at the start of each scheduling pass.
	// This prevents stale values from the prior step from causing phantom budget
	// restoration when a request is preempted before being visited in this pass.
	for _, req := range result.RunningBatch.Requests {
		req.NumNewTokens = 0
	}

	// Phase 1: Process continuing requests (chunked prefill + decode).
	// Index-based loop: re-evaluates len() each iteration so evicted requests
	// are never visited. In priority mode, non-tail eviction shifts elements
	// left; the reqAdjustment returned by preemptForTokens compensates by
	// decrementing reqIndex, preventing element skipping.
	// Analog of vLLM v1 req_index -= 1 (scheduler.py:853).
	reqIndex := 0
	for reqIndex < len(result.RunningBatch.Requests) {
		if tokenBudget <= 0 {
			logrus.Warnf("[tick %07d] token budget exhausted, deferring remaining requests to next step", ctx.Now)
			break
		}
		req := result.RunningBatch.Requests[reqIndex]

		numNewTokens := util.Len64(req.InputTokens) - req.ProgressIndex
		// Chunked prefill for running requests
		if numNewTokens > 0 {
			if 0 < ctx.PrefillTokenThreshold && ctx.PrefillTokenThreshold < numNewTokens {
				numNewTokens = ctx.PrefillTokenThreshold
			}
			numNewTokens = min(numNewTokens, tokenBudget)
			// Proactive MaxModelLen cap (BC-1): match vLLM scheduler.py:773-774.
			// Note: the enqueue guard (len(InputTokens) < maxModelLen) guarantees
			// maxAllowed >= 1 during prefill, so this clamp only reduces chunk size,
			// never eliminates it. Defense-in-depth for bypass scenarios.
			if ctx.MaxModelLen > 0 {
				maxAllowed := max(ctx.MaxModelLen-1-req.ProgressIndex, 0)
				numNewTokens = min(numNewTokens, maxAllowed)
			}

			// Soft EA-aware preemption: scale per-tick allocation by
			// 1 / (1 + weight * cumExt / alpha). No-op when weight==0 or
			// tracker is nil. Floors at 1 to avoid starvation (R19).
			// Applied AFTER the chunk + budget + MaxModelLen caps so the
			// existing caps still bound the upper end.
			numNewTokens = v.applySoftPreemptionWeight(req, numNewTokens, ctx.Tracker)

			canSchedule, adj := v.preemptForTokens(req, numNewTokens, &result, ctx, &tokenBudget, reqIndex)
			reqIndex -= adj
			if !canSchedule {
				break
			}

			tokenBudget -= numNewTokens
			req.NumNewTokens = int(numNewTokens)
			ctx.ComputedTokens[req.ID] += numNewTokens
		}
		// Decode phase: allocate 1 token (or 0 if soft-preempted aggressively)
		if req.ProgressIndex >= util.Len64(req.InputTokens) && len(req.OutputTokens) > 0 {
			decodeTokens := int64(1)
			// Proactive MaxModelLen cap (BC-1): skip decode at boundary.
			// Equivalent to max(0, maxModelLen-1-PI) < 1, specialized for single-token decode.
			// Note: when decodeTokens=0, the request stays in RunningBatch with its
			// previously-allocated KV blocks for one zero-work step until processCompletions
			// releases them via ReleaseKVBlocks. Under tight KV pressure this transiently
			// reduces available blocks by the request's allocation.
			if ctx.MaxModelLen > 0 && req.ProgressIndex+decodeTokens > ctx.MaxModelLen-1 {
				decodeTokens = 0
			}
			// Soft preemption note (scope-limited):
			// The decode phase allocates exactly 1 token per step by
			// design. applySoftPreemptionWeight floors at 1, so it cannot
			// reduce decode allocation below the minimum without
			// stranding the request (R19). Soft-preempt during decode
			// would require per-request skip cadence (deterministic
			// step-skipping), which needs a new persistent counter on
			// Request. That plumbing is deferred — soft preemption acts
			// during PREFILL (chunked) where it has the largest effect:
			// an 8192-token aggressor's prefill chunks scale down,
			// stretching prefill duration, which keeps cache pressure
			// signaling longer and lets ea-wfq reorder cooperators ahead
			// more aggressively. Decode-skip is filed as a follow-up.
			if decodeTokens > 0 {
				canSchedule, adj := v.preemptForTokens(req, decodeTokens, &result, ctx, &tokenBudget, reqIndex)
				reqIndex -= adj
				if !canSchedule {
					break
				}
				tokenBudget--
				req.NumNewTokens = 1
				ctx.ComputedTokens[req.ID] += 1
			}
		}
		reqIndex++
	}

	// Phase 2: Dequeue new requests from wait queue
	for len(result.RunningBatch.Requests) < int(ctx.MaxRunningReqs) && ctx.WaitQ.Len() > 0 && tokenBudget > 0 && !result.PreemptionHappened {
		next := ctx.WaitQ.Peek()

		// Credit gate: stop dequeuing when the head request's tenant is throttled.
		// Relies on scheduleBatch having reordered the WaitQ so non-throttled requests
		// are at the front. When the head is throttled, all remaining requests are
		// also throttled (or the batch is full), so stopping is correct.
		if ctx.CreditGate != nil && !ctx.CreditGate(next) {
			if ctx.ThrottleNotify != nil {
				ctx.ThrottleNotify(next, ctx.Now)
			}
			break
		}

		// Handle decode-only requests (PD disaggregation: KV pre-allocated by transfer).
		// IsDecodeSubRequest is set exclusively by KVTransferStartedEvent when it
		// reserves KV on the decode pod (issue #1343), so this path fires only for
		// requests that genuinely arrived via PD KV transfer.
		// ProgressIndex has already been set to len(InputTokens) by ReserveTransferredKV.
		if next.IsDecodeSubRequest {
			decodeTokens := int64(1)
			if ok := ctx.KVCache.AllocateKVBlocks(next, next.ProgressIndex, next.ProgressIndex+decodeTokens, nil); !ok {
				break
			}
			ctx.WaitQ.DequeueBatch()
			result.RunningBatch.Requests = append(result.RunningBatch.Requests, next)
			next.ScheduledStepIdx = ctx.StepCount
			result.NewlyScheduled = append(result.NewlyScheduled, ScheduledRequest{Request: next})
			tokenBudget -= decodeTokens
			next.State = StateRunning
			next.NumNewTokens = 1
			ctx.ComputedTokens[next.ID] = next.ProgressIndex + decodeTokens
			continue
		}

		cachedBlocks := ctx.KVCache.GetCachedBlocks(next.InputTokens)
		numNewTokens := util.Len64(next.InputTokens) - util.Len64(cachedBlocks)*ctx.KVCache.BlockSize()

		if 0 < ctx.PrefillTokenThreshold && ctx.PrefillTokenThreshold < numNewTokens {
			numNewTokens = ctx.PrefillTokenThreshold
		}
		numNewTokens = min(numNewTokens, tokenBudget)
		startIndex := util.Len64(cachedBlocks) * ctx.KVCache.BlockSize()
		// Proactive MaxModelLen cap (BC-2): BLIS safety extension (vLLM only caps running requests).
		// For valid enqueued requests (input < maxModelLen), this is a no-op.
		if ctx.MaxModelLen > 0 {
			maxAllowed := max(ctx.MaxModelLen-1-startIndex, 0)
			numNewTokens = min(numNewTokens, maxAllowed)
		}
		endIndex := startIndex + numNewTokens

		if ok := ctx.KVCache.AllocateKVBlocks(next, startIndex, endIndex, cachedBlocks); !ok {
			break
		}

		ctx.WaitQ.DequeueBatch()
		result.RunningBatch.Requests = append(result.RunningBatch.Requests, next)
		next.ScheduledStepIdx = ctx.StepCount

		result.NewlyScheduled = append(result.NewlyScheduled, ScheduledRequest{
			Request: next,
		})

		tokenBudget -= numNewTokens
		next.State = StateRunning
		next.NumNewTokens = int(numNewTokens)
		ctx.ComputedTokens[next.ID] = numNewTokens + util.Len64(cachedBlocks)*ctx.KVCache.BlockSize()
	}

	return result
}

// preemptForTokens tries to allocate numNewTokens of KV blocks for req,
// evicting victims if needed. Returns (canSchedule, reqAdjustment) where
// reqAdjustment counts evictions at indices below reqIndex.
// The caller must apply reqIndex -= reqAdjustment after each call to prevent
// element skipping when non-tail removal shifts elements left.
// Analog of vLLM scheduler.py:853 (req_index -= 1).
func (v *VLLMBatchFormation) preemptForTokens(req *Request, numNewTokens int64, result *BatchResult, ctx BatchContext, tokenBudget *int64, reqIndex int) (bool, int) {
	adjustment := 0
	for {
		if ok := ctx.KVCache.AllocateKVBlocks(req, req.ProgressIndex, req.ProgressIndex+numNewTokens, nil); !ok {
			// Circuit breaker: empty batch means cache is too small (R19)
			if len(result.RunningBatch.Requests) == 0 {
				logrus.Warnf("[tick %07d] preemption: KV cache too small for request %s (need %d tokens, no running requests to evict)",
					ctx.Now, req.ID, numNewTokens)
				return false, adjustment
			}

			result.PreemptionHappened = true

			var victimIdx int
			switch v.preemptionPolicy {
			case PreemptionPriority:
				victimIdx = v.selectPriorityVictim(result.RunningBatch.Requests)
			case PreemptionEAAware:
				// Documented degradation: nil tracker → fall back to FCFS
				// rather than panicking. Lets runs complete even if the
				// tracker hasn't been wired (e.g., a test harness).
				if ctx.Tracker == nil {
					victimIdx = len(result.RunningBatch.Requests) - 1
				} else {
					victimIdx = v.selectEAAwareVictim(result.RunningBatch.Requests, ctx.Tracker)
				}
			default:
				victimIdx = len(result.RunningBatch.Requests) - 1
			}

			preemptedRequest := result.RunningBatch.Requests[victimIdx]
			logrus.Warnf("[tick %07d] preemption: evicting %s to make room", ctx.Now, preemptedRequest.ID)

			// Remove by index (supports non-tail eviction in priority mode).
			result.RunningBatch.Requests = append(
				result.RunningBatch.Requests[:victimIdx],
				result.RunningBatch.Requests[victimIdx+1:]...,
			)

			// Track Phase 1 index adjustment: if the victim was before the
			// caller's current position, elements shifted left under the cursor.
			// The caller must decrement reqIndex by the returned adjustment.
			// Analog of vLLM scheduler.py:853 (req_index -= 1 when preempted
			// request was in scheduled_running_reqs).
			// For FCFS, victimIdx is always the tail (>= reqIndex), so adjustment
			// is always 0 — preserving current FCFS behavior exactly.
			//
			// Divergence from vLLM: vLLM's req_index -= 1 is conditional on
			// preempted_req in scheduled_running_reqs. BLIS fires unconditionally
			// for any victim below reqIndex. This is correct because BLIS's Phase 1
			// loop can leave a MaxModelLen-capped request (decodeTokens=0) in the
			// batch below reqIndex without calling preemptForTokens. In vLLM, all
			// skip paths (lines 743/759/808) increment req_index before continue,
			// so unscheduled requests are never below req_index when preemption fires.
			if victimIdx < reqIndex-adjustment {
				adjustment++
			}

			result.Preempted = append(result.Preempted, PreemptedRequest{
				Request: preemptedRequest,
			})

			// Restore token budget if preempted request was already scheduled
			// in this step (visited earlier in Phase 1, NumNewTokens > 0).
			// Reachable in priority mode when victim was at index < reqIndex
			// (already visited and allocated tokens this step).
			// With FCFS (tail-only eviction), unreachable because evicted
			// requests are always unvisited (beyond reqIndex).
			if preemptedRequest.NumNewTokens > 0 {
				*tokenBudget += int64(preemptedRequest.NumNewTokens)
				preemptedRequest.NumNewTokens = 0
			}

			preemptedRequest.State = StateQueued
			preemptedRequest.ProgressIndex = 0
			preemptedRequest.ITL = nil
			preemptedRequest.TTFTSet = false // lets the !TTFTSet guard in executeBatchStep fire on re-prefill, updating FirstTokenTime (#1122)
			ctx.KVCache.ReleaseKVBlocks(preemptedRequest)
			delete(ctx.ComputedTokens, preemptedRequest.ID)
			ctx.WaitQ.PrependFront(preemptedRequest)

			if preemptedRequest == req {
				return false, adjustment
			}
		} else {
			return true, adjustment
		}
	}
}

// selectPriorityVictim returns the index of the least-urgent running request.
// Least urgent = highest Request.Priority value (vLLM convention: lower = more urgent).
// Ties broken by latest ArrivalTime (most recently arrived evicted first, least KV investment).
//
// Directly mirrors vLLM scheduler.py:1086-1089:
//
//	preempted_req = max(self.running, key=lambda r: (r.priority, r.arrival_time))
//
// Priority is set at instance entry by the pre-processor (EnqueueRequest/EnqueueDecodeSubRequest)
// via SLOPriorityMap.InvertForVLLM — not read from SLOClass here.
func (v *VLLMBatchFormation) selectPriorityVictim(requests []*Request) int {
	victimIdx := len(requests) - 1
	victimPri := requests[victimIdx].Priority
	victimArrival := requests[victimIdx].ArrivalTime

	for i := len(requests) - 2; i >= 0; i-- {
		pri := requests[i].Priority
		if pri > victimPri || (pri == victimPri && requests[i].ArrivalTime > victimArrival) {
			victimIdx = i
			victimPri = pri
			victimArrival = requests[i].ArrivalTime
		}
	}
	return victimIdx
}

// selectEAAwareVictim returns the index of the running request whose
// tenant has the HIGHEST accumulated externality per alpha unit. Same
// fairness signal as ea-wfq's queue reorder, used for victim selection
// during preemption.
//
// On ties (e.g., two requests from the same tenant), prefers the one
// with the LATEST ArrivalTime — same tiebreak as PreemptionPriority and
// vLLM scheduler.py:1086. Latest-arrival tiebreak preserves the more-
// invested earlier requests when possible.
//
// Caller guarantees tracker != nil (the FCFS fallback for nil tracker
// happens at the call site).
func (v *VLLMBatchFormation) selectEAAwareVictim(requests []*Request, tracker TenantExternalityTracker) int {
	victimIdx := 0
	victimScore := tracker.CumExternality(requests[0].TenantID) / alphaForSLO(requests[0].SLOClass)
	victimArrival := requests[0].ArrivalTime

	for i := 1; i < len(requests); i++ {
		score := tracker.CumExternality(requests[i].TenantID) / alphaForSLO(requests[i].SLOClass)
		if score > victimScore || (score == victimScore && requests[i].ArrivalTime > victimArrival) {
			victimIdx = i
			victimScore = score
			victimArrival = requests[i].ArrivalTime
		}
	}
	return victimIdx
}

// applySoftPreemptionWeight scales `numNewTokens` for a request based on
// its tenant's accumulated externality. Returns the scaled value, never
// less than 1 (R19 circuit breaker — every running request must make at
// least 1 token of progress per step to avoid starvation).
//
// When softPreemptionWeight == 0 OR tracker is nil OR cumExternality
// is 0, returns numNewTokens unchanged. This makes the call cheap to
// invoke unconditionally from the FormBatch loop.
//
// Why floor at 1: zero-progress in decode strands a request in the
// running batch holding KV blocks indefinitely. The fairness penalty is
// achieved by SLOWING aggressors, not stranding them.
func (v *VLLMBatchFormation) applySoftPreemptionWeight(req *Request, numNewTokens int64, tracker TenantExternalityTracker) int64 {
	if v.softPreemptionWeight == 0 || tracker == nil {
		return numNewTokens
	}
	cumExt := tracker.CumExternality(req.TenantID)
	if cumExt == 0 {
		return numNewTokens
	}
	alpha := alphaForSLO(req.SLOClass)
	if alpha == 0 {
		// Defensive: alphaForSLO always returns ≥ 1.0 for known classes
		// and 1.0 default. A zero alpha would imply division by zero;
		// fall back to passthrough rather than NaN.
		return numNewTokens
	}
	weight := 1.0 / (1.0 + v.softPreemptionWeight*cumExt/alpha)
	scaled := int64(math.Floor(float64(numNewTokens) * weight))
	if scaled < 1 {
		return 1
	}
	return scaled
}

// NewBatchFormation creates the default BatchFormation.
//
// preemptionPolicy selects HARD victim strategy:
//   - "" or "fcfs": tail-of-batch (vLLM default)
//   - "priority": least-urgent SLO tier (vLLM priority-mode parity)
//   - "ea-aware": tenant with highest cumExternality / alpha
//
// softPreemptionWeight controls per-step decode-token scaling for
// running requests. 0 (default) disables soft preemption (current
// behavior). Positive values slow high-externality tenants
// proportionally — see VLLMBatchFormation.softPreemptionWeight.
//
// Validation per R3:
//   - softPreemptionWeight must be a finite value >= 0. Negative weights
//     would amplify high-externality tenants (the inverse of the intended
//     fairness behavior); we panic at construction rather than allow it.
//
// In "priority" and "ea-aware" modes, victim selection reads runtime
// state (Request.Priority for priority; BatchContext.Tracker for
// ea-aware). No sloMap argument is needed here — alpha lookups go
// through alphaForSLO directly.
func NewBatchFormation(preemptionPolicy string, softPreemptionWeight float64) BatchFormation {
	if math.IsNaN(softPreemptionWeight) || math.IsInf(softPreemptionWeight, 0) || softPreemptionWeight < 0 {
		panic(fmt.Sprintf("NewBatchFormation: softPreemptionWeight must be a finite value >= 0, got %v", softPreemptionWeight))
	}
	policy := PreemptionPolicy(preemptionPolicy)
	if policy == "" {
		policy = PreemptionFCFS
	}
	return &VLLMBatchFormation{
		preemptionPolicy:     policy,
		softPreemptionWeight: softPreemptionWeight,
	}
}
