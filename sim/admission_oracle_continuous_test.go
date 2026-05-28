// Behavioral tests for ContinuousRateOracleAdmission.

package sim

import (
	"strings"
	"testing"
)

func TestContinuousRateOracle_AdmitsCooperators(t *testing.T) {
	o := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	state := makeRouterStateWithKVUtil(0.95, 1000) // high pressure
	req := &Request{ID: "r", TenantID: "coop-1"}
	if admitted, _ := o.Admit(req, state); !admitted {
		t.Fatal("cooperator must always be admitted regardless of pressure")
	}
}

func TestContinuousRateOracle_FixedAggressorRate(t *testing.T) {
	// Default aggressor admission probability is 0.1.
	// With 1000 deterministic-hash samples, observed admit rate should
	// be ~0.1 ± a few percent.
	o := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	state := makeRouterStateWithKVUtil(0.5, 1000)
	admits := 0
	for i := 0; i < 1000; i++ {
		req := &Request{ID: "agg-" + string(rune(i)) + string(rune(i*7)), TenantID: "aggressor"}
		if admitted, _ := o.Admit(req, state); admitted {
			admits++
		}
	}
	rate := float64(admits) / 1000
	if rate < 0.07 || rate > 0.13 {
		t.Fatalf("expected admit rate ~0.1 (default), got %.3f", rate)
	}
}

func TestContinuousRateOracle_PressureIndependent(t *testing.T) {
	// Fixed-rate oracle is pressure-independent: same admit rate at any kvUtil.
	o := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	for _, util := range []float64{0.1, 0.5, 0.9} {
		state := makeRouterStateWithKVUtil(util, 1000)
		admits := 0
		for i := 0; i < 500; i++ {
			req := &Request{ID: "agg-" + string(rune(i)) + string(rune(i*7)), TenantID: "aggressor"}
			if admitted, _ := o.Admit(req, state); admitted {
				admits++
			}
		}
		rate := float64(admits) / 500
		if rate < 0.07 || rate > 0.13 {
			t.Fatalf("at kvUtil=%v: expected ~0.1 admit rate, got %.3f", util, rate)
		}
	}
}

func TestContinuousRateOracle_Deterministic(t *testing.T) {
	o1 := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	o2 := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	state := makeRouterStateWithKVUtil(0.7, 1000)
	for i := 0; i < 50; i++ {
		req := &Request{ID: "deterministic-" + string(rune(i)), TenantID: "aggressor"}
		a1, _ := o1.Admit(req, state)
		a2, _ := o2.Admit(req, state)
		if a1 != a2 {
			t.Fatalf("non-deterministic for id=%s: %v vs %v", req.ID, a1, a2)
		}
	}
}

func TestContinuousRateOracle_RejectionMessage(t *testing.T) {
	o := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	state := makeRouterStateWithKVUtil(0.95, 1000)
	req := &Request{ID: "r", TenantID: "aggressor"}
	admitted, reason := o.Admit(req, state)
	if admitted {
		t.Fatal("expected rejection at high pressure")
	}
	if !strings.Contains(reason, "oracle-cont") || !strings.Contains(reason, "kvUtil=") {
		t.Fatalf("rejection should mention 'oracle-cont' and 'kvUtil=', got %q", reason)
	}
}

func TestContinuousRateOracle_UnknownTenantsAdmitted(t *testing.T) {
	o := NewContinuousRateOracleAdmission("coop", "aggressor", 0.5, 0.85)
	state := makeRouterStateWithKVUtil(0.95, 1000)
	req := &Request{ID: "r", TenantID: "stranger"}
	if admitted, _ := o.Admit(req, state); !admitted {
		t.Fatal("unknown tenants should always be admitted (conservative)")
	}
}

func TestNewContinuousRateOracleAdmission_PanicsOnInvalid(t *testing.T) {
	cases := []struct {
		name           string
		low, high      float64
	}{
		{"low < 0", -0.1, 0.85},
		{"low >= 1", 1.0, 1.0},
		{"high <= low", 0.5, 0.5},
		{"high > 1", 0.5, 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for %s", tc.name)
				}
			}()
			NewContinuousRateOracleAdmission("coop", "aggressor", tc.low, tc.high)
		})
	}
}
