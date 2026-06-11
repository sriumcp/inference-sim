// Package kvtime implements the KV-time entitlement scheduling framework
// described in the memorytime.tex campaign.
//
// This package provides:
//   - Meter: per-tenant memory-time accumulator (Riemann sum over resident KV blocks)
//   - BucketManager: per-tenant entitlement buckets (arrival-curve enforcement)
//   - GreedyKVScheduler: KV-time entitlement scheduler (cost-effectiveness dispatch)
//   - WFQScheduler: Token-WFQ / VTC baseline (Sheng et al. OSDI 2024)
//
// All code in this package is experiment-only; it lives in an isolated worktree
// and does NOT modify any production BLIS files.
package kvtime

import (
	"github.com/inference-sim/inference-sim/sim/kv"
)

// Meter accumulates per-tenant memory-time A_i(T) = ∫_0^T k_i(s) ds
// where k_i(s) is the total resident KV tokens belonging to tenant i at time s.
//
// Implementation uses a Riemann sum: at each scheduler tick, it samples the
// current per-tenant resident block counts, multiplies by Δt, and accumulates.
//
// Conservation invariant (strict per-tenant form, iter-1 apparatus gate):
//   Σ_tenant attributedTokens(tick) == kvCache.UsedBlocks() × kvCache.BlockSizeTokens
//
// The meter walks ALL entries in KVCacheState.RequestMap, not just the running
// batch — preempted/queued-with-blocks requests must also be attributed. A meter
// that only walks RunningBatch.Requests would undercount and fail the conservation
// check whenever preempted requests hold blocks.
type Meter struct {
	// tenantKVTime tracks accumulated memory-time per tenant (token·µs units).
	tenantKVTime map[string]float64

	// tenantBlocksLastTick stores per-tenant block count at the most recent tick.
	// Used as the "left endpoint" of the next Riemann rectangle.
	tenantBlocksLastTick map[string]float64

	// lastTickUs is the simulation clock at the most recent Tick call.
	lastTickUs int64

	// blockSizeTokens is the number of tokens per KV block (BlockSizeTokens from KVCacheState).
	blockSizeTokens int64

	// conservationViolations counts ticks where Σ_tenant ≠ UsedBlocks × BlockSize.
	conservationViolations int64

	// totalTicks counts all Tick calls (for reporting).
	totalTicks int64

	// perRequestResidency tracks C̃_r = accumulated token·µs per request (for externality rank).
	// keyed by RequestID.
	perRequestResidency map[string]float64
}

// NewMeter creates a Meter with the given block size.
// blockSizeTokens must match KVCacheState.BlockSizeTokens (16 for standard BLIS config).
func NewMeter(blockSizeTokens int64) *Meter {
	return &Meter{
		tenantKVTime:         make(map[string]float64),
		tenantBlocksLastTick: make(map[string]float64),
		perRequestResidency:  make(map[string]float64),
		blockSizeTokens:      blockSizeTokens,
	}
}

// Tick samples the current per-tenant resident block counts from the KV cache,
// computes A_i(Δt) = k_i × Δt for each tenant, and accumulates.
//
// reqToTenant maps RequestID → TenantID for ALL requests that may hold KV blocks.
// Requests with no TenantID in the map are skipped (should not happen in well-formed
// workloads; logged as an unattributed count for debugging).
//
// nowUs is the current simulation clock in microseconds.
func (m *Meter) Tick(kvCache *kv.KVCacheState, reqToTenant map[string]string, nowUs int64) {
	m.totalTicks++

	// Compute per-tenant block counts from KVCacheState.RequestMap.
	// Walk ALL entries — not just running batch.
	tenantBlocks := make(map[string]float64, 4)
	reqBlocks := make(map[string]float64, len(kvCache.RequestMap))
	unattributed := 0

	for reqID, blockIDs := range kvCache.RequestMap {
		tenant, ok := reqToTenant[reqID]
		if !ok || tenant == "" {
			unattributed++
			continue
		}
		blockCount := float64(len(blockIDs))
		tenantBlocks[tenant] += blockCount
		reqBlocks[reqID] += blockCount
	}
	_ = unattributed // available for debugging

	// Conservation check: Σ_tenant tenantBlocks == UsedBlocks.
	usedBlocks := kvCache.UsedBlocks()
	var totalAttributed float64
	for _, b := range tenantBlocks {
		totalAttributed += b
	}
	// Allow ±1 block tolerance for edge cases (block mid-allocation).
	if int64(totalAttributed+0.5) != usedBlocks {
		m.conservationViolations++
	}

	// Accumulate Riemann sum: A_i(Δt) = k_i(last) × Δt (left-endpoint rule).
	// On first tick (lastTickUs == 0), skip accumulation (no previous sample to form rectangle).
	if m.lastTickUs > 0 && nowUs > m.lastTickUs {
		deltaUs := float64(nowUs - m.lastTickUs)
		for tenant, blocks := range m.tenantBlocksLastTick {
			tokens := blocks * float64(m.blockSizeTokens)
			m.tenantKVTime[tenant] += tokens * deltaUs
		}
		// Per-request residency (for externality rank arm in iter-2).
		for reqID, blocks := range reqBlocks {
			tokens := blocks * float64(m.blockSizeTokens)
			m.perRequestResidency[reqID] += tokens * deltaUs
		}
	}

	// Update "last tick" state for next Riemann rectangle.
	m.tenantBlocksLastTick = tenantBlocks
	m.lastTickUs = nowUs
}

// TenantKVTime returns the accumulated A_i(T) for each tenant seen so far.
// Returns a snapshot copy (safe to read after simulation ends).
func (m *Meter) TenantKVTime() map[string]float64 {
	out := make(map[string]float64, len(m.tenantKVTime))
	for k, v := range m.tenantKVTime {
		out[k] = v
	}
	return out
}

// PerRequestResidency returns C̃_r for each request (token·µs of resident time).
func (m *Meter) PerRequestResidency() map[string]float64 {
	out := make(map[string]float64, len(m.perRequestResidency))
	for k, v := range m.perRequestResidency {
		out[k] = v
	}
	return out
}

// ConservationViolations returns the count of ticks where the strict per-tenant
// conservation invariant failed: Σ_tenant blocks ≠ UsedBlocks.
func (m *Meter) ConservationViolations() int64 { return m.conservationViolations }

// TotalTicks returns the number of Tick calls made.
func (m *Meter) TotalTicks() int64 { return m.totalTicks }

// TenantBlocksAtLastTick returns the per-tenant block count at the most recent tick.
// Used by the bucket manager and for the utilization apparatus gate.
func (m *Meter) TenantBlocksAtLastTick() map[string]float64 {
	out := make(map[string]float64, len(m.tenantBlocksLastTick))
	for k, v := range m.tenantBlocksLastTick {
		out[k] = v
	}
	return out
}
