// Package kvtime — BucketManager: per-tenant entitlement buckets.
//
// Mathematical model:
//
//	B_i(t) = min(B_i^max, B_i(prev) + ω_i · K · Δt − A_i(Δt))
//
// where:
//   - ω_i  is tenant i's entitlement share (fraction of K·Δt)
//   - K    is total KV capacity in tokens (TotalBlocks × BlockSizeTokens)
//   - Δt   is the tick interval in µs
//   - B_i^max = β · K  (β in seconds, converts to token·µs)
//   - A_i(Δt) is the memory-time consumed by tenant i since the last tick
//     (obtained from Meter.TenantKVTime delta, not cumulative total)
//
// Admission score per request:
//
//	θ_r = max(0, B_i) / k_r
//
// where k_r is the current resident block count of request r (in tokens).
// Requests are sorted by θ_r descending — highest-score (most entitled,
// most efficient) first.  When B_i ≤ 0 the tenant is overdrawn and all
// its requests score 0.
package kvtime

// TenantBucketConfig holds per-tenant entitlement parameters.
type TenantBucketConfig struct {
	// OmegaI is tenant i's entitlement share (fraction of K·Δt credited per µs).
	// Must be in (0, 1].  Σ OmegaI over all tenants should equal η ≤ 1.
	OmegaI float64

	// BetaSeconds is the bucket depth in seconds (B_i^max = β · K in token·µs).
	// A depth of 1s means a tenant can bank up to 1s worth of full-cache occupancy.
	BetaSeconds float64

	// HSeconds is the overdraft floor in seconds (floor = -H · K).
	// Zero means no overdraft allowed; negative balances still occur briefly
	// from the Reconcile arithmetic but requests score max(0, balance).
	HSeconds float64
}

// BucketManager maintains per-tenant entitlement balances and reconciles
// them at each scheduler tick.
//
// Reconcile must be called once per tick AFTER the Meter has been ticked
// (so that deltaKVTime values are fresh).
//
// Thread-safety: not safe for concurrent use.  The scheduler calls
// Reconcile and Score from a single goroutine.
type BucketManager struct {
	// tenantBalance holds the current B_i for each tenant (token·µs).
	tenantBalance map[string]float64

	// tenantLastKVTime holds the cumulative A_i(T) at the most recent Reconcile call.
	// Used to compute Δ A_i = A_i(now) − A_i(prev) per tick.
	tenantLastKVTime map[string]float64

	// configs holds per-tenant parameters (omega, beta, h).
	configs map[string]TenantBucketConfig

	// totalCapacityTokens is K = TotalBlocks × BlockSizeTokens.
	// Constant for the lifetime of the simulation.
	totalCapacityTokens float64

	// defaultConfig is used for tenants not explicitly registered.
	// omega=1/N (equal share), beta=1s, h=0.
	defaultConfig TenantBucketConfig
}

// NewBucketManager creates a BucketManager.
//
//   - totalKVBlocks: KVCacheState.TotalBlocks
//   - blockSizeTokens: KVCacheState.BlockSizeTokens
//   - configs: per-tenant config map (keyed by TenantID); tenants absent from
//     the map will use defaultConfig (equal share with the registered set).
func NewBucketManager(totalKVBlocks, blockSizeTokens int64, configs map[string]TenantBucketConfig) *BucketManager {
	totalCap := float64(totalKVBlocks) * float64(blockSizeTokens)

	// Default: equal share across registered tenants (fallback for unregistered).
	n := len(configs)
	if n == 0 {
		n = 1
	}
	defaultCfg := TenantBucketConfig{
		OmegaI:      1.0 / float64(n),
		BetaSeconds: 1.0,
		HSeconds:    0.0,
	}

	bm := &BucketManager{
		tenantBalance:       make(map[string]float64, len(configs)),
		tenantLastKVTime:    make(map[string]float64, len(configs)),
		configs:             make(map[string]TenantBucketConfig, len(configs)),
		totalCapacityTokens: totalCap,
		defaultConfig:       defaultCfg,
	}

	// Initialise each tenant's balance at B_i^max = β · K (full bucket).
	// Starting at B_i^max ensures the bucket begins fully credited so that
	// steady-state dynamics are reached quickly. The 30s warmup period is
	// excluded from metrics, which washes out any transient effects from
	// the initial full-bucket state. Starting at 0 would cause both tenants
	// to begin "overdrawn", producing anomalous scheduling artefacts in the
	// warmup window (iter-2 fix; iter-1 used 0 which was incorrect per campaign spec).
	for tenant, cfg := range configs {
		bm.configs[tenant] = cfg
		ceiling := cfg.BetaSeconds * totalCap * 1e6 // B_i^max in token·µs
		bm.tenantBalance[tenant] = ceiling
		bm.tenantLastKVTime[tenant] = 0.0
	}

	return bm
}

