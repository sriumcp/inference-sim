package cluster

import (
	"math"

	"github.com/sirupsen/logrus"
)

// SharedSpillBus models a node-shared NVMe KV-offload link (~2 GB/s) shared by all
// replicas on a single node. It meters GPU→NVMe spill traffic (MirrorToCPU events)
// through a single bandwidth budget, applies fair-share contention, and optionally
// enforces per-tenant flow buckets.
//
// The bus is owned by ClusterSimulator (cluster-level resource: one per node).
//
// Architecture: called from ClusterSimulator after each instance step via AccountStep().
// Per-tenant spill counts are extracted from TieredKVCache.ConsumePerTenantSpills().
//
// Measurement model (not a DES event; analytical queuing approximation):
//   - Each step, AccountStep() receives per-tenant blocks spilled by all instances.
//   - The bus accumulates a sliding utilization: busyUs / windowUs.
//   - Flow bucket: per-tenant rate-plus-burst bucket; when overdrawn (enforce=true),
//     the excess bytes are "deferred" — tracked as queue delay added to that tenant.
//   - L_spill_bus_queue_delay_us: cumulative hypothetical queue delay for tenant L.
//   - enforcement_bites_delta: difference in L's queue delay between enforce=false and
//     enforce=true conditions (positive = enforcement helped L).
//
// This is apparatus code for the shared-spill-bus-enforcement experiment family.
// NOT production code — designed for iter-1 rehearsal feasibility validation.
type SharedSpillBus struct {
	// Configuration
	cBWBytesPerUs float64 // C_BW in bytes/µs (e.g. 2 GB/s = 2000 bytes/µs)
	omegaBW       float64 // per-tenant entitlement fraction (e.g. 0.45)
	betaBWSec     float64 // burst depth in seconds (default = L-distribution-derived)
	blockBytes    int64   // bytes per KV block (1 MiB for Llama-3.1-8B FP8)
	enforce       bool    // true → flow-bucket enforcement active (h-main); false → control-negative

	// Flow bucket state per tenant (rate-plus-burst leaky-bucket)
	// balance: current credit (bytes); refills at rate=omega*C_BW; positive=in-credit, negative=overdrawn.
	tenantBuckets map[string]*spillBucket

	// Bus utilization tracking
	// busyUs: total µs the bus was busy (sum of transfer durations scheduled on bus)
	// windowUs: total µs elapsed (horizon elapsed so far)
	busyUs   float64
	windowUs float64

	// Queue depth: estimated pending bytes in the bus queue
	pendingQueueBytes float64

	// Per-tenant accumulated bus queue delay (µs)
	tenantQueueDelayUs map[string]float64

	// Per-tenant total bytes spilled
	tenantSpillBytes map[string]int64

	// enforcement_bites: count of steps where enforcement deferred H's spills
	enforcementBitesCount int64

	// Last step timestamp (to compute delta time for bucket refills)
	lastStepUs int64

	// activeSpills: count of concurrent spills being metered this step
	// (same formula as PD fair-share: C_BW / max(1, activeSpills))
	activeSpills int

	// Totals
	totalBlocksAccountedH int64
	totalBlocksAccountedL int64
}

// spillBucket is a per-tenant rate-plus-burst token bucket for flow metering.
type spillBucket struct {
	balance    float64 // current credit in bytes; negative = overdrawn
	rate       float64 // bytes/µs = omega * C_BW
	burst      float64 // max balance in bytes (burst depth * rate)
	lastUpdate int64   // last µs timestamp for refill
}

// SharedSpillBusConfig holds configuration for SharedSpillBus construction.
type SharedSpillBusConfig struct {
	Enabled          bool    // If false, SharedSpillBus is a no-op
	CBWGBps          float64 // C_BW in GB/s (default 2.0)
	OmegaBW          float64 // per-tenant entitlement fraction (default 0.45)
	BetaBWSec        float64 // burst depth in seconds (default 1.0)
	BlockSizeTokens  int64   // tokens per KV block (from SimConfig.BlockSizeTokens)
	KVBytesPerToken  float64 // bytes per KV token (from KVBytesPerToken(model, tp))
	Enforce          bool    // true → flow-bucket enforcement; false → fair-share only
}

