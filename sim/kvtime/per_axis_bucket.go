// per_axis_bucket.go implements PerAxisBucketManager: one token bucket per
// tenant per axis, for the two-pool vector KVtime scheduler.
//
// Mathematical model (per axis r ∈ {P, D}):
//
//	B_i^r(t) = clamp(B_i^r(prev) + ω_i·C^r·Δt − A_i^r(Δt), −H·C^r, β·C^r)
//
// where:
//   - ω_i   is the tenant's per-axis entitlement share
//   - C^r   is the per-axis capacity in tokens (K_r × blockSize)
//   - Δt    is the tick interval in µs
//   - β·C^r is the bucket depth (B_i^max per axis)
//   - H·C^r is the overdraft floor
//   - A_i^r(Δt) is the per-axis KV-time delta from VectorMeter
//
// Dominant admission score (integrate-then-combine):
//
//	θ_i = max(0, max_r B_i^r) / k_i
//
// where k_i is the request's current resident token count.
// Each axis is regulated against its own capacity; the dominant (max-over-axes)
// score combines axes for scheduling decisions. This prevents cross-axis subsidy:
// a tenant cannot spend slack from a light axis to subsidize a heavy axis.
//
// This file is part of patch 02-vector-tracked.
// It does NOT modify any production BLIS files.
package kvtime

// AxisBucketConfig holds per-tenant entitlement parameters (same for both axes
// in this campaign: ω=0.45, β=10s, H=0s).
type AxisBucketConfig struct {
	OmegaI      float64 // tenant entitlement share (fraction of C^r·Δt credited per µs)
	BetaSeconds float64 // bucket depth in seconds (B_i^max = β·C^r in token·µs)
	HSeconds    float64 // overdraft floor in seconds (floor = −H·C^r)
}

// PerAxisBucketManager maintains two sets of token buckets (one per axis)
// for each tenant. It mirrors BucketManager (bucket.go) but operates per-axis.
//
// Thread-safety: not safe for concurrent use. Called from a single goroutine
// in the scheduler's OrderQueue.
type PerAxisBucketManager struct {
	// balanceP[tenant] = current B_i^P (token·µs)
	balanceP map[string]float64
	// balanceD[tenant] = current B_i^D (token·µs)
	balanceD map[string]float64

	// lastKVTimeP/D: cumulative A_i^r(T) at last Reconcile call.
	lastKVTimeP map[string]float64
	lastKVTimeD map[string]float64

	// configs: per-tenant axis bucket parameters.
	configs map[string]AxisBucketConfig

	// capacityP/D: C^r = K_r × blockSize × µs/s (token·µs capacity for one second).
	// Stored as pure token capacity (K_r × blockSize); multiplied by Δt during reconcile.
	capacityTokensP float64 // K_P × blockSizeTokens
	capacityTokensD float64 // K_D × blockSizeTokens

	// defaultConfig: fallback for unregistered tenants.
	defaultConfig AxisBucketConfig
}

// NewPerAxisBucketManager creates a PerAxisBucketManager.
//
//   - kP, kD: P-pool and D-pool capacities in blocks
//   - blockSizeTokens: tokens per block
//   - configs: per-tenant parameters (keyed by TenantID)
//
// Buckets are initialized at B_i^max = β·C^r (full bucket) so steady-state
// dynamics are reached quickly (consistent with BucketManager, bucket.go:104).
func NewPerAxisBucketManager(kP, kD, blockSizeTokens int64, configs map[string]AxisBucketConfig) *PerAxisBucketManager {
	capP := float64(kP) * float64(blockSizeTokens)
	capD := float64(kD) * float64(blockSizeTokens)

	n := len(configs)
	if n == 0 {
		n = 1
	}
	defaultCfg := AxisBucketConfig{
		OmegaI:      1.0 / float64(n),
		BetaSeconds: 1.0,
		HSeconds:    0.0,
	}

	bm := &PerAxisBucketManager{
		balanceP:        make(map[string]float64, len(configs)),
		balanceD:        make(map[string]float64, len(configs)),
		lastKVTimeP:     make(map[string]float64, len(configs)),
		lastKVTimeD:     make(map[string]float64, len(configs)),
		configs:         make(map[string]AxisBucketConfig, len(configs)),
		capacityTokensP: capP,
		capacityTokensD: capD,
		defaultConfig:   defaultCfg,
	}

	// Initialize balances at B_i^max = β · C^r (full bucket).
	for tenant, cfg := range configs {
		bm.configs[tenant] = cfg
		ceilP := cfg.BetaSeconds * capP * 1e6 // token·µs
		ceilD := cfg.BetaSeconds * capD * 1e6
		bm.balanceP[tenant] = ceilP
		bm.balanceD[tenant] = ceilD
	}

	return bm
}

