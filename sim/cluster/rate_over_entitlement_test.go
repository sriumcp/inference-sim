package cluster

import (
	"math"
	"testing"
)

// TestSharedSpillBus_RateOverEntitlement verifies the quantity thm:vector-burst
// bounds: rate_over_entitlement_i = bytes_i / (omega_i * C_BW * T), where T is
// elapsed bus time. =1.0 means the tenant consumed exactly its entitled share of
// bus capacity-time; >1.0 means it monopolized beyond entitlement.
//
// This is P1's named PRIMARY metric. Unlike cumulative volume-share (which divides
// by other tenants' usage and is rate-invariant under open-loop demand), this
// divides by capacity-time AVAILABLE, so throttling a tenant's rate lowers it.
func TestSharedSpillBus_RateOverEntitlement(t *testing.T) {
	cfg := SharedSpillBusConfig{
		Enabled: true, CBWGBps: 2.0, OmegaBW: 0.45, BetaBWSec: 1.0,
		BlockSizeTokens: 16, KVBytesPerToken: 65536, // 1 MiB/block
	}
	b := NewSharedSpillBus(cfg)

	// Over 1 second (1e6 µs) of bus time, C_BW=2 GB/s moves 2e9 bytes total.
	// H's entitlement = omega*C_BW*T = 0.45 * 2e9 = 9e8 bytes.
	// Make H spill ~1.8e9 bytes (1717 blocks × 1 MiB) → ~2x its entitlement.
	step := int64(1_000_000) // advance 1s of bus time
	b.ComputeSpillLatency(map[string]int64{"H": 1717}, step, step)

	roe := b.RateOverEntitlement("H")
	// 1717 * 1MiB / (0.45 * 2e9 * 1.0s) ≈ 1.8e9 / 9e8 ≈ 2.0
	if math.Abs(roe-2.0) > 0.2 {
		t.Errorf("H rate-over-entitlement = %.3f, want ~2.0 (spilled ~2x its entitled share)", roe)
	}
	// A tenant that never spilled has 0 rate-over-entitlement.
	if z := b.RateOverEntitlement("L"); z != 0 {
		t.Errorf("L (no spills) rate-over-entitlement = %.3f, want 0", z)
	}
}
