package sim

import "testing"

// TestMetrics_PerTenantE2E_SeparatesTenants verifies that E2E latencies are
// accumulated per-tenant, so a tenant's mean E2E can be computed over ITS OWN
// requests only — not the node-aggregate (which mixes tenants and is dominated
// by the heaviest one). This is the missing instrumentation behind the iter-2
// "L E2E" claim, which had no per-tenant source in the metrics output.
func TestMetrics_PerTenantE2E_SeparatesTenants(t *testing.T) {
	m := NewMetrics()

	// Two L requests (E2E 100, 200) and one H request (E2E 900).
	m.RecordTenantE2E("L", 100)
	m.RecordTenantE2E("L", 200)
	m.RecordTenantE2E("H", 900)

	lat := m.E2EsForTenant("L")
	if len(lat) != 2 {
		t.Fatalf("L E2E count = %d, want 2 (only L's requests)", len(lat))
	}
	var sum float64
	for _, v := range lat {
		sum += v
	}
	if mean := sum / float64(len(lat)); mean != 150 {
		t.Errorf("L mean E2E = %.0f, want 150 (node-aggregate would be 400)", mean)
	}
	if h := m.E2EsForTenant("H"); len(h) != 1 || h[0] != 900 {
		t.Errorf("H E2E = %v, want [900]", h)
	}
}
