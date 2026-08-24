// sim/saturation/workdrift_metamorphic_test.go
//
// The DISCRIMINATING tests. §2e.6 of detection_strategies.md commits, in
// advance, to a predicted scorecard: SWD/OWD should PASS T2-IN and T3 where a
// fixed-rate / level-based detector FAILS. §3b makes the stakes explicit:
//
//	"T2-IN and T3 must flip relative to fixed-r_dec VWD. If VWD passes those
//	 ladders too, the prefill term and the adaptive rate are not earning their
//	 complexity and the paper's claim collapses to 'VWD, restated.'"
//
// So these tests are falsifiable in BOTH directions, and they are the reason the
// new detector is worth building at all. They compare detectors on the SAME
// event stream — the metamorphic discipline: change one property of the input,
// require the verdict to change in the specified direction.
package saturation

import "testing"

// feed drives one detector over a synthetic stream and returns its final level.
func feed(d Detector, events []Event) Level {
	d.Reset()
	for _, e := range events {
		d.Observe(e)
	}
	return d.Detect().Level
}

// buildStream generates a workload: n requests, `gapUs` apart, each carrying `in`
// prefill and `out` decode tokens, served in `svcUs`.
//
// APPARATUS WARNING -- svcUs is INDEPENDENT of `in` and `out`, which makes this
// builder PHYSICALLY INCOHERENT for any size ladder. In a real engine a bigger
// prompt costs more prefill and therefore more latency: measured on BLIS, an 8x
// prompt at fixed rate raises mean E2E 25.9x (6.2s -> 162s). A test that scales
// `in` while pinning `svcUs` asserts that prompts are free, and any detector that
// (correctly) watches latency will look "blind" to a stressor that, in this stream,
// genuinely costs nothing.
//
// That is exactly how an earlier version of TestMetamorphic_T2IN_IncumbentComparison
// concluded all three incumbent detectors were blind to T2-IN. On the real
// simulator with calibrated thresholds, composite and threshold both PASS T2-IN.
// The claim was an artifact of this builder, not a property of the detectors.
//
// Use it only where the stressor does not change per-request cost (T1's arrival
// rate). For size ladders, scale svcUs with the token counts, or measure on the
// simulator via campaign/bench/score_detector.py.
func buildStream(n int, gapUs int64, in, out int, svcUs int64) []Event {
	ev := make([]Event, 0, 2*n)
	for i := 0; i < n; i++ {
		ts := int64(i) * gapUs
		ev = append(ev, mkArrival(ts, itoa(i), in, out))
		ev = append(ev, mkCompletion(ts+svcUs, itoa(i), in, out, float64(svcUs)/1000.0))
	}
	return ev
}

// T2-IN: hold the arrival rate and output length FIXED; grow only the PROMPT.
// A detector with no prefill term in its statistic cannot see this.
//
// The prediction (§2e.6): OWD PASSES because of the kappa*I term; a VWD variant
// with kappa pinned to 0 FAILS. Both are exercised here so the test proves the
// TERM is what earns the pass, not the detector's overall sensitivity.
func TestMetamorphic_T2IN_PrefillTermIsWhatSeesIt(t *testing.T) {
	// Baseline: small prompts, comfortably sub-capacity.
	base := buildStream(400, 100_000, 128, 128, 20_000)
	// Stressed: 32x the prompt, everything else byte-identical.
	stressed := buildStream(400, 100_000, 4096, 128, 20_000)

	// The full detector: prefill term live.
	withPrefill := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.05, RDec0: 3000.0, Threshold: 2000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeKappa: true, FreezeRDec: true,
	})
	baseLevel := feed(withPrefill, base)
	stressLevel := feed(withPrefill, stressed)
	if baseLevel != Stable {
		t.Fatalf("apparatus: baseline must be STABLE for the test to mean anything, got %v", baseLevel)
	}
	if stressLevel == Stable {
		t.Errorf("T2-IN FAILED for OWD: a 32x prompt increase left it STABLE (predicted PASS, §2e.6)")
	}

	// The ablation: kappa = 0 removes the prefill term (Proposition 4 => VWD).
	noPrefill := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.0, RDec0: 3000.0, Threshold: 2000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeKappa: true, FreezeRDec: true,
	})
	vwdBase := feed(noPrefill, base)
	vwdStress := feed(noPrefill, stressed)
	if vwdBase != vwdStress {
		t.Errorf("ablation invalid: kappa=0 must be blind to prompt size, but verdict moved %v -> %v", vwdBase, vwdStress)
	}
	t.Logf("T2-IN: OWD %v->%v ; kappa=0 (VWD) %v->%v", baseLevel, stressLevel, vwdBase, vwdStress)
}

