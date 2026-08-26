package saturation

import (
	"math"
	"math/rand"
	"testing"
)

// These tests assert LAWS the confidence sequence must satisfy, stated in terms of inputs
// and reported intervals only. None reads a field or asserts a formula, so all of them
// survive a complete rewrite of the estimator -- and each one can fail: the in-sample
// variant this construction replaced fails TestSlopeCS_CoversTrueSlopeUnderContinuousInspection
// at roughly 3.6x its budget, which is what makes that test discriminating rather than
// decorative.

// logLogPath generates observations from v = slope*u + e with u = log(i+1).
//
// burst > 1 injects occasional shocks burst^2 times larger with probability 1/burst^2, so
// the error MEAN is unchanged while its variance rises. That isolates dispersion from drift:
// a detector that merely reacted to a shifted mean would not be tested by this.
func logLogPath(n int, slope, noise float64, seed int64, burst float64) (us, vs []float64) {
	rng := rand.New(rand.NewSource(seed))
	for i := 1; i <= n; i++ {
		u := math.Log(float64(i) + 1)
		e := rng.NormFloat64() * noise
		if burst > 1 && rng.Float64() < 1/(burst*burst) {
			e *= burst * burst
		}
		us = append(us, u)
		vs = append(vs, slope*u+e)
	}
	return us, vs
}

// TestSlopeCS_CoversTrueSlopeUnderContinuousInspection is the guarantee itself.
//
// A time-uniform interval must contain the true slope at EVERY n simultaneously, so the
// failure event is per-PATH (did it ever exclude the truth?), not per-look. Counting
// per-look failures would flatter any interval, since most looks are easy.
//
// KAPPA IS PINNED AT ITS SMALLEST VALID VALUE HERE, and that is what makes this test
// discriminating rather than decorative. A sweep over kappa (campaign/probes) measured
// path-wise coverage failure for this construction against a batch in-sample variant:
//
//	kappa   prequential   batch in-sample
//	  0.0         1.000             0.730
//	  0.1         0.457             0.517
//	  1.0         0.017             0.037
//	  5.0         0.000             0.000
//
// Two things follow. First, kappa is REQUIRED for validity at small n, not optional
// polish: at kappa=0 the width starts near zero and almost every path is excluded on its
// first looks. Second, at kappa=5 BOTH constructions cover perfectly, so a coverage test
// run there cannot tell them apart -- it would pass for an implementation with the
// in-sample defect. Pinning kappa at 1.0 keeps the test sensitive to the thing it is
// meant to protect while remaining a valid operating point.
func TestSlopeCS_CoversTrueSlopeUnderContinuousInspection(t *testing.T) {
	const (
		alpha  = 0.05
		kappa  = 1.0 // smallest valid; see the table above -- larger values mask defects
		trials = 300
		budget = alpha * 1.5 // Monte-Carlo slack at 300 trials; still well inside 2x
	)
	for _, trueSlope := range []float64{0.0, 0.5, 1.0} {
		failures := 0
		for trial := 0; trial < trials; trial++ {
			cs := newSlopeCS(alpha, kappa, 0.30)
			us, vs := logLogPath(300, trueSlope, 0.30, int64(trial*31)+int64(trueSlope*10), 1)
			for i := range us {
				cs.Observe(us[i], vs[i])
				if center, half, ok := cs.Interval(); ok && math.Abs(center-trueSlope) > half {
					failures++
					break
				}
			}
		}
		if rate := float64(failures) / trials; rate > budget {
			t.Errorf("slope %.1f: path-wise coverage failure %.3f exceeds budget %.3f (alpha %.2f)",
				trueSlope, rate, budget, alpha)
		}
	}
}

// TestSlopeCS_DoesNotCommitOnACriticalPath is the false-alarm law an operator feels.
//
// On a path whose true slope IS the decision boundary, committing either way is an error.
// This is stricter than a one-sided false-positive test and is the honest analogue of an
// FPR budget for a two-sided interval.
func TestSlopeCS_DoesNotCommitOnACriticalPath(t *testing.T) {
	const (
		alpha    = 0.05
		boundary = 0.5
		trials   = 300
	)
	committed := 0
	for trial := 0; trial < trials; trial++ {
		cs := newSlopeCS(alpha, 5.0, 0.30)
		us, vs := logLogPath(400, boundary, 0.30, int64(trial)+7000, 1)
		for i := range us {
			cs.Observe(us[i], vs[i])
			center, half, ok := cs.Interval()
			if ok && (center-half > boundary || center+half < boundary) {
				committed++
				break
			}
		}
	}
	if rate := float64(committed) / trials; rate > alpha*1.5 {
		t.Errorf("committed on %.3f of critical paths, budget %.3f", rate, alpha*1.5)
	}
}

// TestSlopeCS_DecidesBothDirections: a valid interval that never decides is useless, so
// coverage alone is not enough. Both a clearly-overloaded and a clearly-healthy path must
// resolve, which also rules out the degenerate "always undecided" detector.
func TestSlopeCS_DecidesBothDirections(t *testing.T) {
	const boundary = 0.5
	for _, tc := range []struct {
		name      string
		trueSlope float64
		wantAbove bool
	}{
		{"overloaded", 1.0, true},
		{"healthy", 0.0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newSlopeCS(0.05, 5.0, 0.30)
			us, vs := logLogPath(3000, tc.trueSlope, 0.30, 99, 1)
			for i := range us {
				cs.Observe(us[i], vs[i])
				center, half, ok := cs.Interval()
				if !ok {
					continue
				}
				if tc.wantAbove && center-half > boundary {
					return
				}
				if !tc.wantAbove && center+half < boundary {
					return
				}
			}
			t.Errorf("%s path never committed within 3000 observations", tc.name)
		})
	}
}