// NewSharedSpillBus creates a SharedSpillBus from configuration.
// Returns nil if cfg.Enabled is false (backward-compatible no-op).
func NewSharedSpillBus(cfg SharedSpillBusConfig) *SharedSpillBus {
	if !cfg.Enabled {
		return nil
	}
	if cfg.CBWGBps <= 0 {
		cfg.CBWGBps = 2.0
	}
	if cfg.OmegaBW <= 0 {
		cfg.OmegaBW = 0.45
	}
	if cfg.BetaBWSec <= 0 {
		cfg.BetaBWSec = 1.0
	}

	blockBytes := int64(float64(cfg.BlockSizeTokens) * cfg.KVBytesPerToken)
	if blockBytes <= 0 {
		blockBytes = 1048576 // 1 MiB fallback (Llama-3.1-8B FP8 TP=1 default)
	}

	cBWBytesPerUs := cfg.CBWGBps * 1000.0 // GB/s → bytes/µs

	logrus.Infof("[shared_spill_bus] Initialized: C_BW=%.1f GB/s (%.0f bytes/µs), omega=%.2f, beta=%.1fs, enforce=%v, blockBytes=%d",
		cfg.CBWGBps, cBWBytesPerUs, cfg.OmegaBW, cfg.BetaBWSec, cfg.Enforce, blockBytes)

	return &SharedSpillBus{
		cBWBytesPerUs:      cBWBytesPerUs,
		omegaBW:            cfg.OmegaBW,
		betaBWSec:          cfg.BetaBWSec,
		blockBytes:         blockBytes,
		enforce:            cfg.Enforce,
		tenantBuckets:      make(map[string]*spillBucket),
		tenantQueueDelayUs: make(map[string]float64),
		tenantSpillBytes:   make(map[string]int64),
	}
}

// AccountStep accounts for spill traffic from one instance in one simulation step.
// tenantSpills maps tenantID → blocks newly mirrored to CPU this step (for this instance).
// nowUs is the current simulation clock in microseconds.
// stepDurationUs is the duration of this simulation step.
//
// This method updates bus utilization and bucket state but does NOT inject DES latency.
// For physical enforcement, use ComputeSpillLatency which also returns per-instance latency.
//
// Deprecated: use ComputeSpillLatency for physical enforcement (iter-2+). AccountStep is
// retained for backward compatibility with tests but ComputeSpillLatency supersedes it.
func (b *SharedSpillBus) AccountStep(tenantSpills map[string]int64, nowUs, stepDurationUs int64) {
	b.ComputeSpillLatency(tenantSpills, nowUs, stepDurationUs)
}

