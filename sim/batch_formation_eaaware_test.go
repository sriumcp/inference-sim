package sim

import (
	"math"
	"testing"
)

// TestNewBatchFormation_ValidatesSoftPreemptionWeight covers R3 — bad
// soft-preemption weights panic at construction.
func TestNewBatchFormation_ValidatesSoftPreemptionWeight(t *testing.T) {
	tests := []struct {
		name      string
		weight    float64
		wantPanic bool
	}{
		{name: "zero is valid (disabled)", weight: 0, wantPanic: false},
		{name: "small positive is valid", weight: 0.001, wantPanic: false},
		{name: "large positive is valid", weight: 100, wantPanic: false},
		{name: "negative panics", weight: -0.001, wantPanic: true},
		{name: "NaN panics", weight: math.NaN(), wantPanic: true},
		{name: "Inf panics", weight: math.Inf(1), wantPanic: true},
		{name: "-Inf panics", weight: math.Inf(-1), wantPanic: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if tt.wantPanic && r == nil {
					t.Errorf("expected panic, got none")
				}
				if !tt.wantPanic && r != nil {
					t.Errorf("unexpected panic: %v", r)
				}
			}()
			_ = NewBatchFormation("", tt.weight)
		})
	}
}

// TestSelectEAAwareVictim_PicksHighestExternalityOverAlpha verifies the
// selector picks the tenant with the largest cumExt / alpha.
func TestSelectEAAwareVictim_PicksHighestExternalityOverAlpha(t *testing.T) {
	v := &VLLMBatchFormation{}
	requests := []*Request{
		{ID: "r0", TenantID: "coop-0", SLOClass: "critical", ArrivalTime: 100},
		{ID: "r1", TenantID: "aggressor", SLOClass: "sheddable", ArrivalTime: 200},
		{ID: "r2", TenantID: "coop-1", SLOClass: "critical", ArrivalTime: 300},
	}
	tracker := &fakeTracker{
		cumExt: map[string]float64{
			"coop-0":    100, // /5 (critical) = 20
			"aggressor": 100, // /1 (sheddable→default) = 100
			"coop-1":    50,  // /5 = 10
		},
	}
	idx := v.selectEAAwareVictim(requests, tracker)
	if idx != 1 {
		t.Errorf("expected aggressor (idx=1) to be selected as victim, got idx=%d (req %s)", idx, requests[idx].ID)
	}
}

// TestSelectEAAwareVictim_TiebreakLatestArrival verifies that when two
// running requests share the same cumExt/alpha (e.g. both from the same
// tenant), the LATEST arrival is evicted — preserving more-invested
// earlier requests. Same tiebreak as PreemptionPriority.
func TestSelectEAAwareVictim_TiebreakLatestArrival(t *testing.T) {
	v := &VLLMBatchFormation{}
	requests := []*Request{
		{ID: "r-old", TenantID: "agg", SLOClass: "sheddable", ArrivalTime: 100},
		{ID: "r-mid", TenantID: "agg", SLOClass: "sheddable", ArrivalTime: 500},
		{ID: "r-new", TenantID: "agg", SLOClass: "sheddable", ArrivalTime: 999},
	}
	tracker := &fakeTracker{
		cumExt: map[string]float64{"agg": 100},
	}
	idx := v.selectEAAwareVictim(requests, tracker)
	if idx != 2 {
		t.Errorf("expected latest-arrival r-new (idx=2) on tie, got idx=%d (req %s)", idx, requests[idx].ID)
	}
}

// TestSelectEAAwareVictim_SingleRequest verifies the trivial case —
// only one running request → it must be the victim.
func TestSelectEAAwareVictim_SingleRequest(t *testing.T) {
	v := &VLLMBatchFormation{}
	requests := []*Request{
		{ID: "r0", TenantID: "any", SLOClass: "critical"},
	}
	tracker := &fakeTracker{}
	if idx := v.selectEAAwareVictim(requests, tracker); idx != 0 {
		t.Errorf("expected idx=0 for single-element batch, got %d", idx)
	}
}

// TestApplySoftPreemptionWeight_ZeroWeightIsPassthrough verifies that
// the helper is a no-op when softPreemptionWeight == 0.
func TestApplySoftPreemptionWeight_ZeroWeightIsPassthrough(t *testing.T) {
	v := &VLLMBatchFormation{softPreemptionWeight: 0}
	tracker := &fakeTracker{cumExt: map[string]float64{"any": 1000}}
	req := &Request{TenantID: "any", SLOClass: "critical"}
	got := v.applySoftPreemptionWeight(req, 100, tracker)
	if got != 100 {
		t.Errorf("expected passthrough (100), got %d", got)
	}
}

