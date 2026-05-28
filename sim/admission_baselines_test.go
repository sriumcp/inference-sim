// Behavioral tests for iter-3 baseline admission policies.
// Mock at the policy seam (no live LLM calls); assert observable
// behavior through Admit() return values and internal state queries.

package sim

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// AIMD
// ---------------------------------------------------------------------------

func makeRouterStateWithKVUtil(util float64, clock int64) *RouterState {
	return &RouterState{
		Clock: clock,
		Snapshots: []RoutingSnapshot{
			{KVUtilization: util},
		},
	}
}

func TestAIMD_StartsFullyOpen(t *testing.T) {
	a := NewAIMDAdmission(0.05, 1e-6, 0.5, 0.9)
	if a.CurrentRate() != 1.0 {
		t.Fatalf("expected initial rate 1.0, got %v", a.CurrentRate())
	}
}

func TestAIMD_MultiplicativeDecreaseOnPressure(t *testing.T) {
	a := NewAIMDAdmission(0.05, 1e-6, 0.5, 0.9)
	state := makeRouterStateWithKVUtil(0.95, 1000) // above threshold
	req := &Request{ID: "r1", TenantID: "coop-1"}
	a.Admit(req, state)
	if a.CurrentRate() != 0.5 {
		t.Fatalf("expected rate to halve to 0.5, got %v", a.CurrentRate())
	}
	a.Admit(req, state)
	if a.CurrentRate() != 0.25 {
		t.Fatalf("expected rate to halve again to 0.25, got %v", a.CurrentRate())
	}
}

func TestAIMD_AdditiveIncreaseUnderSlack(t *testing.T) {
	a := NewAIMDAdmission(0.05, 1e-6, 0.5, 0.9)
	a.rate = 0.5 // start at half-open
	state := makeRouterStateWithKVUtil(0.5, 0) // below threshold
	req := &Request{ID: "r1", TenantID: "coop-1"}
	a.Admit(req, state)
	// Initial admit at clock=0 sets lastUpdateTick=0, elapsed=0 ⇒ no growth yet.
	state.Clock = 1_000_000 // 1 s later
	a.Admit(req, state)
	// elapsed=1e6, increase = 1e-6 * 1e6 = 1.0; rate clamps to 1.0.
	if a.CurrentRate() != 1.0 {
		t.Fatalf("expected rate to reach 1.0 after 1s of slack, got %v", a.CurrentRate())
	}
}

func TestAIMD_RateFloorEnforced(t *testing.T) {
	a := NewAIMDAdmission(0.1, 1e-6, 0.5, 0.9)
	state := makeRouterStateWithKVUtil(0.95, 1000)
	req := &Request{ID: "r1", TenantID: "coop-1"}
	for i := 0; i < 20; i++ {
		a.Admit(req, state)
	}
	if a.CurrentRate() < 0.1 {
		t.Fatalf("expected rate floor 0.1 enforced, got %v", a.CurrentRate())
	}
}

func TestAIMD_AdmissionDeterministicByRequestID(t *testing.T) {
	// Same request ID + same rate ⇒ same admit decision (reproducibility).
	a := NewAIMDAdmission(0.05, 1e-6, 0.5, 0.9)
	a.rate = 0.5
	state := makeRouterStateWithKVUtil(0.5, 0) // no rate change
	req := &Request{ID: "deterministic-id", TenantID: "coop-1"}

	first, _ := a.Admit(req, state)
	a.rate = 0.5 // reset rate (Admit may have updated it)
	second, _ := a.Admit(req, state)
	if first != second {
		t.Fatalf("AIMD should be reproducible on same id+rate; got %v then %v", first, second)
	}
}

func TestAIMD_RejectionReasonMentionsRate(t *testing.T) {
	a := NewAIMDAdmission(0.05, 1e-6, 0.5, 0.9)
	a.rate = 0.0 // forced rejection
	a.rateFloor = 0.0
	state := makeRouterStateWithKVUtil(0.5, 0)
	req := &Request{ID: "r1", TenantID: "coop-1"}
	admitted, reason := a.Admit(req, state)
	if admitted {
		t.Fatalf("expected rejection at rate=0")
	}
	if !strings.Contains(reason, "aimd") || !strings.Contains(reason, "rate=") {
		t.Fatalf("rejection reason should mention 'aimd' and 'rate=', got %q", reason)
	}
}

func TestNewAIMDAdmission_PanicsOnInvalidParams(t *testing.T) {
	cases := []struct {
		name                                    string
		rateFloor, incStep, decFactor, pressure float64
	}{
		{"zero rate floor", 0, 1e-6, 0.5, 0.9},
		{"negative incStep", 0.05, -1, 0.5, 0.9},
		{"decFactor >= 1", 0.05, 1e-6, 1.0, 0.9},
		{"zero pressure threshold", 0.05, 1e-6, 0.5, 0},
		{"pressure threshold > 1", 0.05, 1e-6, 0.5, 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for %s", tc.name)
				}
			}()
			NewAIMDAdmission(tc.rateFloor, tc.incStep, tc.decFactor, tc.pressure)
		})
	}
}

// ---------------------------------------------------------------------------
// AWT
// ---------------------------------------------------------------------------

func TestAWT_AdmitsBelowQuota(t *testing.T) {
	a := NewAWTAdmission(10_000_000, 5)
	state := &RouterState{Clock: 1000}
	req := &Request{ID: "r1", TenantID: "coop-1"}
	for i := 0; i < 5; i++ {
		admitted, _ := a.Admit(req, state)
		if !admitted {
			t.Fatalf("expected admit %d/5, got rejection", i+1)
		}
	}
}

