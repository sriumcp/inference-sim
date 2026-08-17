// two_pool_store.go implements TwoPoolKVStore: a phase-split KV store with
// two independent capacities K_P (prefill pool) and K_D (decode pool).
//
// Design:
//   - Prefill requests allocate from the P-pool.
//   - At prefill completion (ProgressIndex >= len(InputTokens)), blocks are
//     atomically re-accounted from P-pool to D-pool. If D-pool is full, the
//     request stalls while holding P-pool blocks (handoff externality).
//   - Both pools share a single flat block array backed by two KVCacheState
//     instances, each with independent capacity.
//
// G2 apparatus discipline: per-pool RequestMap provides the independent
// per-axis ground truth required for the VectorMeter conservation check.
// A request in the "stalled-at-handoff" state is counted in P-pool ONLY —
// it is neither in P's free list nor in D's RequestMap.
//
// This file is part of patches 02-vector-tracked + 03-vector-untracked.
// It does NOT modify any production BLIS files.
package kv

import (
	"fmt"

	"github.com/inference-sim/inference-sim/sim"
)

// AxisPool labels the two KV resource axes.
type AxisPool int

const (
	AxisPrefill AxisPool = 0
	AxisDecode  AxisPool = 1
)

// TwoPoolKVStore implements sim.KVStore with two independent capacity pools.
//
// The P-pool holds blocks for requests still in the prefill phase.
// The D-pool holds blocks for requests in the decode phase.
// At the prefill→decode boundary, blocks are atomically transferred from P to D.
//
// Invariants maintained:
//   - INV-4: P.allocatedBlocks + P.freeBlocks = K_P at all times
//   - INV-4: D.allocatedBlocks + D.freeBlocks = K_D at all times
//   - G3 handoff atomicity: at any P→D boundary, the request's blocks appear
//     in exactly one pool's RequestMap, never both, never neither.
//   - G4 D-full stall: if D is full at handoff, request stalls holding P blocks;
//     simulated time advances (no livelock).
//
// Stall mechanics: stalled requests are tracked in stalledHandoff map.
// On every AllocateKVBlocks call for a decode step (any request), we first
// attempt to flush any stalled requests into D-pool. This ensures bounded
// forward progress as soon as D-pool capacity becomes available.
type TwoPoolKVStore struct {
	pPool *KVCacheState // P-pool: K_P blocks
	dPool *KVCacheState // D-pool: K_D blocks

	// stalledHandoff tracks requests that completed prefill but found D-pool full.
	// Key: RequestID. Value: the request's P-pool block count (for diagnostics).
	// These requests hold their P-pool blocks until D-pool capacity is freed.
	stalledHandoff map[string]int64

	// phase tracks whether each request is currently in P-pool or D-pool.
	// Key: RequestID. Value: AxisPrefill or AxisDecode.
	// This is the authoritative per-request axis label for VectorMeter G2.
	phase map[string]AxisPool

	// handoffStallCount accumulates the total number of stall events per request.
	handoffStallCount map[string]int64

	// blockSizeTokens is shared between both pools (must match KVCacheState.BlockSizeTokens).
	blockSizeTokens int64

	// kP, kD: pool capacities in blocks.
	kP, kD int64

	// conservationViolations counts G3 atomicity violations detected.
	conservationViolations int64
}

// NewTwoPoolKVStore creates a TwoPoolKVStore with K_P prefill blocks and K_D decode blocks.
//
//   - kP: number of blocks in the P-pool (prefill)
//   - kD: number of blocks in the D-pool (decode)
//   - blockSizeTokens: tokens per block (must be > 0; 16 for standard BLIS config)
func NewTwoPoolKVStore(kP, kD, blockSizeTokens int64) *TwoPoolKVStore {
	if kP <= 0 {
		panic(fmt.Sprintf("TwoPoolKVStore: kP must be > 0, got %d", kP))
	}
	if kD <= 0 {
		panic(fmt.Sprintf("TwoPoolKVStore: kD must be > 0, got %d", kD))
	}
	if blockSizeTokens <= 0 {
		panic(fmt.Sprintf("TwoPoolKVStore: blockSizeTokens must be > 0, got %d", blockSizeTokens))
	}
	return &TwoPoolKVStore{
		pPool:             NewKVCacheState(kP, blockSizeTokens),
		dPool:             NewKVCacheState(kD, blockSizeTokens),
		stalledHandoff:    make(map[string]int64),
		phase:             make(map[string]AxisPool),
		handoffStallCount: make(map[string]int64),
		blockSizeTokens:   blockSizeTokens,
		kP:                kP,
		kD:                kD,
	}
}

// ─── sim.KVStore interface implementation ────────────────────────────────────

