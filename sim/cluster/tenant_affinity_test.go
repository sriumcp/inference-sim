package cluster

import (
	"testing"

	"github.com/inference-sim/inference-sim/sim"
)

// TestTenantAffinity_BuildRouterState_FiltersByTenant verifies that when
// instances are tenant-affined (inst.TenantAffinity set), a request from a
// given tenant routes ONLY to that tenant's instances.
//
// This is the apparatus fix for the node-shared-SSD experiment: H and L must
// not co-batch on the same instance (BLIS is batch-synchronous, so a shared
// step-advance would let H's spill-deferral latency contaminate L's TTFT).
// Pinning each tenant to a disjoint instance subset — while all instances still
// share one SharedSpillBus — eliminates that contamination. Mirrors the proven
// model-filter path (T048).
func TestTenantAffinity_BuildRouterState_FiltersByTenant(t *testing.T) {
	// 2 instances affined to tenant "H", 2 to tenant "L".
	cfg := newTestDeploymentConfigWithModel(4, "llama")
	cs := NewClusterSimulator(cfg, nil, nil)
	cs.instances[0].TenantAffinity = "H"
	cs.instances[1].TenantAffinity = "H"
	cs.instances[2].TenantAffinity = "L"
	cs.instances[3].TenantAffinity = "L"

	hReq := &sim.Request{ID: "req-h", TenantID: "H", ArrivalTime: 0,
		InputTokens: []int{1, 2, 3}, OutputTokens: []int{1, 2}, State: sim.StateQueued}
	state := buildRouterState(cs, hReq)

	if len(state.Snapshots) != 2 {
		t.Fatalf("got %d routable snapshots for tenant-H request, want 2 (only H-affined instances)", len(state.Snapshots))
	}
	for _, snap := range state.Snapshots {
		id := snap.ID
		if id != string(cs.instances[0].ID()) && id != string(cs.instances[1].ID()) {
			t.Errorf("tenant-H request routed to non-H instance %q", id)
		}
	}
}

// TestTenantAffinity_ConfigAssignsAffinities verifies that DeploymentConfig.TenantAffinities
// is applied to the constructed instances (the CLI-reachable assignment path).
func TestTenantAffinity_ConfigAssignsAffinities(t *testing.T) {
	cfg := newTestDeploymentConfigWithModel(4, "llama")
	cfg.TenantAffinities = []string{"H", "H", "L", "L"}
	cs := NewClusterSimulator(cfg, nil, nil)

	want := []string{"H", "H", "L", "L"}
	if len(cs.instances) != 4 {
		t.Fatalf("got %d instances, want 4", len(cs.instances))
	}
	for i, inst := range cs.instances {
		if inst.TenantAffinity != want[i] {
			t.Errorf("instance %d TenantAffinity = %q, want %q", i, inst.TenantAffinity, want[i])
		}
	}
}
