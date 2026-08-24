// sim/saturation/workdrift_test.go
//
// Behavioral tests for the work-conservation residual detectors (SWD/OWD),
// specified in ../papers/saturation/detection_strategies.md §2e.
//
// Per §2e.7's "Suggested tests" table these assert LAWS, not golden values:
// each test states a property the construction must satisfy for any input, so it
// survives a full reimplementation (CLAUDE.md BDD/TDD rule 5, refactor survival).
package saturation

import (
	"math"
	"testing"
)

// mkArrival / mkCompletion build the two events the detector consumes. Work is
// token-based (kappa*I + O) per §2e.5, so prefill/decode counts are the payload.
func mkArrival(tsUs int64, id string, in, out int) Event {
	return Event{Timestamp: tsUs, Type: Arrival, RequestID: id, InputTokens: in, OutputTokens: out}
}

func mkCompletion(tsUs int64, id string, in, out int, e2eMs float64) Event {
	return Event{Timestamp: tsUs, Type: Completion, RequestID: id, InputTokens: in, OutputTokens: out, LatencyMs: e2eMs}
}

// LAW 1 (§2e.7): the residual R is never negative — the max(0,·) floor is the
// reflecting boundary that makes R a Lindley recursion.
func TestWorkDrift_ResidualNeverNegative(t *testing.T) {
	for _, name := range []string{"swd", "owd"} {
		t.Run(name, func(t *testing.T) {
			d := newTestWorkDrift(t, name)
			// Deliberately starve the detector: tiny work, huge inter-arrival
			// gaps, so w - rdec*dt is strongly negative every step.
			for i := 0; i < 200; i++ {
				ts := int64(i) * 10_000_000 // 10s apart
				d.Observe(mkArrival(ts, "r", 1, 1))
				d.Observe(mkCompletion(ts+1000, "r", 1, 1, 1.0))
				got := d.Detect().Signals["residual"]
				if got < 0 {
					t.Fatalf("residual went negative at step %d: %v", i, got)
				}
			}
		})
	}
}

// LAW 2 (§2e.7 + metamorphic_tests.md §5.1/§5.2): the T4 ratchet. Under a
// burst-lull cycle at a HELD over-capacity MEAN, a detector that has climbed
// must not be zeroed by the lull trough.
//
// §5.2 is explicit that "a burst followed by a lull is not a reduction in load;
// it is the variance we put there, same mean throughout" -- so the lull here
// keeps the SAME mean offered load, concentrating it differently in time. An
// earlier version of this test drove 40s of near-idle traffic, which is a
// capacity REDUCTION, not a lull: the queue genuinely drained and reaching zero
// was correct behavior, not a T4 violation.
func TestWorkDrift_LullDoesNotZeroResidual(t *testing.T) {
	d := newTestWorkDrift(t, "owd")
	// Mean offered work must exceed drain capacity throughout. rdec0 = 5000
	// decode-token equiv/s; each request carries kappa*2048 + 512 ~= 553.
	// At one request per 50ms that is ~11060/s offered vs 5000/s drained:
	// a genuinely super-capacity mean.
	ts := int64(0)
	burstLull := func(cycles int) {
		for c := 0; c < cycles; c++ {
			// BURST: 15 requests tightly packed (10ms apart).
			for i := 0; i < 15; i++ {
				ts += 10_000
				d.Observe(mkArrival(ts, "r", 2048, 512))
				d.Observe(mkCompletion(ts+5_000, "r", 2048, 512, 5.0))
			}
			// LULL: 3 requests spread out (150ms apart). Same 18 requests per
			// ~600ms cycle => mean preserved, variance injected.
			for i := 0; i < 3; i++ {
				ts += 150_000
				d.Observe(mkArrival(ts, "r", 2048, 512))
				d.Observe(mkCompletion(ts+5_000, "r", 2048, 512, 5.0))
			}
		}
	}
	burstLull(10)
	peak := d.Detect().Signals["residual"]
	if peak <= 0 {
		t.Fatalf("expected positive residual under super-capacity mean, got %v", peak)
	}
	// Track the residual across every lull trough of ten further cycles: the
	// ratchet claim is that no trough returns it to zero.
	minTrough := math.Inf(1)
	for c := 0; c < 10; c++ {
		burstLull(1)
		r := d.Detect().Signals["residual"]
		if r < minTrough {
			minTrough = r
		}
	}
	if minTrough <= 0 {
		t.Errorf("a lull trough zeroed the residual (min=%v); T4 ratchet violated", minTrough)
	}
}

// LAW 3 (§2e.7, Proposition 4): with kappa == 0 the statistic must ignore the
// prefill term entirely — it reduces to VWD. Two streams differing ONLY in input
// token count must then produce identical residuals.
func TestWorkDrift_KappaZeroReducesToVWD(t *testing.T) {
	run := func(in int) float64 {
		d := newWorkDriftForTest(workDriftConfig{
			Kind: "owd", Kappa0: 0.0, FreezeKappa: true,
			RDec0: 5000.0, Threshold: 1e9, WindowSizeUs: 1_000_000, ConsecutiveK: 3,
		})
		for i := 0; i < 100; i++ {
			ts := int64(i) * 100_000
			d.Observe(mkArrival(ts, "r", in, 128))
			d.Observe(mkCompletion(ts+5_000, "r", in, 128, 5.0))
		}
		return d.Detect().Signals["residual"]
	}
	lo, hi := run(64), run(8192)
	if math.Abs(lo-hi) > 1e-9 {
		t.Errorf("kappa=0 must ignore input tokens, but residual differed: I=64 -> %v, I=8192 -> %v", lo, hi)
	}
}

