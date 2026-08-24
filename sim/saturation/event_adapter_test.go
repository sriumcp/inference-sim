// sim/saturation/event_adapter_test.go
package saturation

import (
	"testing"

	"github.com/inference-sim/inference-sim/sim"
)

// buildSortedEvents must carry token counts onto BOTH events.
//
// REGRESSION GUARD. These fields were previously left at zero, which silently
// disabled every token-aware detector: the work term w_i = kappa*I_i + O_i
// collapsed to 0, so the residual never left its floor and the busy-window ridge
// fit never saw a non-degenerate window. The detector then reported STABLE
// unconditionally -- indistinguishable, from the outside, from a healthy system.
//
// This is asserted as a LAW (no token count may be dropped by the adapter)
// rather than against golden values, so it survives a rewrite of the adapter.
func TestBuildSortedEvents_CarriesTokenCounts(t *testing.T) {
	reqs := []sim.RequestMetrics{
		{ID: "a", ArrivedAt: 1.0, E2E: 6000.0, NumPrefillTokens: 545, NumDecodeTokens: 517},
		{ID: "b", ArrivedAt: 2.0, E2E: 3000.0, NumPrefillTokens: 600, NumDecodeTokens: 400},
	}
	want := map[string][2]int{
		"a": {545, 517},
		"b": {600, 400},
	}
	events := buildSortedEvents(reqs)
	if len(events) != 2*len(reqs) {
		t.Fatalf("expected %d events, got %d", 2*len(reqs), len(events))
	}
	seen := map[string]int{}
	for _, e := range events {
		w, ok := want[e.RequestID]
		if !ok {
			t.Fatalf("unexpected request id %q", e.RequestID)
		}
		if e.InputTokens != w[0] {
			t.Errorf("%s (%v): InputTokens = %d, want %d", e.RequestID, e.Type, e.InputTokens, w[0])
		}
		if e.OutputTokens != w[1] {
			t.Errorf("%s (%v): OutputTokens = %d, want %d", e.RequestID, e.Type, e.OutputTokens, w[1])
		}
		seen[e.RequestID]++
	}
	for id, n := range seen {
		if n != 2 {
			t.Errorf("request %s produced %d events, want 2 (arrival + completion)", id, n)
		}
	}
}

// A token-aware detector fed through the REAL adapter must actually move its
// estimates off their seeded priors. This is the end-to-end version of the guard
// above: it fails if the adapter is ever changed to drop the token fields, even
// if the struct-level test above were somehow satisfied.
func TestWorkDrift_EstimatesMoveOffPriorsViaAdapter(t *testing.T) {
	// ~200 requests over ~20s of simulated time, realistic token shapes.
	reqs := make([]sim.RequestMetrics, 0, 200)
	for i := 0; i < 200; i++ {
		reqs = append(reqs, sim.RequestMetrics{
			ID:               itoa(i),
			ArrivedAt:        float64(i) * 0.1, // 10 req/s
			E2E:              6000.0,
			NumPrefillTokens: 512 + i%64,
			NumDecodeTokens:  256 + i%32,
		})
	}
	d := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.02, RDec0: 5000.0, Threshold: 5000.0,
		WindowSizeUs: 1_000_000, NumWindows: 20, ConsecutiveK: 3,
	})
	collector := NewInMemoryCollector()
	ReplayOneDetector(d, reqs, collector)

	res := d.Detect()
	if res.Signals["r_dec_hat"] == 5000.0 {
		t.Errorf("r_dec_hat never moved off its seeded prior (5000.0) — the online estimator is inert; is the adapter dropping token counts?")
	}
	if res.Signals["kappa_hat"] == 0.02 {
		t.Errorf("kappa_hat never moved off its prior (0.02) — §2e.6's stated failure tell for a prior-dominated estimator")
	}
	if len(collector.Records()) == 0 {
		t.Error("no trace records produced")
	}
}