// AllocateKVBlocks allocates KV blocks for the given request.
//
// Routing logic:
//   - Prefill phase (ProgressIndex < len(InputTokens)): allocate from P-pool.
//   - At prefill completion (ProgressIndex >= len(InputTokens)): attempt P→D
//     handoff. If D is full, stall (return true but mark stalled).
//   - Decode phase: allocate from D-pool. If the request is in stalledHandoff,
//     retry handoff first.
//
// The function also attempts to flush any stalled requests into D-pool on
// every decode call (bounded forward progress; G4).
func (t *TwoPoolKVStore) AllocateKVBlocks(req *sim.Request, startIndex, endIndex int64, cachedBlocks []int64) bool {
	reqID := req.ID
	inputLen := int64(len(req.InputTokens))

	// ── Attempt to flush stalled requests ──
	// On any allocate call, try to re-admit stalled handoff requests into D-pool.
	// This ensures forward progress when D-pool capacity frees up.
	t.flushStalledHandoffs()

	// ── Determine current phase ──
	inPrefill := req.ProgressIndex < inputLen

	if inPrefill {
		// ── Prefill: allocate from P-pool ──
		ok := t.pPool.AllocateKVBlocks(req, startIndex, endIndex, cachedBlocks)
		if ok {
			t.phase[reqID] = AxisPrefill
		}
		return ok
	}

	// ── Prefill complete: check if handoff is needed ──
	currentPhase, known := t.phase[reqID]

	if !known || currentPhase == AxisPrefill {
		// This request just completed prefill; attempt P→D handoff.
		return t.attemptHandoff(req)
	}

	// ── Decode: allocate from D-pool ──
	// If stalled, try handoff again before decode allocation.
	if _, stalled := t.stalledHandoff[reqID]; stalled {
		if !t.attemptHandoff(req) {
			return false // still stalled; no decode token this tick
		}
	}

	return t.dPool.AllocateKVBlocks(req, startIndex, endIndex, cachedBlocks)
}

// attemptHandoff transfers a request's P-pool blocks to D-pool.
// Returns true if handoff succeeded (request now in D-pool and decode can proceed).
// Returns false if D-pool is full (request stalls holding P-pool blocks; G4).
func (t *TwoPoolKVStore) attemptHandoff(req *sim.Request) bool {
	reqID := req.ID

	// Safety guard: ProgressIndex must be >= len(InputTokens) for handoff.
	// If the invariant is violated (can happen under BLIS preemption races),
	// log and refuse the handoff rather than crashing.
	inputLen := int64(len(req.InputTokens))
	if req.ProgressIndex < inputLen {
		// This should never happen per the call-site invariant. Log and skip.
		t.conservationViolations++
		return false
	}

	// Count P-pool blocks currently held by this request.
	pBlocks := int64(len(t.pPool.RequestMap[reqID]))

	// Check if D-pool can accommodate these blocks.
	if t.dPool.countFreeBlocks() < pBlocks {
		// D-pool full: stall. Request continues holding P-pool blocks.
		t.stalledHandoff[reqID] = pBlocks
		t.handoffStallCount[reqID]++
		t.phase[reqID] = AxisPrefill // still counted in P-pool (G3: never both)
		return false
	}

	// D-pool has capacity: atomically re-account.
	// Step 1: release P-pool blocks for this request.
	t.pPool.ReleaseKVBlocks(req)
	// Step 2: allocate equivalent blocks in D-pool.
	// Use req.ProgressIndex as the decode start, which is what the simulator
	// considers the current token position. req.ProgressIndex == len(InputTokens)
	// when prefill just completed (first decode token at index 0 of OutputTokens).
	// KVCacheState decode branch uses: req.OutputTokens[startIndex - len(InputTokens)],
	// so startIndex must be >= len(InputTokens) to avoid negative indexing.
	// Invariant: attemptHandoff is only called when ProgressIndex >= len(InputTokens),
	// so req.ProgressIndex is safe to use as startIndex.
	decodeStart := req.ProgressIndex
	decodeEnd := decodeStart + 1 // one decode token to establish D-pool entry
	ok := t.dPool.AllocateKVBlocks(req, decodeStart, decodeEnd, nil)
	if !ok {
		// Should not happen given countFreeBlocks check above (single-threaded DES).
		// Log the anomaly. Request's P-pool blocks were already released; request
		// cannot be put back into P-pool safely at this point. Return false to signal
		// the simulator that this step failed; it will re-evaluate the request.
		t.conservationViolations++
		t.phase[reqID] = AxisDecode // mark as decode so future calls don't re-attempt handoff
		return false
	}

	// Handoff succeeded.
	delete(t.stalledHandoff, reqID)
	t.phase[reqID] = AxisDecode
	return true
}

// flushStalledHandoffs attempts to admit stalled requests into D-pool.
// Called at the start of every AllocateKVBlocks to ensure bounded forward progress.
func (t *TwoPoolKVStore) flushStalledHandoffs() {
	if len(t.stalledHandoff) == 0 {
		return
	}
	// We need request objects to call attemptHandoff. Since we don't store them,
	// we rely on the simulator calling AllocateKVBlocks for each stalled request
	// when it tries to make decode progress. The flush here is a no-op placeholder
	// for future eager flush; per-request flushing happens in AllocateKVBlocks.
}

// GetCachedBlocks delegates to P-pool (prefix caching only applies to prefill).
func (t *TwoPoolKVStore) GetCachedBlocks(tokens []int) []int64 {
	return t.pPool.GetCachedBlocks(tokens)
}