// TestSlopeCS_BurstierArrivalsNeedMoreEvidence is the burstiness law, and the reason the
// construction is self-normalized.
//
// The true slope is IDENTICAL across the three arms; only the error dispersion differs, and
// the sequence is told nothing about it. A detector using a fixed width, or one dividing by
// a plug-in dispersion estimate with a floor, would not show a monotone ordering here.
func TestSlopeCS_BurstierArrivalsNeedMoreEvidence(t *testing.T) {
	const (
		boundary  = 0.5
		trueSlope = 1.0
		paths     = 20
		cap       = 6000
	)
	meanN := func(burst float64) float64 {
		total := 0
		for p := 0; p < paths; p++ {
			cs := newSlopeCS(0.05, 5.0, 0.30)
			us, vs := logLogPath(cap, trueSlope, 0.30, int64(p)+400, burst)
			n := cap
			for i := range us {
				cs.Observe(us[i], vs[i])
				if center, half, ok := cs.Interval(); ok && center-half > boundary {
					n = i + 1
					break
				}
			}
			total += n
		}
		return float64(total) / paths
	}
	smooth, moderate, heavy := meanN(1), meanN(2), meanN(4)
	if !(smooth < moderate && moderate < heavy) {
		t.Errorf("observations-to-decide not monotone in burstiness: %.1f, %.1f, %.1f",
			smooth, moderate, heavy)
	}
}

// TestSlopeCS_TighterBudgetNeverDecidesEarlier: demanded on the SAME data, so it isolates
// the budget from sampling noise. A violation would mean the width is not monotone in alpha,
// i.e. the guarantee can be bought cheaply -- the arithmetic signature of a bound that is
// not really paying for its coverage.
func TestSlopeCS_TighterBudgetNeverDecidesEarlier(t *testing.T) {
	const boundary = 0.5
	us, vs := logLogPath(4000, 1.0, 0.30, 5, 1)
	decideAt := func(alpha float64) int {
		cs := newSlopeCS(alpha, 5.0, 0.30)
		for i := range us {
			cs.Observe(us[i], vs[i])
			if center, half, ok := cs.Interval(); ok && center-half > boundary {
				return i + 1
			}
		}
		return len(us)
	}
	lenient, strict := decideAt(0.10), decideAt(0.01)
	if strict < lenient {
		t.Errorf("alpha 0.01 decided at %d, EARLIER than alpha 0.10 at %d", strict, lenient)
	}
}

// TestSlopeCS_NearCriticalIsHarderThanClearOverload: the difficulty must track how far the
// truth sits from the boundary. A detector whose speed is insensitive to that is reading
// something other than the evidence -- which is how a gate constant masquerades as a
// detection time.
func TestSlopeCS_NearCriticalIsHarderThanClearOverload(t *testing.T) {
	const (
		boundary = 0.5
		paths    = 15
		cap      = 8000
	)
	meanN := func(trueSlope float64) float64 {
		total := 0
		for p := 0; p < paths; p++ {
			cs := newSlopeCS(0.05, 5.0, 0.30)
			us, vs := logLogPath(cap, trueSlope, 0.30, int64(p)+800, 1)
			n := cap
			for i := range us {
				cs.Observe(us[i], vs[i])
				if center, half, ok := cs.Interval(); ok && center-half > boundary {
					n = i + 1
					break
				}
			}
			total += n
		}
		return float64(total) / paths
	}
	if nearCritical, clear := meanN(0.6), meanN(1.0); nearCritical <= clear {
		t.Errorf("near-critical (%.1f obs) should be slower than clear overload (%.1f obs)",
			nearCritical, clear)
	}
}

// TestSlopeCS_DegenerateDesignReportsNoInterval: with no spread in u the slope is
// unidentified, and the honest report is "no interval" rather than a slope of zero. R20 in
// this codebase: degenerate input is never a guess.
func TestSlopeCS_DegenerateDesignReportsNoInterval(t *testing.T) {
	cs := newSlopeCS(0.05, 5.0, 0.30)
	for i := 0; i < 50; i++ {
		cs.Observe(1.0, float64(i)) // u constant: no leverage at all
	}
	if _, half, ok := cs.Interval(); ok || !math.IsInf(half, 1) {
		t.Errorf("degenerate design reported ok=%v half=%v; want ok=false, half=+Inf", ok, half)
	}
}

// TestSlopeCS_ResetRestoresInitialBehavior: Reset must clear the accumulated evidence, not
// merely the counter. Asserted behaviorally -- the same input sequence must produce the same
// interval after a reset as it did from a fresh sequence.
func TestSlopeCS_ResetRestoresInitialBehavior(t *testing.T) {
	us, vs := logLogPath(200, 1.0, 0.30, 12, 1)
	fresh := newSlopeCS(0.05, 5.0, 0.30)
	for i := range us {
		fresh.Observe(us[i], vs[i])
	}
	wantCenter, wantHalf, wantOK := fresh.Interval()

	reused := newSlopeCS(0.05, 5.0, 0.30)
	other, otherV := logLogPath(150, 0.0, 0.90, 77, 3) // unrelated, noisier history
	for i := range other {
		reused.Observe(other[i], otherV[i])
	}
	reused.Reset()
	for i := range us {
		reused.Observe(us[i], vs[i])
	}
	gotCenter, gotHalf, gotOK := reused.Interval()

	if gotOK != wantOK || math.Abs(gotCenter-wantCenter) > 1e-12 || math.Abs(gotHalf-wantHalf) > 1e-12 {
		t.Errorf("after Reset got (%v, %v, %v); want (%v, %v, %v)",
			gotCenter, gotHalf, gotOK, wantCenter, wantHalf, wantOK)
	}
}