// ComputeSpillLatency accounts for spill traffic from one instance in one simulation step
// and returns the physical latency (µs) to inject into that instance's pending transfer
// accumulator. This is the PHYSICAL enforcement path (iter-2+).
//
// tenantSpills maps tenantID → blocks newly mirrored to CPU this step (for this instance).
// nowUs is the current simulation clock in microseconds.
// stepDurationUs is the duration of this simulation step.
//
// Physical contention model:
//   - Fair-share cost: each instance's blocks × blockBytes / C_BW per block (uncontended).
//   - With contention (activeSpills > 1): effective rate = C_BW / activeSpills.
//     activeSpills is estimated from bus utilization (busyUs / windowUs, clamped to [1, N]).
//   - With enforce=true and overdrawn tenant: additional deferral penalty for that tenant =
//     min(overdraft_bytes, spill_bytes) / tenantRate injected on top of fair-share cost.
//
// Returns the total latency_us to inject via InjectSpillBusLatency for this instance.
func (b *SharedSpillBus) ComputeSpillLatency(tenantSpills map[string]int64, nowUs, stepDurationUs int64) int64 {
	if len(tenantSpills) == 0 {
		// No spills this step — advance window but no bus load
		b.windowUs += float64(stepDurationUs)
		b.lastStepUs = nowUs
		return 0
	}

	// Elapsed time since last step (for bucket refills)
	var elapsedUs float64
	if b.lastStepUs > 0 {
		elapsedUs = float64(nowUs - b.lastStepUs)
	} else {
		elapsedUs = float64(stepDurationUs)
	}
	b.lastStepUs = nowUs
	b.windowUs += float64(stepDurationUs)

	// Compute total bytes this step and per-tenant bytes
	totalBlocks := int64(0)
	tenantBytes := make(map[string]float64, len(tenantSpills))
	for tenantID, blocks := range tenantSpills {
		bytes := float64(blocks) * float64(b.blockBytes)
		tenantBytes[tenantID] = bytes
		totalBlocks += blocks
		b.tenantSpillBytes[tenantID] += blocks * b.blockBytes
	}
	totalBytes := float64(totalBlocks) * float64(b.blockBytes)

	// Bus transfer time for all bytes at full C_BW (ideal, no contention)
	idealTransferUs := totalBytes / b.cBWBytesPerUs
	b.busyUs += idealTransferUs

	// Estimate active concurrent spills from bus utilization.
	// When bus utilization > 1.0, multiple instances are contending:
	// activeSpills ≈ max(1, round(busUtilization)) clamped to [1, 8].
	// This is an analytical approximation: in the sequential DES, each instance's step
	// fires one at a time, but they all share the same physical NVMe link within a real-time
	// interval. The utilization ratio captures how much demand exceeds capacity.
	activeSpills := 1
	if b.windowUs > 0 {
		util := b.busyUs / b.windowUs
		if util > 1.0 {
			activeSpills = int(math.Round(util))
			if activeSpills < 1 {
				activeSpills = 1
			}
			if activeSpills > 8 {
				activeSpills = 8
			}
		}
	}

	// Physical bus cost for this instance's spills:
	// effectiveBW = C_BW / max(1, activeSpills)
	// latency_us = totalBytes / effectiveBW
	effectiveBW := b.cBWBytesPerUs / float64(activeSpills)
	physicalLatencyUs := int64(totalBytes / effectiveBW)

	// Per-tenant entitlement per step (in bytes/µs)
	tenantEntitlementBytesPerUs := b.omegaBW * b.cBWBytesPerUs

	// Process each tenant: refill bucket, compute usage vs entitlement.
	// Also compute enforcement deferral if enforce=true and tenant is overdrawn.
	enforcementPenaltyUs := int64(0)
	for tenantID, bytes := range tenantBytes {
		bucket := b.getBucket(tenantID)

		// Refill bucket: rate = omega * C_BW (bytes/µs)
		if elapsedUs > 0 {
			refill := tenantEntitlementBytesPerUs * elapsedUs
			bucket.balance += refill
			if bucket.balance > bucket.burst {
				bucket.balance = bucket.burst
			}
		}
		bucket.lastUpdate = nowUs

		// Debit spill bytes from bucket
		bucket.balance -= bytes

		// Track per-tenant totals
		switch tenantID {
		case "H":
			b.totalBlocksAccountedH += tenantSpills[tenantID]
		case "L":
			b.totalBlocksAccountedL += tenantSpills[tenantID]
		}

		// Enforcement deferral: if enforce=true and this tenant is overdrawn,
		// add additional penalty = min(overdraft, spillBytes) / tenantRate.
		// This slows down the overdrawn tenant, reducing their concurrent presence on
		// the bus in subsequent steps and allowing under-quota tenants (L) to proceed
		// with lower contention.
		if b.enforce && bucket.balance < 0 {
			overdraftBytes := math.Min(bytes, -bucket.balance)
			tenantRate := b.omegaBW * b.cBWBytesPerUs
			if tenantRate > 0 {
				deferralUs := int64(overdraftBytes / tenantRate)
				enforcementPenaltyUs += deferralUs
				b.enforcementBitesCount++
			}
		}
	}

	// Compute diagnostic queue delay (legacy measurement model, kept for metric continuity).
	busCapacityBytes := b.cBWBytesPerUs * float64(stepDurationUs)
	excessBytes := math.Max(0, totalBytes-busCapacityBytes)
	if excessBytes > 0 && b.enforce {
		for tenantID, bytes := range tenantBytes {
			bucket := b.getBucket(tenantID)
			if bucket.balance < 0 {
				tenantOverdraft := math.Min(bytes, -bucket.balance)
				delayUs := tenantOverdraft / b.cBWBytesPerUs
				b.tenantQueueDelayUs[tenantID] += delayUs
			}
			_ = bytes
		}
	} else if excessBytes > 0 {
		if totalBytes > 0 {
			for tenantID, bytes := range tenantBytes {
				fraction := bytes / totalBytes
				delayUs := (excessBytes / b.cBWBytesPerUs) * fraction
				b.tenantQueueDelayUs[tenantID] += delayUs
			}
		}
	}

	// Total physical latency = fair-share contention cost + enforcement deferral penalty.
	return physicalLatencyUs + enforcementPenaltyUs
}