// T3: hold the OFFERED WORKLOAD byte-identical; reduce server CAPACITY.
//
// CAPACITY MUST BE MODELLED AS TOKEN THROUGHPUT, not as per-request service
// time. metamorphic_tests.md §1.2 (the factor-B trap) is the reason: an LLM
// engine is "one worker whose output is shared", so inflating each request's
// residency while completions still stream at the arrival rate raises
// CONCURRENCY without reducing capacity. An earlier version of this test did
// exactly that and the estimator correctly saw nothing -- the windows were
// byte-identical in both legs. A real collapse means fewer tokens COMPLETED per
// unit time, which is what the busy-window estimator reads.
//
// The prediction (§2e.6): an ADAPTIVE r_dec tracks the collapse and fires; a
// PINNED r_dec cannot see it at all. Both are exercised.
func TestMetamorphic_T3_AdaptiveRateIsWhatSeesIt(t *testing.T) {
	// Identical arrival stream in both legs; only the DRAIN rate differs.
	build := func(completionGapUs int64) []Event {
		var ev []Event
		for i := 0; i < 600; i++ {
			ev = append(ev, mkArrival(int64(i)*100_000, itoa(i), 512, 256))
		}
		for i := 0; i < 600; i++ {
			ev = append(ev, mkCompletion(int64(i)*completionGapUs+50_000, itoa(i), 512, 256, 50.0))
		}
		sortEventsByTimestamp(ev)
		return ev
	}
	healthy := build(100_000)   // drain keeps pace with arrivals
	collapsed := build(400_000) // 4x slower drain: capacity genuinely reduced

	adaptive := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.02, RDec0: 4000.0, Threshold: 5000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3,
	})
	hAdapt := feed(adaptive, healthy)
	rHealthy := adaptive.Detect().Signals["r_dec_hat"]
	cAdapt := feed(adaptive, collapsed)
	rCollapsed := adaptive.Detect().Signals["r_dec_hat"]

	// The MECHANISM claim: the estimated drain rate must FALL under collapse.
	// §2e.6's stated tell for a mis-gated estimator is "r_dec_hat flat while
	// completions fall" -- assert against it with a real margin, not just "<".
	if !(rCollapsed < 0.9*rHealthy) {
		t.Errorf("T3 mechanism broken: r_dec_hat must fall materially under collapse, got healthy=%.1f collapsed=%.1f", rHealthy, rCollapsed)
	}
	// And the verdict must actually flip.
	if hAdapt != Stable {
		t.Fatalf("apparatus: healthy leg must be STABLE, got %v", hAdapt)
	}
	if cAdapt == Stable {
		t.Errorf("T3 FAILED: capacity collapse left the detector STABLE (predicted PASS, §2e.6)")
	}
	t.Logf("T3: r_dec_hat %.1f -> %.1f ; level %v -> %v", rHealthy, rCollapsed, hAdapt, cAdapt)

	// The pinned-rate ablation must be structurally blind to the collapse.
	pinned := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.02, RDec0: 4000.0, Threshold: 5000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeRDec: true, FreezeKappa: true,
	})
	feed(pinned, healthy)
	pHealthy := pinned.Detect().Signals["r_dec_hat"]
	feed(pinned, collapsed)
	pCollapsed := pinned.Detect().Signals["r_dec_hat"]
	if pHealthy != pCollapsed {
		t.Errorf("ablation invalid: a pinned r_dec must not move, got %.1f -> %.1f", pHealthy, pCollapsed)
	}
}

