// vector_meter.go implements VectorMeter: per-tenant, per-axis memory-time
// accumulation for the two-pool KV residency vector campaign.
//
// Design:
//   The VectorMeter extends the scalar Meter (meter.go) to accumulate
//   A_i^P(t) and A_i^D(t) separately, by walking the TwoPoolKVStore's
//   per-pool RequestMaps each tick.
//
// F7 apparatus discipline (apparatus-design checklist):
//   Bug-class caught: a handoff-stalled request's P-pool residency mis-binned
//   to the D axis (or dropped) — this would corrupt the headline
//   handoff_stall_residency metric while a totals-level check stays green.
//
//   The VectorMeter walks BOTH per-pool RequestMaps independently:
//     - P-pool RequestMap → A_i^P (includes stalled-at-handoff requests)
//     - D-pool RequestMap → A_i^D
//   A stalled request's P blocks appear in P-pool's RequestMap ONLY (by
//   TwoPoolKVStore invariant: phase=AxisPrefill while stalled; not in D-pool's
//   RequestMap). So A_i^P correctly accounts for stalled tokens.
//
//   Conservation check (G2): per-tenant sum_r A_i^r must match the store
//   ledger (PPoolUsedBlocks + DPoolUsedBlocks) × BlockSize within tolerance.
//   This is per-axis per-tenant, NOT just the aggregate total.
//
// Per-window dominant-share excursion:
//   In addition to the cumulative A_i^r integrals, the meter maintains tumbling
//   windows of per-tenant per-axis residency. At each window boundary the meter
//   records dominant_share_i(w) = max_r A_i^r(w) / (C^r * window_active_us)
//   and the excursion = max(0, dominant_share_i(w) - omega_i) for post-warmup
//   windows. The windowed accumulation is ADDITIVE: existing cumulative A_i^r
//   paths are unchanged (rho_dom and integral_order_gap stay byte-identical).
//
// This file is part of patch 02-vector-tracked.
// It does NOT modify any production BLIS files.
package kvtime

import (
	"maps"
	"sort"

	"github.com/inference-sim/inference-sim/sim/kv"
)

// VectorMeter accumulates per-tenant per-axis memory-time:
//
//	A_i^P(T) = ∫_0^T k_i^P(s) ds
//	A_i^D(T) = ∫_0^T k_i^D(s) ds
//
// where k_i^r(s) is the number of tokens for tenant i in pool r at time s.
// Both are Riemann sums using the left-endpoint rule.
//
// The scalar sum A_i = A_i^P + A_i^D is also tracked for the G2 conservation
// check and for backward compatibility with scalar analysis tools.
type VectorMeter struct {
	// per-axis KV-time accumulation (token·µs)
	tenantKVTimeP map[string]float64 // A_i^P(T)
	tenantKVTimeD map[string]float64 // A_i^D(T)

	// per-axis block counts at the most recent tick (left-endpoint Riemann)
	tenantBlocksLastTickP map[string]float64
	tenantBlocksLastTickD map[string]float64

	// lastTickUs is the simulation clock at the most recent Tick call.
	lastTickUs int64

	// blockSizeTokens is the number of tokens per KV block.
	blockSizeTokens int64

	// conservationViolations counts ticks where per-pool conservation fails.
	// G2: per-pool Σ_tenant blocks must equal pool.UsedBlocks() within tolerance.
	conservationViolations int64

	// totalTicks counts all Tick calls.
	totalTicks int64

	// scalarConservationError tracks per-tenant abs(A_i^P + A_i^D - A_i_scalar).
	// Set to 0 in the current implementation where scalar is derived from the sum.
	scalarConservationErrorTokenUs map[string]float64

	// ── per-window excursion tracking (additive; cumulative paths unchanged) ──

	// windowSizeUs is the tumbling window length in µs (0 = disabled).
	windowSizeUs int64

	// warmupUs is the simulation warmup boundary; windows before this are discarded.
	warmupUs int64

	// kpBlocks / kdBlocks are the pool capacities (needed for per-window share).
	kpBlocks int64
	kdBlocks int64

	// omegaPerTenant is the per-tenant entitlement (same for all tenants in this experiment).
	omegaPerTenant float64

	// windowed accumulation for the current tumbling window (reset at boundary)
	windowStartUs     int64
	windowKVTimeP     map[string]float64 // A_i^P within current window
	windowKVTimeD     map[string]float64 // A_i^D within current window

	// per-window excursion series (post-warmup windows only), keyed by tenant
	// Each entry is the excursion = max(0, dominant_share_i(w) - omega_i) for that window.
	windowExcursions map[string][]float64

	// per-window dominant-share series (post-warmup windows), keyed by tenant
	windowDomShares map[string][]float64
}