// getBucket returns (or creates) the flow bucket for a tenant.
func (b *SharedSpillBus) getBucket(tenantID string) *spillBucket {
	if bucket, ok := b.tenantBuckets[tenantID]; ok {
		return bucket
	}
	rate := b.omegaBW * b.cBWBytesPerUs
	burst := rate * b.betaBWSec * 1e6 // betaBWSec * µs/s = µs of burst
	bucket := &spillBucket{
		balance: burst, // start full (maximum credit)
		rate:    rate,
		burst:   burst,
	}
	b.tenantBuckets[tenantID] = bucket
	return bucket
}

// BusUtilization returns the fraction of time the shared bus was busy (0.0–1.0+).
// Values > 1.0 indicate overload (more demand than capacity).
func (b *SharedSpillBus) BusUtilization() float64 {
	if b.windowUs == 0 {
		return 0
	}
	return b.busyUs / b.windowUs
}

// TenantQueueDelayUs returns the cumulative spill-bus queue delay for a tenant (µs).
func (b *SharedSpillBus) TenantQueueDelayUs(tenantID string) float64 {
	return b.tenantQueueDelayUs[tenantID]
}

// TenantSpillBytes returns the total bytes spilled by a tenant.
func (b *SharedSpillBus) TenantSpillBytes(tenantID string) int64 {
	return b.tenantSpillBytes[tenantID]
}

// EnforcementBitesCount returns the number of steps where enforcement deferred H's spills.
func (b *SharedSpillBus) EnforcementBitesCount() int64 {
	return b.enforcementBitesCount
}

// SpillBusMetrics holds the output metrics from the SharedSpillBus for inclusion in MetricsOutput.
type SpillBusMetrics struct {
	Enabled             bool    `json:"enabled"`
	Enforce             bool    `json:"enforce"`
	SharedBusUtil       float64 `json:"shared_bus_utilization"`
	LQueueDelayUs       float64 `json:"L_spill_bus_queue_delay_us"`
	HQueueDelayUs       float64 `json:"H_spill_bus_queue_delay_us"`
	LSpillBytes         int64   `json:"L_spill_bytes"`
	HSpillBytes         int64   `json:"H_spill_bytes"`
	EnforcementBites    int64   `json:"enforcement_bites_count"`
	TotalBlocksH        int64   `json:"total_blocks_accounted_H"`
	TotalBlocksL        int64   `json:"total_blocks_accounted_L"`
}

// Metrics returns the SpillBusMetrics snapshot.
func (b *SharedSpillBus) Metrics() SpillBusMetrics {
	return SpillBusMetrics{
		Enabled:          true,
		Enforce:          b.enforce,
		SharedBusUtil:    b.BusUtilization(),
		LQueueDelayUs:    b.tenantQueueDelayUs["L"],
		HQueueDelayUs:    b.tenantQueueDelayUs["H"],
		LSpillBytes:      b.tenantSpillBytes["L"],
		HSpillBytes:      b.tenantSpillBytes["H"],
		EnforcementBites: b.enforcementBitesCount,
		TotalBlocksH:     b.totalBlocksAccountedH,
		TotalBlocksL:     b.totalBlocksAccountedL,
	}
}