// TestApplySoftPreemptionWeight_NilTrackerIsPassthrough verifies that
// the helper handles nil tracker gracefully.
func TestApplySoftPreemptionWeight_NilTrackerIsPassthrough(t *testing.T) {
	v := &VLLMBatchFormation{softPreemptionWeight: 1.0}
	req := &Request{TenantID: "any", SLOClass: "critical"}
	got := v.applySoftPreemptionWeight(req, 100, nil /* tracker */)
	if got != 100 {
		t.Errorf("expected passthrough (100) on nil tracker, got %d", got)
	}
}

// TestApplySoftPreemptionWeight_ZeroExternalityIsPassthrough verifies
// that a fresh tenant (cumExt=0) gets full allocation — the mechanism
// only PUNISHES accumulated externality, never penalizes well-behaved
// new tenants.
func TestApplySoftPreemptionWeight_ZeroExternalityIsPassthrough(t *testing.T) {
	v := &VLLMBatchFormation{softPreemptionWeight: 1.0}
	tracker := &fakeTracker{cumExt: map[string]float64{}}
	req := &Request{TenantID: "fresh", SLOClass: "critical"}
	got := v.applySoftPreemptionWeight(req, 100, tracker)
	if got != 100 {
		t.Errorf("expected passthrough (100) for cumExt=0, got %d", got)
	}
}

// TestApplySoftPreemptionWeight_HighExternalityScalesDown verifies the
// formula: weight = 1 / (1 + softWeight * cumExt / alpha) reduces
// allocation proportionally.
func TestApplySoftPreemptionWeight_HighExternalityScalesDown(t *testing.T) {
	// softWeight=1, cumExt=99, alpha=1 (sheddable default)
	// → weight = 1 / (1 + 1*99/1) = 1/100 = 0.01
	// → 1000 tokens → floor(1000 * 0.01) = 10
	v := &VLLMBatchFormation{softPreemptionWeight: 1.0}
	tracker := &fakeTracker{cumExt: map[string]float64{"agg": 99}}
	req := &Request{TenantID: "agg", SLOClass: "sheddable"}
	got := v.applySoftPreemptionWeight(req, 1000, tracker)
	if got != 10 {
		t.Errorf("expected scaled allocation 10 (=1000/100), got %d", got)
	}
}

// TestApplySoftPreemptionWeight_AlphaScalesDownPenalty verifies that
// critical-class tenants (high alpha) are penalized LESS than
// sheddable-class tenants (low alpha) at the same cumExt — the alpha
// term in the denominator buys protection for high-priority classes.
func TestApplySoftPreemptionWeight_AlphaScalesDownPenalty(t *testing.T) {
	v := &VLLMBatchFormation{softPreemptionWeight: 1.0}
	tracker := &fakeTracker{cumExt: map[string]float64{
		"critical-tenant": 100,
		"sheddable-tenant": 100,
	}}
	critReq := &Request{TenantID: "critical-tenant", SLOClass: "critical"}
	shedReq := &Request{TenantID: "sheddable-tenant", SLOClass: "sheddable"}

	critTokens := v.applySoftPreemptionWeight(critReq, 1000, tracker)
	shedTokens := v.applySoftPreemptionWeight(shedReq, 1000, tracker)

	// critical alpha=5, weight = 1/(1+100/5) = 1/21 ≈ 0.0476 → 47
	// sheddable alpha=1, weight = 1/(1+100/1) = 1/101 ≈ 0.0099 → 9
	// critical must get more tokens than sheddable at same cumExt.
	if critTokens <= shedTokens {
		t.Errorf("critical should get MORE tokens than sheddable at same cumExt: crit=%d shed=%d", critTokens, shedTokens)
	}
	// Specific values (deterministic).
	if critTokens != 47 {
		t.Errorf("critical: expected 47 tokens (1/(1+100/5) * 1000), got %d", critTokens)
	}
	if shedTokens != 9 {
		t.Errorf("sheddable: expected 9 tokens (1/(1+100/1) * 1000), got %d", shedTokens)
	}
}

// TestApplySoftPreemptionWeight_FloorsAtOne (R19 circuit breaker)
// verifies that even under extreme externality, the helper never
// returns 0 — every running request must make at least 1 token of
// progress per step to avoid stranding indefinitely.
func TestApplySoftPreemptionWeight_FloorsAtOne(t *testing.T) {
	// Extreme weight + cumExt → naive multiplier would yield 0.
	v := &VLLMBatchFormation{softPreemptionWeight: 1e6}
	tracker := &fakeTracker{cumExt: map[string]float64{"agg": 1e6}}
	req := &Request{TenantID: "agg", SLOClass: "sheddable"}
	got := v.applySoftPreemptionWeight(req, 100, tracker)
	if got < 1 {
		t.Errorf("R19: must floor at 1 token to avoid starvation, got %d", got)
	}
	if got > 100 {
		t.Errorf("must never exceed input numNewTokens=100, got %d", got)
	}
}
