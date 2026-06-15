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

func TestOccupancyMeter_CapacityScores(t *testing.T) {
	m := NewOccupancyMeter()
	totalBlocks := 100
	m.Tick(map[string]int{"H": 10, "L": 40}, 0)
	m.Tick(map[string]int{"H": 10, "L": 40}, 1000)
	// static-DRF: instantaneous share — H=10/100=0.10, L=40/100=0.40.
	if s := m.StaticDRFShare("H", totalBlocks); math.Abs(s-0.10) > 1e-9 {
		t.Errorf("static-DRF H = %.3f, want 0.10", s)
	}
	// SDRF time-integrated share over elapsed 1000µs: H = (10*1000)/(100*1000) = 0.10.
	if s := m.SDRFShare("H", totalBlocks); math.Abs(s-0.10) > 1e-9 {
		t.Errorf("SDRF H = %.3f, want 0.10", s)
	}
	// vector-KVtime capacity view coincides with SDRF on a single axis.
	if math.Abs(m.KVtimeCapacityShare("H", totalBlocks)-m.SDRFShare("H", totalBlocks)) > 1e-9 {
		t.Errorf("KVtime capacity view must equal SDRF on single axis")
	}
	// The dissociation precondition: H is LIGHTER than L on every capacity meter.
	if m.SDRFShare("H", totalBlocks) >= m.SDRFShare("L", totalBlocks) {
		t.Errorf("expected H lighter than L on SDRF (the stock-light precondition)")
	}
}