// Reconcile updates per-axis bucket balances for all tenants.
//
//   - cumKVTimeP/D: current cumulative A_i^P(T) and A_i^D(T) (from VectorMeter)
//   - nowUs, prevUs: current and previous simulation clock in µs
//
// Credit per axis:   credit_i^r = ω_i · C^r · Δt  (token·µs)
// Drain per axis:    drain_i^r  = A_i^r(now) − A_i^r(prev)
// Balance update:    B_i^r ← clamp(B_i^r + credit − drain, −H·C^r, β·C^r)
func (bm *PerAxisBucketManager) Reconcile(cumKVTimeP, cumKVTimeD map[string]float64, nowUs, prevUs int64) {
	if nowUs <= prevUs {
		return
	}
	deltaUs := float64(nowUs - prevUs)

	// Process all registered tenants.
	for tenant, cfg := range bm.configs {
		capP := bm.capacityTokensP
		capD := bm.capacityTokensD
		creditP := cfg.OmegaI * capP * deltaUs
		creditD := cfg.OmegaI * capD * deltaUs

		drainP := cumKVTimeP[tenant] - bm.lastKVTimeP[tenant]
		if drainP < 0 {
			drainP = 0
		}
		drainD := cumKVTimeD[tenant] - bm.lastKVTimeD[tenant]
		if drainD < 0 {
			drainD = 0
		}

		floorP := -cfg.HSeconds * capP * 1e6
		ceilP := cfg.BetaSeconds * capP * 1e6
		floorD := -cfg.HSeconds * capD * 1e6
		ceilD := cfg.BetaSeconds * capD * 1e6

		newBalP := bm.balanceP[tenant] + creditP - drainP
		if newBalP < floorP {
			newBalP = floorP
		}
		if newBalP > ceilP {
			newBalP = ceilP
		}
		bm.balanceP[tenant] = newBalP
		bm.lastKVTimeP[tenant] = cumKVTimeP[tenant]

		newBalD := bm.balanceD[tenant] + creditD - drainD
		if newBalD < floorD {
			newBalD = floorD
		}
		if newBalD > ceilD {
			newBalD = ceilD
		}
		bm.balanceD[tenant] = newBalD
		bm.lastKVTimeD[tenant] = cumKVTimeD[tenant]
	}

	// Handle tenants seen in cumKVTime but not yet registered.
	for tenant, cumP := range cumKVTimeP {
		if _, known := bm.balanceP[tenant]; !known {
			cfg := bm.configFor(tenant)
			capP := bm.capacityTokensP
			capD := bm.capacityTokensD
			creditP := cfg.OmegaI * capP * deltaUs
			creditD := cfg.OmegaI * capD * deltaUs
			drainP := cumP - bm.lastKVTimeP[tenant]
			if drainP < 0 {
				drainP = 0
			}
			drainD := cumKVTimeD[tenant] - bm.lastKVTimeD[tenant]
			if drainD < 0 {
				drainD = 0
			}
			newBalP := creditP - drainP
			newBalD := creditD - drainD
			ceilP := cfg.BetaSeconds * capP * 1e6
			ceilD := cfg.BetaSeconds * capD * 1e6
			floorP := -cfg.HSeconds * capP * 1e6
			floorD := -cfg.HSeconds * capD * 1e6
			if newBalP > ceilP {
				newBalP = ceilP
			}
			if newBalP < floorP {
				newBalP = floorP
			}
			if newBalD > ceilD {
				newBalD = ceilD
			}
			if newBalD < floorD {
				newBalD = floorD
			}
			bm.balanceP[tenant] = newBalP
			bm.balanceD[tenant] = newBalD
			bm.lastKVTimeP[tenant] = cumP
			bm.lastKVTimeD[tenant] = cumKVTimeD[tenant]
		}
	}
}

// DominantBalance returns max_r B_i^r (the dominant per-axis balance).
// Used for integrate-then-combine scoring.
func (bm *PerAxisBucketManager) DominantBalance(tenant string) float64 {
	bp := bm.balanceP[tenant]
	bd := bm.balanceD[tenant]
	if bp > bd {
		return bp
	}
	return bd
}

// Score returns the dominant admission score:
//
//	θ_i = max(0, max_r B_i^r) / k_i
//
// where k_i is the request's current resident token count.
// Returns max(0, dominant_balance) if k_i == 0.
func (bm *PerAxisBucketManager) Score(tenant string, residentTokens float64) float64 {
	dom := bm.DominantBalance(tenant)
	if dom <= 0 {
		return 0
	}
	if residentTokens <= 0 {
		return dom
	}
	return dom / residentTokens
}

// IsOverdrawn reports whether the tenant's dominant balance is <= 0.
// Used by AllowAdmission in the vector-kvtime scheduler.
func (bm *PerAxisBucketManager) IsOverdrawn(tenant string) bool {
	return bm.DominantBalance(tenant) <= 0
}

// BalanceP returns B_i^P for a tenant.
func (bm *PerAxisBucketManager) BalanceP(tenant string) float64 { return bm.balanceP[tenant] }

// BalanceD returns B_i^D for a tenant.
func (bm *PerAxisBucketManager) BalanceD(tenant string) float64 { return bm.balanceD[tenant] }

// configFor returns the AxisBucketConfig for a tenant, with fallback to default.
func (bm *PerAxisBucketManager) configFor(tenant string) AxisBucketConfig {
	if cfg, ok := bm.configs[tenant]; ok {
		return cfg
	}
	return bm.defaultConfig
}

// TenantBalancesP returns a snapshot of all P-axis balances.
func (bm *PerAxisBucketManager) TenantBalancesP() map[string]float64 {
	out := make(map[string]float64, len(bm.balanceP))
	for k, v := range bm.balanceP {
		out[k] = v
	}
	return out
}

// TenantBalancesD returns a snapshot of all D-axis balances.
func (bm *PerAxisBucketManager) TenantBalancesD() map[string]float64 {
	out := make(map[string]float64, len(bm.balanceD))
	for k, v := range bm.balanceD {
		out[k] = v
	}
	return out
}