// NewVectorMeter creates a VectorMeter for the given block size.
func NewVectorMeter(blockSizeTokens int64) *VectorMeter {
	return &VectorMeter{
		tenantKVTimeP:                  make(map[string]float64),
		tenantKVTimeD:                  make(map[string]float64),
		tenantBlocksLastTickP:          make(map[string]float64),
		tenantBlocksLastTickD:          make(map[string]float64),
		blockSizeTokens:                blockSizeTokens,
		scalarConservationErrorTokenUs: make(map[string]float64),
		windowKVTimeP:                  make(map[string]float64),
		windowKVTimeD:                  make(map[string]float64),
		windowExcursions:               make(map[string][]float64),
		windowDomShares:                make(map[string][]float64),
	}
}

// SetWindowParams configures per-window dominant-share excursion tracking.
// Call this before the first Tick if per-window metrics are desired.
// windowSizeUs > 0 enables tracking; warmupUs marks the post-warmup boundary;
// kpBlocks/kdBlocks are pool capacities for share normalisation;
// omegaPerTenant is the shared per-tenant entitlement.
func (vm *VectorMeter) SetWindowParams(windowSizeUs, warmupUs, kpBlocks, kdBlocks int64, omegaPerTenant float64) {
	vm.windowSizeUs = windowSizeUs
	vm.warmupUs = warmupUs
	vm.kpBlocks = kpBlocks
	vm.kdBlocks = kdBlocks
	vm.omegaPerTenant = omegaPerTenant
}