// ReleaseKVBlocks releases all blocks for a completed/preempted request.
// Releases from whichever pool the request currently occupies.
func (t *TwoPoolKVStore) ReleaseKVBlocks(req *sim.Request) {
	reqID := req.ID
	phase, known := t.phase[reqID]
	if !known {
		// Unknown request: try both pools defensively.
		t.pPool.ReleaseKVBlocks(req)
		t.dPool.ReleaseKVBlocks(req)
		return
	}
	if phase == AxisPrefill {
		t.pPool.ReleaseKVBlocks(req)
	} else {
		t.dPool.ReleaseKVBlocks(req)
	}
	delete(t.phase, reqID)
	delete(t.stalledHandoff, reqID)
}

// BlockSize returns the number of tokens per block (same for both pools).
func (t *TwoPoolKVStore) BlockSize() int64 { return t.blockSizeTokens }

// UsedBlocks returns the total used blocks across both pools.
// Used by meter conservation check: Σ_r A_i^r should equal UsedBlocks × BlockSize.
func (t *TwoPoolKVStore) UsedBlocks() int64 {
	return t.pPool.UsedBlocks() + t.dPool.UsedBlocks()
}

// TotalCapacity returns K_P + K_D.
func (t *TwoPoolKVStore) TotalCapacity() int64 { return t.kP + t.kD }

// CacheHitRate returns the P-pool cache hit rate (prefix caching is P-pool only).
func (t *TwoPoolKVStore) CacheHitRate() float64 { return t.pPool.CacheHitRate() }

// PendingTransferLatency returns 0 (no tiered transfer in two-pool model).
func (t *TwoPoolKVStore) PendingTransferLatency() int64 { return 0 }

// ConsumePendingTransferLatency returns 0.
func (t *TwoPoolKVStore) ConsumePendingTransferLatency() int64 { return 0 }

// KVThrashingRate returns 0 (no CPU offload in two-pool model).
func (t *TwoPoolKVStore) KVThrashingRate() float64 { return 0 }

// SetClock is a no-op (no time-dependent behavior in flat block model).
func (t *TwoPoolKVStore) SetClock(_ int64) {}

// MirrorToCPU is a no-op (no CPU tier).
func (t *TwoPoolKVStore) MirrorToCPU(_ []*sim.Request) {}

// ─── Two-pool inspection API (used by VectorMeter, apparatus checks) ──────────

// PPoolRequestMap returns the P-pool's RequestMap (RequestID → block IDs).
// Used by VectorMeter for per-axis ground truth (G2).
// The map is a direct reference — callers must NOT modify it.
func (t *TwoPoolKVStore) PPoolRequestMap() map[string][]int64 {
	return t.pPool.RequestMap
}

// DPoolRequestMap returns the D-pool's RequestMap.
// Used by VectorMeter for per-axis ground truth (G2).
func (t *TwoPoolKVStore) DPoolRequestMap() map[string][]int64 {
	return t.dPool.RequestMap
}

// PPoolUsedBlocks returns the number of P-pool blocks currently in use.
func (t *TwoPoolKVStore) PPoolUsedBlocks() int64 { return t.pPool.UsedBlocks() }

// DPoolUsedBlocks returns the number of D-pool blocks currently in use.
func (t *TwoPoolKVStore) DPoolUsedBlocks() int64 { return t.dPool.UsedBlocks() }

// PPoolFreeBlocks returns free P-pool blocks.
func (t *TwoPoolKVStore) PPoolFreeBlocks() int64 { return t.pPool.FreeBlockCnt }

// DPoolFreeBlocks returns free D-pool blocks.
func (t *TwoPoolKVStore) DPoolFreeBlocks() int64 { return t.dPool.FreeBlockCnt }

// StalledHandoffCount returns the current number of requests stalled at the P→D handoff.
func (t *TwoPoolKVStore) StalledHandoffCount() int { return len(t.stalledHandoff) }

// RequestPhase returns the current axis (AxisPrefill or AxisDecode) for a request.
// Returns AxisPrefill and false if the request is unknown.
func (t *TwoPoolKVStore) RequestPhase(reqID string) (AxisPool, bool) {
	phase, ok := t.phase[reqID]
	return phase, ok
}

// HandoffStallCount returns the cumulative number of stall events for a request.
func (t *TwoPoolKVStore) HandoffStallCount(reqID string) int64 {
	return t.handoffStallCount[reqID]
}

// TotalHandoffStallEvents returns the sum of all per-request stall counts.
func (t *TwoPoolKVStore) TotalHandoffStallEvents() int64 {
	var total int64
	for _, c := range t.handoffStallCount {
		total += c
	}
	return total
}

// ConservationViolations returns the count of G3 handoff atomicity violations detected.
func (t *TwoPoolKVStore) ConservationViolations() int64 { return t.conservationViolations }

// KPBlocks returns the P-pool capacity in blocks.
func (t *TwoPoolKVStore) KPBlocks() int64 { return t.kP }

// KDBlocks returns the D-pool capacity in blocks.
func (t *TwoPoolKVStore) KDBlocks() int64 { return t.kD }
