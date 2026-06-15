// sim/occupancy_meter.go
package sim

// OccupancyMeter accumulates per-tenant HBM residency over time on a SINGLE capacity
// axis (HBM blocks). A_i(T) = ∫ k_i(s) ds via left-endpoint Riemann sum, where k_i(s)
// is the HBM blocks held by tenant i at time s. This is the capacity side of the meter
// dissociation: capacity meters score tenants by this residency, blind to the flow axis.
// Re-axed from the two-pool (prefill/decode) reference meter to a single HBM axis.
type OccupancyMeter struct {
	residencyBlockUs map[string]float64 // tenant → ∫ blocks dt (block·µs)
	lastBlocks       map[string]int     // blocks held per tenant at the previous tick
	lastTickUs       int64
	started          bool
}

func NewOccupancyMeter() *OccupancyMeter {
	return &OccupancyMeter{
		residencyBlockUs: make(map[string]float64),
		lastBlocks:       make(map[string]int),
	}
}

// Tick records per-tenant HBM block counts at nowUs and accumulates the residency
// integral over [lastTickUs, nowUs) using the PREVIOUS tick's counts (left-endpoint).
func (m *OccupancyMeter) Tick(tenantBlocks map[string]int, nowUs int64) {
	if m.started && nowUs > m.lastTickUs {
		delta := float64(nowUs - m.lastTickUs)
		for tenant, blocks := range m.lastBlocks {
			m.residencyBlockUs[tenant] += float64(blocks) * delta
		}
	}
	m.lastBlocks = make(map[string]int, len(tenantBlocks))
	for tenant, blocks := range tenantBlocks {
		m.lastBlocks[tenant] = blocks
	}
	m.lastTickUs = nowUs
	m.started = true
}

// ResidencyBlockUs returns tenant i's accumulated HBM residency ∫ k_i dt (block·µs).
func (m *OccupancyMeter) ResidencyBlockUs(tenantID string) float64 {
	return m.residencyBlockUs[tenantID]
}
