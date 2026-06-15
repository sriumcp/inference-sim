package sim

import (
	"math"
	"testing"
)

func TestOccupancyMeter_IntegratesPerTenantResidency(t *testing.T) {
	m := NewOccupancyMeter()
	// Tenant H holds 10 blocks for 100µs; L holds 4 blocks for 100µs.
	// Left-endpoint Riemann: first tick seeds, second tick accumulates over delta.
	m.Tick(map[string]int{"H": 10, "L": 4}, 0)
	m.Tick(map[string]int{"H": 10, "L": 4}, 100)
	if got := m.ResidencyBlockUs("H"); math.Abs(got-1000) > 1e-9 { // 10 blocks * 100µs
		t.Errorf("H residency = %.1f, want 1000", got)
	}
	if got := m.ResidencyBlockUs("L"); math.Abs(got-400) > 1e-9 {
		t.Errorf("L residency = %.1f, want 400", got)
	}
}