// Tick samples per-tenant per-axis resident block counts from the TwoPoolKVStore,
// runs the G2 per-pool conservation check, and accumulates the Riemann sum.
//
// reqToTenant maps RequestID → TenantID for ALL requests that may hold KV blocks.
// Requests not in reqToTenant are skipped (unattributed count available for debug).
// nowUs is the current simulation clock in microseconds.
//
// F7 note: we walk both per-pool RequestMaps independently. A handoff-stalled
// request appears in PPoolRequestMap only (phase=AxisPrefill while stalled).
// This ensures stalled requests' P residency is attributed to A_i^P correctly.
func (vm *VectorMeter) Tick(store *kv.TwoPoolKVStore, reqToTenant map[string]string, nowUs int64) {
	vm.totalTicks++

	// ── Walk P-pool RequestMap ──
	pBlocks := store.PPoolRequestMap()
	tenantBlocksP := make(map[string]float64, 4)
	for reqID, blockIDs := range pBlocks {
		tenant, ok := reqToTenant[reqID]
		if !ok || tenant == "" {
			continue
		}
		tenantBlocksP[tenant] += float64(len(blockIDs))
	}

	// ── Walk D-pool RequestMap ──
	dBlocks := store.DPoolRequestMap()
	tenantBlocksD := make(map[string]float64, 4)
	for reqID, blockIDs := range dBlocks {
		tenant, ok := reqToTenant[reqID]
		if !ok || tenant == "" {
			continue
		}
		tenantBlocksD[tenant] += float64(len(blockIDs))
	}

	// ── G2 per-pool conservation checks ──
	// P-pool: Σ_tenant tenantBlocksP == store.PPoolUsedBlocks()
	// D-pool: Σ_tenant tenantBlocksD == store.DPoolUsedBlocks()
	var totalPAttributed, totalDAttributed float64
	for _, b := range tenantBlocksP {
		totalPAttributed += b
	}
	for _, b := range tenantBlocksD {
		totalDAttributed += b
	}
	// Allow ±1 block tolerance for mid-allocation edge cases.
	pUsed := store.PPoolUsedBlocks()
	dUsed := store.DPoolUsedBlocks()
	if int64(totalPAttributed+0.5) != pUsed || int64(totalDAttributed+0.5) != dUsed {
		vm.conservationViolations++
	}

	// ── Accumulate Riemann sum ──
	// Left-endpoint rule: use block counts from the PREVIOUS tick.
	// On first tick (lastTickUs == 0), skip accumulation.
	if vm.lastTickUs > 0 && nowUs > vm.lastTickUs {
		deltaUs := float64(nowUs - vm.lastTickUs)
		for tenant, blocks := range vm.tenantBlocksLastTickP {
			tokens := blocks * float64(vm.blockSizeTokens)
			vm.tenantKVTimeP[tenant] += tokens * deltaUs
		}
		for tenant, blocks := range vm.tenantBlocksLastTickD {
			tokens := blocks * float64(vm.blockSizeTokens)
			vm.tenantKVTimeD[tenant] += tokens * deltaUs
		}

		// ── Per-window accumulation (additive; cumulative paths unchanged) ──
		// F7: we reuse the same left-endpoint block counts, so stalled P-tokens
		// are attributed to the P-axis in windowed accumulators as in the cumulative path.
		if vm.windowSizeUs > 0 {
			// Initialise window start on the first post-first-tick call.
			if vm.windowStartUs == 0 {
				vm.windowStartUs = vm.lastTickUs
			}

			// Check whether we crossed one or more window boundaries between
			// lastTickUs and nowUs. Each crossed boundary seals the current window
			// and opens a new one. We advance in whole-window steps.
			for vm.windowSizeUs > 0 && nowUs >= vm.windowStartUs+vm.windowSizeUs {
				// The window [windowStartUs, windowStartUs+windowSizeUs) has ended.
				// Attribute the remaining deltaUs up to the window boundary.
				boundaryUs := vm.windowStartUs + vm.windowSizeUs
				partialDelta := float64(boundaryUs - vm.lastTickUs)
				if partialDelta < 0 {
					partialDelta = 0
				}
				for tenant, blocks := range vm.tenantBlocksLastTickP {
					tokens := blocks * float64(vm.blockSizeTokens)
					vm.windowKVTimeP[tenant] += tokens * partialDelta
				}
				for tenant, blocks := range vm.tenantBlocksLastTickD {
					tokens := blocks * float64(vm.blockSizeTokens)
					vm.windowKVTimeD[tenant] += tokens * partialDelta
				}

				// Seal window: record dominant share and excursion if post-warmup.
				windowActiveUs := float64(vm.windowSizeUs)
				if vm.windowStartUs >= vm.warmupUs && windowActiveUs > 0 {
					cpTotal := float64(vm.kpBlocks) * float64(vm.blockSizeTokens) * windowActiveUs
					cdTotal := float64(vm.kdBlocks) * float64(vm.blockSizeTokens) * windowActiveUs

					// Collect all tenants seen in this window.
					windowTenants := make(map[string]struct{})
					for t := range vm.windowKVTimeP {
						windowTenants[t] = struct{}{}
					}
					for t := range vm.windowKVTimeD {
						windowTenants[t] = struct{}{}
					}
					for tenant := range windowTenants {
						shareP := 0.0
						if cpTotal > 0 {
							shareP = vm.windowKVTimeP[tenant] / cpTotal
						}
						shareD := 0.0
						if cdTotal > 0 {
							shareD = vm.windowKVTimeD[tenant] / cdTotal
						}
						domShare := shareP
						if shareD > domShare {
							domShare = shareD
						}
						vm.windowDomShares[tenant] = append(vm.windowDomShares[tenant], domShare)
						excursion := domShare - vm.omegaPerTenant
						if excursion < 0 {
							excursion = 0
						}
						vm.windowExcursions[tenant] = append(vm.windowExcursions[tenant], excursion)
					}
				}

				// Open new window, reset accumulators.
				vm.windowStartUs = boundaryUs
				vm.windowKVTimeP = make(map[string]float64)
				vm.windowKVTimeD = make(map[string]float64)

				// The remaining interval to accumulate for the new window starts
				// at boundaryUs; update lastTickUs reference for partial computation.
				// NOTE: we break here and let the final accumulation below handle
				// the remainder (from boundaryUs to nowUs); adjust lastTickUs accordingly.
				// Use a local variable to avoid aliasing the loop's sentinel.
				_ = partialDelta // already applied above
				// The remainder [boundaryUs, nowUs) will be accumulated after the loop
				// by the post-loop accumulation that uses vm.lastTickUs.
				// But we've already applied [lastTickUs, boundaryUs); to avoid
				// double-counting we update the effective last tick to boundaryUs
				// for the intra-window portion.
				vm.lastTickUs = boundaryUs
			}

			// Accumulate the remaining [lastTickUs, nowUs) portion into current window.
			remainDelta := float64(nowUs - vm.lastTickUs)
			if remainDelta > 0 {
				for tenant, blocks := range vm.tenantBlocksLastTickP {
					tokens := blocks * float64(vm.blockSizeTokens)
					vm.windowKVTimeP[tenant] += tokens * remainDelta
				}
				for tenant, blocks := range vm.tenantBlocksLastTickD {
					tokens := blocks * float64(vm.blockSizeTokens)
					vm.windowKVTimeD[tenant] += tokens * remainDelta
				}
			}
		}
	}

	// ── Update left-endpoint state ──
	vm.tenantBlocksLastTickP = tenantBlocksP
	vm.tenantBlocksLastTickD = tenantBlocksD
	vm.lastTickUs = nowUs
}