// sortEventsByTimestamp puts a merged arrival/completion slice into the
// time order the real event stream has (insertion sort: stable, tiny inputs).
func sortEventsByTimestamp(ev []Event) {
	for i := 1; i < len(ev); i++ {
		for j := i; j > 0 && ev[j].Timestamp < ev[j-1].Timestamp; j-- {
			ev[j], ev[j-1] = ev[j-1], ev[j]
		}
	}
}

// T1: raising the arrival RATE with everything else fixed must be seen. Every
// detector is predicted to pass this; it is the sanity ladder.
func TestMetamorphic_T1_RateResponse(t *testing.T) {
	slow := buildStream(400, 200_000, 512, 256, 20_000)
	fast := buildStream(400, 10_000, 512, 256, 20_000)
	d := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.02, RDec0: 4000.0, Threshold: 3000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeKappa: true, FreezeRDec: true,
	})
	lo := feed(d, slow)
	hi := feed(d, fast)
	if lo != Stable {
		t.Fatalf("apparatus: slow rung must be STABLE, got %v", lo)
	}
	if hi == Stable {
		t.Errorf("T1 FAILED: a 20x rate increase left the detector STABLE")
	}
	t.Logf("T1: %v -> %v", lo, hi)
}

// T2-OUT: hold rate and prompt fixed; grow only the OUTPUT length.
func TestMetamorphic_T2OUT_OutputResponse(t *testing.T) {
	short := buildStream(400, 100_000, 512, 64, 20_000)
	long := buildStream(400, 100_000, 512, 2048, 20_000)
	d := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.02, RDec0: 4000.0, Threshold: 3000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeKappa: true, FreezeRDec: true,
	})
	lo := feed(d, short)
	hi := feed(d, long)
	if lo != Stable {
		t.Fatalf("apparatus: short-output rung must be STABLE, got %v", lo)
	}
	if hi == Stable {
		t.Errorf("T2-OUT FAILED: a 32x output increase left the detector STABLE")
	}
	t.Logf("T2-OUT: %v -> %v", lo, hi)
}

// The ABLATION this file can legitimately support: within the work-drift statistic,
// the kappa*I term is what makes the residual respond to a prompt-only change.
//
// This is NOT a claim about the incumbent detectors. An earlier version of this test
// compared them here and reported all three "BLIND" to T2-IN; that was an artifact of
// buildStream pinning service time while scaling prompt tokens (see its comment).
// Measured on the real simulator with calibrated thresholds, composite and threshold
// both PASS T2-IN -- an 8x prompt raises mean E2E 25.9x, which a latency detector
// sees without needing any prefill term. The honest cross-detector comparison lives
// in the campaign (campaign/bench/score_detector.py), on the simulator, at a matched
// false-alarm rate; a unit test over hand-built events cannot make that claim.
func TestMetamorphic_T2IN_KappaTermIsTheMechanism(t *testing.T) {
	base := buildStream(400, 100_000, 128, 128, 20_000)
	stressed := buildStream(400, 100_000, 4096, 128, 20_000)

	withPrefill := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.05, RDec0: 3000.0, Threshold: 2000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeKappa: true, FreezeRDec: true,
	})
	noPrefill := newWorkDriftForTest(workDriftConfig{
		Kind: "owd", Kappa0: 0.0, RDec0: 3000.0, Threshold: 2000.0,
		WindowSizeUs: 1_000_000, ConsecutiveK: 3, FreezeKappa: true, FreezeRDec: true,
	})

	wBase, wStress := feed(withPrefill, base), feed(withPrefill, stressed)
	nBase, nStress := feed(noPrefill, base), feed(noPrefill, stressed)

	if wBase == wStress {
		t.Errorf("kappa>0 must respond to a prompt-only change, but the verdict stayed %v", wBase)
	}
	if nBase != nStress {
		t.Errorf("kappa=0 must be blind to prompt size (Proposition 4), but moved %v -> %v", nBase, nStress)
	}
	t.Logf("kappa=0.05: %v -> %v ; kappa=0 (VWD): %v -> %v", wBase, wStress, nBase, nStress)
}
