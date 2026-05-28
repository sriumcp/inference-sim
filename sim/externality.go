package sim

// TenantExternalityTracker exposes the per-tenant externality state to
// control-plane policies (admission, preemption, batch formation) so they
// can price requests by EA-awareness without privately re-implementing
// the simulator's accounting.
//
// The same accumulator drives multiple actuators at different time scales:
//
//   - Scheduling (millisecond): WaitQueue.Reorder reads CumExternality to
//     order queued requests. See ea-wfq case in Simulator.Reorder.
//   - Admission (seconds): EAAwareTokenBucket reads KVPressureSignal +
//     per-request kappa to price entry. See sim/admission.go.
//   - Preemption hard (per-step): EA-aware victim selection reads
//     CumExternality / alphaForSLO over the running batch. See
//     selectEAAwareVictim in sim/batch_formation.go.
//   - Preemption soft (per-step): per-tenant decode-token weighting
//     proportionally slows high-externality tenants without evicting
//     them. See applySoftPreemptionWeight in sim/batch_formation.go.
//
// Implementations live alongside the state owners: the per-instance
// Simulator implements the tracker for its local tenantCumExternality
// map; the cluster ClusterSimulator aggregates across instances.
//
// Nil-safety contract: callers may pass a nil tracker — every consumer
// checks for nil and degrades to its baseline behavior (no EA-awareness,
// equivalent to the pre-existing policy without this contract). This
// matches the SLOPriorityMap nil-safety pattern in NewTierShedAdmission.
type TenantExternalityTracker interface {
	// CumExternality returns the running externality counter for tenant
	// tid. Returns 0 for unknown tenants (counter starts at 0; the map
	// access in sim.tenantCumExternality is also a zero default for
	// missing keys, so this is a deterministic identity).
	CumExternality(tenantID string) float64

	// KVPressureSignal returns a scalar in [0, 1] indicating cache-jam
	// pressure RIGHT NOW. Mirrors the gate condition the simulator's
	// μ̂ formula uses (kvUtil > 0.9 && WaitQ non-empty), but normalized
	// to a 0..1 range that downstream consumers can multiply against
	// directly.
	//
	// Returns 0 when:
	//   - kvUtil <= 0.9 (cache has slack), OR
	//   - the wait queue is empty (no contention)
	//
	// Returns (kvUtil - 0.9) / 0.1 when both conditions are true, capped
	// at 1.0.
	//
	// Why a separate scalar (not raw kvUtil): admission policies want a
	// THRESHOLDED signal that is zero in unloaded regimes (so they don't
	// add overhead at low load) and rises smoothly above the threshold
	// (so behavior changes are continuous, not bang-bang).
	KVPressureSignal() float64
}

// kvPressureFromUtil computes the standard pressure signal from a
// kvUtilization fraction. Shared helper so per-instance Simulator and
// cluster aggregator produce consistent values.
//
// Pressure thresholds the same way the simulator's μ̂ does (0.9 cutoff)
// to keep the admission/preemption layers semantically aligned with the
// scheduler's existing behavior.
func kvPressureFromUtil(kvUtil float64, waitQNonEmpty bool) float64 {
	if !waitQNonEmpty {
		return 0
	}
	if kvUtil <= 0.9 {
		return 0
	}
	pressure := (kvUtil - 0.9) / 0.1
	if pressure > 1.0 {
		return 1.0
	}
	return pressure
}