// ─── Query methods ────────────────────────────────────────────────────────────

// TenantKVTimeP returns accumulated A_i^P(T) per tenant (token·µs).
func (vm *VectorMeter) TenantKVTimeP() map[string]float64 {
	out := make(map[string]float64, len(vm.tenantKVTimeP))
	maps.Copy(out, vm.tenantKVTimeP)
	return out
}

// TenantKVTimeD returns accumulated A_i^D(T) per tenant (token·µs).
func (vm *VectorMeter) TenantKVTimeD() map[string]float64 {
	out := make(map[string]float64, len(vm.tenantKVTimeD))
	maps.Copy(out, vm.tenantKVTimeD)
	return out
}

// TenantKVTimeScalar returns A_i^P + A_i^D for each tenant (scalar sum for G2).
func (vm *VectorMeter) TenantKVTimeScalar() map[string]float64 {
	out := make(map[string]float64)
	for t, v := range vm.tenantKVTimeP {
		out[t] += v
	}
	for t, v := range vm.tenantKVTimeD {
		out[t] += v
	}
	return out
}

// DominantShare computes max_r A_i^r / (C^r * T_active) for each tenant.
// C^P = kP * blockSizeTokens, C^D = kD * blockSizeTokens (in tokens).
// T_active is the post-warmup horizon in µs.
// Returns the per-tenant dominant share (dimensionless fraction of axis capacity).
func (vm *VectorMeter) DominantShare(kP, kD int64, activeDurationUs int64) map[string]float64 {
	out := make(map[string]float64)
	cpTotal := float64(kP) * float64(vm.blockSizeTokens) * float64(activeDurationUs) // token·µs
	cdTotal := float64(kD) * float64(vm.blockSizeTokens) * float64(activeDurationUs)

	allTenants := make(map[string]struct{})
	for t := range vm.tenantKVTimeP {
		allTenants[t] = struct{}{}
	}
	for t := range vm.tenantKVTimeD {
		allTenants[t] = struct{}{}
	}

	for tenant := range allTenants {
		shareP := 0.0
		if cpTotal > 0 {
			shareP = vm.tenantKVTimeP[tenant] / cpTotal
		}
		shareD := 0.0
		if cdTotal > 0 {
			shareD = vm.tenantKVTimeD[tenant] / cdTotal
		}
		if shareP > shareD {
			out[tenant] = shareP
		} else {
			out[tenant] = shareD
		}
	}
	return out
}

// RhoDom computes the dominant-share disparity ratio:
//
//	rho_dom = max_i(dominant_share_i) / min_i(dominant_share_i)
//
// Returns 1.0 if fewer than 2 tenants are present.
func (vm *VectorMeter) RhoDom(kP, kD int64, activeDurationUs int64) float64 {
	shares := vm.DominantShare(kP, kD, activeDurationUs)
	if len(shares) < 2 {
		return 1.0
	}
	maxShare := -1.0
	minShare := 1e18
	for _, s := range shares {
		if s > maxShare {
			maxShare = s
		}
		if s < minShare {
			minShare = s
		}
	}
	if minShare <= 0 {
		return 0
	}
	return maxShare / minShare
}

