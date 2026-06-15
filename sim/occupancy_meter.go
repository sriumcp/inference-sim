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
	firstTickUs      int64
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
	if !m.started {
		m.firstTickUs = nowUs
	}
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

// elapsedUs returns the total sim time spanned by ticks so far (last − first).
func (m *OccupancyMeter) elapsedUs() float64 { return float64(m.lastTickUs - m.firstTickUs) }

// StaticDRFShare: instantaneous HBM occupancy share (snapshot, time-naive).
func (m *OccupancyMeter) StaticDRFShare(tenantID string, totalBlocks int) float64 {
	if totalBlocks <= 0 {
		return 0
	}
	return float64(m.lastBlocks[tenantID]) / float64(totalBlocks)
}

// SDRFShare: time-integrated HBM occupancy share = ∫k_i dt / (totalBlocks · elapsed).
// The strongest TIME-AWARE capacity meter; still blind to the flow axis.
func (m *OccupancyMeter) SDRFShare(tenantID string, totalBlocks int) float64 {
	denom := float64(totalBlocks) * m.elapsedUs()
	if denom <= 0 {
		return 0
	}
	return m.residencyBlockUs[tenantID] / denom
}

// KVtimeCapacityShare: residency-meter view of capacity. On a single HBM axis this is
// identical to SDRF's integrated dominant share — the point being that NO capacity meter,
// however time-aware, sees the flow axis.
func (m *OccupancyMeter) KVtimeCapacityShare(tenantID string, totalBlocks int) float64 {
	return m.SDRFShare(tenantID, totalBlocks)
}