func TestAWT_RejectsWhenQuotaExhausted(t *testing.T) {
	a := NewAWTAdmission(10_000_000, 3)
	state := &RouterState{Clock: 1000}
	req := &Request{ID: "r1", TenantID: "noisy-tenant"}
	for i := 0; i < 3; i++ {
		a.Admit(req, state)
	}
	admitted, reason := a.Admit(req, state)
	if admitted {
		t.Fatalf("expected rejection at quota=3+1, got admit")
	}
	if !strings.Contains(reason, "noisy-tenant") {
		t.Fatalf("rejection reason should mention tenant ID, got %q", reason)
	}
}

func TestAWT_WindowSlide(t *testing.T) {
	a := NewAWTAdmission(1_000_000, 2) // 1s window, quota 2
	req := &Request{ID: "r1", TenantID: "coop-1"}

	// Burst at t=0: 2 admits, 3rd reject.
	state := &RouterState{Clock: 0}
	a.Admit(req, state)
	a.Admit(req, state)
	if admitted, _ := a.Admit(req, state); admitted {
		t.Fatal("expected rejection after quota=2")
	}

	// Wait past window: window slides, fresh quota.
	state.Clock = 2_000_000 // 2s — past 1s window
	if admitted, _ := a.Admit(req, state); !admitted {
		t.Fatal("expected admit after window slide")
	}
}

func TestAWT_PerTenantIsolation(t *testing.T) {
	a := NewAWTAdmission(10_000_000, 2)
	state := &RouterState{Clock: 1000}
	for i := 0; i < 2; i++ {
		a.Admit(&Request{ID: "r", TenantID: "tenant-a"}, state)
	}
	// tenant-a is now at quota; tenant-b should still admit.
	if admitted, _ := a.Admit(&Request{ID: "r", TenantID: "tenant-b"}, state); !admitted {
		t.Fatal("AWT should isolate tenants; tenant-b should admit despite tenant-a saturation")
	}
}

func TestAWT_NoTenantIDAdmits(t *testing.T) {
	// Without a tenant ID, AWT cannot enforce. Default is admit.
	a := NewAWTAdmission(10_000_000, 1)
	state := &RouterState{Clock: 1000}
	req := &Request{ID: "r1", TenantID: ""}
	for i := 0; i < 5; i++ {
		admitted, _ := a.Admit(req, state)
		if !admitted {
			t.Fatalf("expected admit when tenant ID is empty, got rejection")
		}
	}
}

func TestNewAWTAdmission_PanicsOnInvalidParams(t *testing.T) {
	cases := []struct {
		name   string
		window int64
		quota  int
	}{
		{"zero window", 0, 5},
		{"negative window", -1, 5},
		{"zero quota", 1000, 0},
		{"negative quota", 1000, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for %s", tc.name)
				}
			}()
			NewAWTAdmission(tc.window, tc.quota)
		})
	}
}

// ---------------------------------------------------------------------------
// Oracle
// ---------------------------------------------------------------------------

func TestOracle_AdmitsCooperators(t *testing.T) {
	o := NewOracleAdmission("coop", "aggressor")
	state := &RouterState{Clock: 1000}
	for _, tid := range []string{"coop-1", "coop-2", "coop-aggressive-name"} {
		req := &Request{ID: "r", TenantID: tid}
		if admitted, _ := o.Admit(req, state); !admitted {
			t.Fatalf("expected oracle to admit cooperator %q", tid)
		}
	}
}

func TestOracle_RejectsAggressor(t *testing.T) {
	o := NewOracleAdmission("coop", "aggressor")
	state := &RouterState{Clock: 1000}
	req := &Request{ID: "r", TenantID: "aggressor"}
	admitted, reason := o.Admit(req, state)
	if admitted {
		t.Fatalf("expected oracle to reject aggressor")
	}
	if !strings.Contains(reason, "aggressor") {
		t.Fatalf("rejection reason should mention 'aggressor', got %q", reason)
	}
}

func TestOracle_AdmitsUnknownTenants(t *testing.T) {
	// Don't punish what you can't classify (conservative oracle).
	o := NewOracleAdmission("coop", "aggressor")
	state := &RouterState{Clock: 1000}
	req := &Request{ID: "r", TenantID: "unrelated"}
	if admitted, _ := o.Admit(req, state); !admitted {
		t.Fatal("expected oracle to admit unknown tenants (conservative)")
	}
}

func TestOracle_DefaultsForEmptyArgs(t *testing.T) {
	o := NewOracleAdmission("", "")
	if o.CooperatorPrefix != "coop" || o.AggressorTenantID != "aggressor" {
		t.Fatalf("expected default prefixes, got %q / %q",
			o.CooperatorPrefix, o.AggressorTenantID)
	}
}

// ---------------------------------------------------------------------------
// Factory wiring
// ---------------------------------------------------------------------------

func TestNewAdmissionPolicy_OracleWiresCorrectly(t *testing.T) {
	p := NewAdmissionPolicy("oracle", 0, 0)
	if _, ok := p.(*OracleAdmission); !ok {
		t.Fatalf("expected *OracleAdmission, got %T", p)
	}
}

func TestNewAdmissionPolicy_AIMDPanicsForGenericFactory(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from generic factory for aimd")
		}
	}()
	NewAdmissionPolicy("aimd", 0, 0)
}

func TestNewAdmissionPolicy_AWTPanicsForGenericFactory(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from generic factory for awt")
		}
	}()
	NewAdmissionPolicy("awt", 0, 0)
}

func TestIsValidAdmissionPolicy_RecognizesNewPolicies(t *testing.T) {
	for _, name := range []string{"aimd", "awt", "oracle"} {
		if !IsValidAdmissionPolicy(name) {
			t.Errorf("%q should be a valid admission policy", name)
		}
	}
}