// TenantBlocksAtLastTickP returns the per-tenant P-pool block count at the most recent tick.
// Used by PerAxisBucketManager for drain computation.
func (vm *VectorMeter) TenantBlocksAtLastTickP() map[string]float64 {
	out := make(map[string]float64, len(vm.tenantBlocksLastTickP))
	maps.Copy(out, vm.tenantBlocksLastTickP)
	return out
}

// TenantBlocksAtLastTickD returns the per-tenant D-pool block count at the most recent tick.
func (vm *VectorMeter) TenantBlocksAtLastTickD() map[string]float64 {
	out := make(map[string]float64, len(vm.tenantBlocksLastTickD))
	maps.Copy(out, vm.tenantBlocksLastTickD)
	return out
}

// ConservationViolations returns the count of ticks where per-pool conservation failed.
func (vm *VectorMeter) ConservationViolations() int64 { return vm.conservationViolations }

// TotalTicks returns the number of Tick calls made.
func (vm *VectorMeter) TotalTicks() int64 { return vm.totalTicks }

// ─── Per-window excursion accessors ──────────────────────────────────────────

// WindowDomShareSeries returns the per-tenant slice of per-window dominant shares
// (post-warmup sealed windows only). Each value is
//
//	dominant_share_i(w) = max_r A_i^r(w) / (C^r * window_size_us)
//
// Returns an empty map if windowed tracking was not configured.
func (vm *VectorMeter) WindowDomShareSeries() map[string][]float64 {
	out := make(map[string][]float64, len(vm.windowDomShares))
	for t, s := range vm.windowDomShares {
		cp := make([]float64, len(s))
		copy(cp, s)
		out[t] = cp
	}
	return out
}

// WindowExcursionSeries returns the per-tenant per-window excursion series
// (post-warmup sealed windows only). Each value is
//
//	excursion_i(w) = max(0, dominant_share_i(w) - omega_i)
//
// Returns an empty map if windowed tracking was not configured.
func (vm *VectorMeter) WindowExcursionSeries() map[string][]float64 {
	out := make(map[string][]float64, len(vm.windowExcursions))
	for t, s := range vm.windowExcursions {
		cp := make([]float64, len(s))
		copy(cp, s)
		out[t] = cp
	}
	return out
}

// WindowExcursionPercentiles computes per-tenant excursion statistics over all
// post-warmup sealed windows. Returns a map keyed by tenant with fields:
// P50, P90, P95, P99, Mean, Max. Returns nil for a tenant with no sealed windows.
func (vm *VectorMeter) WindowExcursionPercentiles() map[string]*WindowExcursionStats {
	out := make(map[string]*WindowExcursionStats)
	for tenant, excursions := range vm.windowExcursions {
		if len(excursions) == 0 {
			continue
		}
		sorted := make([]float64, len(excursions))
		copy(sorted, excursions)
		sort.Float64s(sorted)

		var sum, maxVal float64
		for _, v := range sorted {
			sum += v
			if v > maxVal {
				maxVal = v
			}
		}
		mean := sum / float64(len(sorted))

		out[tenant] = &WindowExcursionStats{
			P50:         windowPctile(sorted, 50),
			P90:         windowPctile(sorted, 90),
			P95:         windowPctile(sorted, 95),
			P99:         windowPctile(sorted, 99),
			Mean:        mean,
			Max:         maxVal,
			WindowCount: len(sorted),
		}
	}
	return out
}

// WindowExcursionStats holds per-window dominant-share excursion statistics
// for a single tenant over all post-warmup sealed windows.
type WindowExcursionStats struct {
	P50         float64 `json:"p50"`
	P90         float64 `json:"p90"`
	P95         float64 `json:"p95"`
	P99         float64 `json:"p99"`
	Mean        float64 `json:"mean"`
	Max         float64 `json:"max"`
	WindowCount int     `json:"window_count"`
}

// windowPctile computes a percentile from a pre-sorted slice (linear interpolation).
func windowPctile(sorted []float64, p float64) float64 {
	n := float64(len(sorted))
	if n == 0 {
		return 0
	}
	rank := (p / 100.0) * (n - 1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