// Reconcile updates each tenant's bucket balance given:
//
//   - cumKVTime: the current cumulative A_i(T) for all tenants (from Meter.TenantKVTime()).
//   - nowUs: current simulation clock in µs.
//   - prevUs: previous tick's simulation clock in µs.
//
// The credit earned this tick for tenant i is:
//
//	credit_i = ω_i · K · Δt
//
// The drain this tick is:
//
//	drain_i = A_i(now) − A_i(prev)   (delta KV-time from meter)
//
// Balance update:
//
//	B_i ← clamp(B_i + credit_i − drain_i, −H_i · K, β_i · K)
func (bm *BucketManager) Reconcile(cumKVTime map[string]float64, nowUs, prevUs int64) {
	if nowUs <= prevUs {
		return // no time elapsed; skip
	}
	deltaUs := float64(nowUs - prevUs)
	K := bm.totalCapacityTokens

	// Process all known tenants.
	for tenant, bal := range bm.tenantBalance {
		cfg := bm.configFor(tenant)

		// Credit earned this tick.
		credit := cfg.OmegaI * K * deltaUs

		// KV-time drained this tick = cumulative delta since last reconcile.
		currKVTime := cumKVTime[tenant]
		prevKVTime := bm.tenantLastKVTime[tenant]
		drain := currKVTime - prevKVTime
		if drain < 0 {
			drain = 0 // defensive: should not decrease
		}

		// Update balance with clamp to [−H·K, β·K].
		newBal := bal + credit - drain
		floor := -cfg.HSeconds * K * 1e6  // H in seconds × K × µs/s = token·µs
		ceiling := cfg.BetaSeconds * K * 1e6 // β in seconds × K × µs/s = token·µs
		if newBal < floor {
			newBal = floor
		}
		if newBal > ceiling {
			newBal = ceiling
		}
		bm.tenantBalance[tenant] = newBal
		bm.tenantLastKVTime[tenant] = currKVTime
	}

	// Handle tenants seen in cumKVTime but not yet in our balance map.
	// Register them on first encounter.
	for tenant, cumKV := range cumKVTime {
		if _, known := bm.tenantBalance[tenant]; !known {
			cfg := bm.configFor(tenant)
			credit := cfg.OmegaI * K * deltaUs
			prevKV := bm.tenantLastKVTime[tenant] // 0 if first encounter
			drain := cumKV - prevKV
			if drain < 0 {
				drain = 0
			}
			newBal := credit - drain
			ceiling := cfg.BetaSeconds * K * 1e6
			floor := -cfg.HSeconds * K * 1e6
			if newBal > ceiling {
				newBal = ceiling
			}
			if newBal < floor {
				newBal = floor
			}
			bm.tenantBalance[tenant] = newBal
			bm.tenantLastKVTime[tenant] = cumKV
		}
	}
}

// Balance returns the current bucket balance for a tenant (token·µs).
// Returns 0 for unknown tenants (not yet seen in any tick).
func (bm *BucketManager) Balance(tenant string) float64 {
	return bm.tenantBalance[tenant]
}

// IsOverdrawn reports whether tenant i is overdrawn (B_i ≤ 0).
func (bm *BucketManager) IsOverdrawn(tenant string) bool {
	return bm.tenantBalance[tenant] <= 0
}

// Score returns the admission score θ_r = max(0, B_i) / k_r for a request
// belonging to tenant i with k_r resident tokens.
//
// If k_r == 0 (e.g. prefill not yet started), returns max(0, B_i) — the
// tenant's full balance, so the request is ordered with other un-started requests
// by tenant balance only (FIFO within tenant for k_r=0 requests).
func (bm *BucketManager) Score(tenant string, residentTokens float64) float64 {
	bal := bm.tenantBalance[tenant]
	if bal <= 0 {
		return 0
	}
	if residentTokens <= 0 {
		return bal
	}
	return bal / residentTokens
}

// configFor returns the TenantBucketConfig for a tenant, falling back to
// the default config for unregistered tenants.
func (bm *BucketManager) configFor(tenant string) TenantBucketConfig {
	if cfg, ok := bm.configs[tenant]; ok {
		return cfg
	}
	return bm.defaultConfig
}

// TenantBalances returns a snapshot copy of all tenant balances (token·µs).
func (bm *BucketManager) TenantBalances() map[string]float64 {
	out := make(map[string]float64, len(bm.tenantBalance))
	for k, v := range bm.tenantBalance {
		out[k] = v
	}
	return out
}