// LAW 4 (§2e.7): the ridge fit must not produce NaN/Inf on degenerate input
// where a plain least-squares fit is singular (all windows an identical mix, so
// the two columns are collinear).
func TestWorkDrift_RidgeFitNoNaNOnSingularInput(t *testing.T) {
	d := newTestWorkDrift(t, "owd")
	// Every window carries an identical prefill:decode ratio => collinear design.
	for i := 0; i < 400; i++ {
		ts := int64(i) * 100_000
		d.Observe(mkArrival(ts, "r", 512, 128))
		d.Observe(mkCompletion(ts+50_000, "r", 512, 128, 50.0))
	}
	res := d.Detect()
	for _, k := range []string{"residual", "r_dec_hat", "kappa_hat", "drift_est"} {
		v := res.Signals[k]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("signal %q is not finite on singular input: %v", k, v)
		}
	}
	if res.Signals["r_dec_hat"] <= 0 {
		t.Errorf("r_dec_hat must stay positive, got %v", res.Signals["r_dec_hat"])
	}
}

// LAW 5 (§2e.7 + §2e.6 T3 mechanism): when service capacity collapses while the
// OFFERED WORKLOAD IS HELD BYTE-IDENTICAL, the residual must rise. This is the
// test that distinguishes a drift detector from a pinned-rate one.
func TestWorkDrift_RateCollapseRaisesResidual(t *testing.T) {
	// Identical arrival pattern and token counts in both legs; only the
	// service rate (expressed via completion latency / throughput) differs.
	feed := func(slowdown float64) float64 {
		d := newTestWorkDrift(t, "owd")
		for i := 0; i < 400; i++ {
			ts := int64(i) * 100_000
			d.Observe(mkArrival(ts, "r", 512, 256))
			// A collapse means each request occupies the engine longer.
			svc := int64(float64(60_000) * slowdown)
			d.Observe(mkCompletion(ts+svc, "r", 512, 256, float64(svc)/1000.0))
		}
		return d.Detect().Signals["residual"]
	}
	healthy := feed(1.0)
	collapsed := feed(3.0)
	if !(collapsed > healthy) {
		t.Errorf("capacity collapse must raise the residual: healthy=%v collapsed=%v", healthy, collapsed)
	}
}

// LAW 6: determinism (INV-6). The same event sequence must produce a
// byte-identical signal map — no map iteration, no clock reads, no randomness.
func TestWorkDrift_Deterministic(t *testing.T) {
	run := func() Result {
		d := newTestWorkDrift(t, "owd")
		for i := 0; i < 250; i++ {
			ts := int64(i) * 77_777
			d.Observe(mkArrival(ts, "r", 300+i, 100+i/2))
			d.Observe(mkCompletion(ts+40_000, "r", 300+i, 100+i/2, 40.0))
		}
		return d.Detect()
	}
	a, b := run(), run()
	if a.Level != b.Level || a.Score != b.Score || a.Confidence != b.Confidence {
		t.Fatalf("non-deterministic verdict: %+v vs %+v", a, b)
	}
	for k, va := range a.Signals {
		if vb, ok := b.Signals[k]; !ok || va != vb {
			t.Errorf("signal %q differs across identical runs: %v vs %v", k, va, vb)
		}
	}
}

// LAW 7: Reset returns the detector to its initial state (Detector contract).
func TestWorkDrift_ResetRestoresInitialState(t *testing.T) {
	d := newTestWorkDrift(t, "owd")
	fresh := d.Detect()
	for i := 0; i < 100; i++ {
		ts := int64(i) * 50_000
		d.Observe(mkArrival(ts, "r", 4096, 512))
		d.Observe(mkCompletion(ts+10_000, "r", 4096, 512, 10.0))
	}
	d.Reset()
	after := d.Detect()
	if after.Level != fresh.Level || after.Signals["residual"] != fresh.Signals["residual"] {
		t.Errorf("Reset did not restore initial state: fresh=%+v after=%+v", fresh, after)
	}
}

// LAW 8 (INV-9 oracle boundary): the detector is completion-indexed by default
// (§2e.5 causality obligation), so an arrival whose completion has NOT been
// observed must not move the residual. This is what keeps it from peeking at
// Request.OutputTokens before the tokens exist.
func TestWorkDrift_CompletionIndexed_ArrivalAloneDoesNotMoveResidual(t *testing.T) {
	d := newTestWorkDrift(t, "owd")
	before := d.Detect().Signals["residual"]
	for i := 0; i < 50; i++ {
		d.Observe(mkArrival(int64(i)*10_000, "r", 8192, 4096))
	}
	after := d.Detect().Signals["residual"]
	if after != before {
		t.Errorf("completion-indexed detector moved on arrivals alone: %v -> %v", before, after)
	}
}

// newTestWorkDrift builds a detector with a small window so a few hundred
// directly-fed events span enough windows to exercise the online estimator
// (same rationale as NewBacklogDriftDetectorWithConfig's doc comment).
func newTestWorkDrift(t *testing.T, kind string) Detector {
	t.Helper()
	return newWorkDriftForTest(workDriftConfig{
		Kind: kind, Kappa0: 0.02, RDec0: 5000.0,
		Threshold:    1e9, // effectively never fires; these laws test the statistic
		WindowSizeUs: 1_000_000, ConsecutiveK: 3,
	})
}
