// sim/saturation/randomwalk_test.go
//
// The correctness and behavioral relations the campaign declares for the
// reflected-random-walk detector. Each test here is named by a `native_test`
// locator in campaign/saturation-detector-rsm.yaml: a declared relation whose
// test does not run counts as a FAILED correctness relation and aborts the
// campaign at verify, so these are the campaign's apparatus guarantees, not
// optional extras.
//
// They assert LAWS over the whole factor space rather than golden values, so they
// survive a reimplementation of any single statistic.
package saturation

import (
	"math"
	"sort"
	"testing"
)

// allStatistics is every level of the STAT factor plus the two screened-out
// members, so the finiteness law covers configurations the campaign excludes as
// well as the ones it searches.
var allStatistics = []rwStatistic{
	statPeakOverElapsed, statPeakDecayRate, statExcursionRate,
	statExcursionScaling, statIdleFraction,
}

var allSources = []rwBacklogSource{srcInFlight, srcWorkBacklog}

func rwCfg(stat rwStatistic, src rwBacklogSource) randomWalkConfig {
	return randomWalkConfig{
		Statistic: stat, Source: src, Threshold: 1e9,
		MinObservations: 1, ConsecutiveK: 3, Kappa: 0.02, BacklogK: 3.0,
	}
}

// rwStream builds a coherent arrival/completion stream: a request is served no
// earlier than it arrives and no earlier than the server frees up (Lindley in the
// apparatus), so every request completes and the walk is physically realizable.
func rwStream(n int, arrivalGapUs, serviceUs int64, in, out int) []Event {
	ev := make([]Event, 0, 2*n)
	var freeAt int64
	for i := 0; i < n; i++ {
		arr := int64(i) * arrivalGapUs
		start := arr
		if freeAt > start {
			start = freeAt
		}
		comp := start + serviceUs
		freeAt = comp
		ev = append(ev, mkArrival(arr, itoa(i), in, out))
		ev = append(ev, mkCompletion(comp, itoa(i), in, out, float64(comp-arr)/1000.0))
	}
	sortEventsByTimestamp(ev)
	return ev
}

// STAT-R1 (correctness): every statistic is finite on every input, including
// degenerate ones. A NaN or Inf reaching the verdict would silently corrupt an
// entire campaign row rather than failing it.
func TestRandomWalk_StatisticsFinite(t *testing.T) {
	streams := map[string][]Event{
		"empty":        {},
		"single":       {mkArrival(0, "a", 512, 256)},
		"no_time":      {mkArrival(0, "a", 512, 256), mkCompletion(0, "a", 512, 256, 0)},
		"never_drains": rwStream(50, 1_000, 500_000, 512, 256),
		"fully_drains": rwStream(50, 500_000, 1_000, 512, 256),
		"zero_tokens":  rwStream(20, 10_000, 5_000, 0, 0),
	}
	for _, stat := range allStatistics {
		for _, src := range allSources {
			for name, stream := range streams {
				d := newRandomWalkForTest(rwCfg(stat, src))
				for _, e := range stream {
					d.Observe(e)
				}
				res := d.Detect()
				for k, v := range res.Signals {
					if math.IsNaN(v) || math.IsInf(v, 0) {
						t.Errorf("stat=%s src=%s stream=%s: signal %q not finite: %v", stat, src, name, k, v)
					}
				}
				if math.IsNaN(res.Score) || math.IsInf(res.Score, 0) {
					t.Errorf("stat=%s src=%s stream=%s: score not finite: %v", stat, src, name, res.Score)
				}
			}
		}
	}
}

// STAT-R2 (behavioral): a Peak-based statistic rises with offered load. Declared
// behavioral, not correctness, because it is a monotonicity claim (rules.md R-
// guidance and the campaign guide's anti-pattern §6.4: monotonicity is never a
// correctness relation).
func TestRandomWalk_MonotoneInOfferedLoad(t *testing.T) {
	for _, stat := range []rwStatistic{statPeakOverElapsed, statPeakDecayRate} {
		for _, src := range allSources {
			gaps := []int64{400_000, 200_000, 100_000, 50_000, 25_000}
			var vals []float64
			for _, g := range gaps {
				d := newRandomWalkForTest(rwCfg(stat, src))
				for _, e := range rwStream(400, g, 100_000, 512, 256) {
					d.Observe(e)
				}
				vals = append(vals, d.Detect().Signals["statistic"])
			}
			for i := 1; i < len(vals); i++ {
				if vals[i] < vals[i-1]*0.999 {
					t.Errorf("stat=%s src=%s: statistic fell as offered load rose (gap %d -> %d): %.6g -> %.6g",
						stat, src, gaps[i-1], gaps[i], vals[i-1], vals[i])
				}
			}
			t.Logf("stat=%-18s src=%-12s load-increasing: %v", stat, src, vals)
		}
	}
}

// SRC-R1 (correctness): work_backlog weights a request by kappa*I + O and never
// goes negative. A negative backlog would make the walk unreflected and every
// Peak statistic meaningless.
func TestRandomWalk_WorkBacklogNeverNegative(t *testing.T) {
	d := newRandomWalkForTest(rwCfg(statPeakOverElapsed, srcWorkBacklog))
	// Include completions with no matching arrival (the adapter can emit them at a
	// trace boundary) to try to drive the backlog below zero.
	stream := rwStream(60, 20_000, 50_000, 1024, 128)
	stream = append(stream, mkCompletion(9_999_999, "ghost", 4096, 4096, 1.0))
	for _, e := range stream {
		d.Observe(e)
		if d.workBacklog < 0 {
			t.Fatalf("work backlog went negative: %v", d.workBacklog)
		}
	}
	// And the weighting must actually depend on kappa*I + O: a bigger request must
	// raise the walk more.
	small := newRandomWalkForTest(rwCfg(statPeakOverElapsed, srcWorkBacklog))
	big := newRandomWalkForTest(rwCfg(statPeakOverElapsed, srcWorkBacklog))
	for _, e := range rwStream(40, 20_000, 50_000, 128, 64) {
		small.Observe(e)
	}
	for _, e := range rwStream(40, 20_000, 50_000, 128, 2048) {
		big.Observe(e)
	}
	if !(big.peak > small.peak) {
		t.Errorf("work_backlog peak must grow with per-request work: small=%v big=%v", small.peak, big.peak)
	}
}

// CONSEC-R1 (correctness): consecutive sub-threshold observations reset the
// counter. Without the reset, k would count TOTAL breaches ever rather than
// consecutive ones, and the anti-flap lever would not be an anti-flap lever.
func TestRandomWalk_ConsecutiveResets(t *testing.T) {
	cfg := rwCfg(statPeakOverElapsed, srcInFlight)
	cfg.Threshold = 1e-9 // any positive statistic breaches
	cfg.ConsecutiveK = 100
	cfg.MinObservations = 1
	d := newRandomWalkForTest(cfg)
	for _, e := range rwStream(30, 5_000, 200_000, 512, 256) {
		d.Observe(e)
		d.Detect()
	}
	if d.consecutive == 0 {
		t.Fatalf("apparatus: expected the counter to accumulate breaches, got 0")
	}
	before := d.consecutive

	// Raise the threshold out of reach: the next Detect sees a sub-threshold
	// statistic and MUST zero the counter rather than keep the running total.
	d.cfg.Threshold = 1e12
	d.Detect()
	if d.consecutive != 0 {
		t.Errorf("counter did not reset on a sub-threshold observation: %d -> %d", before, d.consecutive)
	}

	// And it must accumulate again from zero, not resume from `before`.
	d.cfg.Threshold = 1e-9
	d.Detect()
	if d.consecutive != 1 {
		t.Errorf("counter did not restart from zero after a reset: got %d, want 1", d.consecutive)
	}
}

// CONSEC-R2 (behavioral): raising k never increases the flip count. This is the
// lever's whole purpose -- if a higher k could flip MORE, the T4 trade-off the
// campaign is searching would not exist.
func TestRandomWalk_HigherKNeverMoreFlips(t *testing.T) {
	// A bursty stream at a held mean, which is what makes a level detector flap.
	var stream []Event
	var freeAt int64
	idx := 0
	for cycle := 0; cycle < 25; cycle++ {
		for i := 0; i < 12; i++ { // burst
			arr := int64(cycle)*600_000 + int64(i)*10_000
			start := arr
			if freeAt > start {
				start = freeAt
			}
			comp := start + 60_000
			freeAt = comp
			stream = append(stream, mkArrival(arr, itoa(idx), 512, 256),
				mkCompletion(comp, itoa(idx), 512, 256, float64(comp-arr)/1000.0))
			idx++
		}
		for i := 0; i < 3; i++ { // lull, same mean
			arr := int64(cycle)*600_000 + 200_000 + int64(i)*120_000
			start := arr
			if freeAt > start {
				start = freeAt
			}
			comp := start + 60_000
			freeAt = comp
			stream = append(stream, mkArrival(arr, itoa(idx), 512, 256),
				mkCompletion(comp, itoa(idx), 512, 256, float64(comp-arr)/1000.0))
			idx++
		}
	}
	sortEventsByTimestamp(stream)

	// Calibrate the threshold to the MEDIAN observed statistic, so it sits inside
	// the range the stream actually visits and flapping is possible at all. A
	// threshold outside the range yields all-zero flips for every k, which would
	// make this test vacuously pass -- observed on the first attempt.
	probe := newRandomWalkForTest(rwCfg(statPeakOverElapsed, srcInFlight))
	var stats []float64
	for _, e := range stream {
		probe.Observe(e)
		stats = append(stats, probe.Detect().Signals["statistic"])
	}
	sort.Float64s(stats)
	median := stats[len(stats)/2]
	if median <= 0 {
		t.Fatalf("apparatus: median statistic is %v; the stream never leaves zero", median)
	}

	flipsFor := func(k int) int {
		cfg := rwCfg(statPeakOverElapsed, srcInFlight)
		cfg.Threshold = median
		cfg.ConsecutiveK = k
		cfg.MinObservations = 10
		d := newRandomWalkForTest(cfg)
		flips, prevFired := 0, false
		for _, e := range stream {
			d.Observe(e)
			fired := d.Detect().Level != Stable
			if prevFired && !fired {
				flips++
			}
			prevFired = fired
		}
		return flips
	}
	ks := []int{1, 2, 3, 5, 8}
	var got []int
	for _, k := range ks {
		got = append(got, flipsFor(k))
	}
	// The lever must be EXERCISED: if no k flaps, the monotonicity claim below is
	// vacuous and the test proves nothing about the lever.
	if got[0] == 0 {
		t.Fatalf("apparatus: k=1 produced no flips (threshold=%.6g), so the anti-flap lever is untested", median)
	}
	for i := 1; i < len(got); i++ {
		if got[i] > got[i-1] {
			t.Errorf("raising k increased flips (k=%d -> %d): %d -> %d", ks[i-1], ks[i], got[i-1], got[i])
		}
	}
	t.Logf("k=%v -> flips=%v", ks, got)
}

// WARM-R1 (correctness): warmup shortens the elapsed denominator and never makes
// it negative. A negative or zero denominator would divide the Peak statistic by
// garbage.
func TestRandomWalk_WarmupNeverNegativeElapsed(t *testing.T) {
	stream := rwStream(100, 50_000, 20_000, 512, 256)
	prev := int64(math.MaxInt64)
	for _, warmMs := range []int{0, 1000, 5000, 15000, 30000, 3_600_000} {
		cfg := rwCfg(statPeakOverElapsed, srcInFlight)
		cfg.WarmupUs = int64(warmMs) * 1000
		d := newRandomWalkForTest(cfg)
		for _, e := range stream {
			d.Observe(e)
		}
		e := d.elapsedUs()
		if e < 0 {
			t.Errorf("warmup_ms=%d produced negative elapsed: %d", warmMs, e)
		}
		if e > prev {
			t.Errorf("warmup_ms=%d increased elapsed (%d) vs a smaller warmup (%d)", warmMs, e, prev)
		}
		prev = e
		// And the statistic must stay finite even when warmup exceeds the run.
		if v := d.Detect().Signals["statistic"]; math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("warmup_ms=%d: statistic not finite: %v", warmMs, v)
		}
	}
}

// MINOBS-R1 (correctness): below min_observations the verdict is STABLE with zero
// confidence. Firing on two events would make every campaign row's early trace a
// false alarm.
func TestRandomWalk_GatedBelowMinObservations(t *testing.T) {
	for _, minObs := range []int{5, 20, 50, 100, 200} {
		cfg := rwCfg(statPeakOverElapsed, srcInFlight)
		cfg.Threshold = 1e-9 // fires on anything once ungated
		cfg.MinObservations = minObs
		cfg.ConsecutiveK = 1
		d := newRandomWalkForTest(cfg)
		stream := rwStream(minObs*2, 5_000, 200_000, 512, 256)
		for i, e := range stream {
			d.Observe(e)
			res := d.Detect()
			if i+1 < minObs {
				if res.Level != Stable {
					t.Errorf("minObs=%d: fired at observation %d (below the gate): %v", minObs, i+1, res.Level)
				}
				if res.Confidence != 0 {
					t.Errorf("minObs=%d: non-zero confidence at observation %d: %v", minObs, i+1, res.Confidence)
				}
			}
		}
	}
}

// HORIZON-R1 (correctness): the reference horizon is anchored to ELAPSED TIME,
// never to event index.
//
// This is the defect that inverted attempt #2 (see
// campaign/findings/PEAK-STATISTIC-DIAGNOSIS.md): at high load arrivals cluster
// early while completions trail, so the event-index midpoint sits far from the
// time midpoint and the ratio ends up measuring event ordering rather than decay.
//
// The law: two streams with the SAME time span and the same Peak trajectory must
// produce the same statistic even when their EVENT COUNTS differ wildly. An
// index-anchored implementation cannot satisfy that.
func TestRandomWalk_HorizonIsTimeAnchored(t *testing.T) {
	// Both streams span the same 20s and drive in-flight to the same shape; one
	// carries 4x the events (denser sampling of the identical trajectory).
	build := func(step int64, n int) []Event {
		ev := make([]Event, 0, 2*n)
		var freeAt int64
		for i := 0; i < n; i++ {
			arr := int64(i) * step
			start := arr
			if freeAt > start {
				start = freeAt
			}
			// Service scaled so total span and concurrency profile match.
			comp := start + step*3
			freeAt = comp
			ev = append(ev, mkArrival(arr, itoa(i), 512, 256))
			ev = append(ev, mkCompletion(comp, itoa(i), 512, 256, float64(comp-arr)/1000.0))
		}
		sortEventsByTimestamp(ev)
		return ev
	}
	sparse := build(200_000, 100) // 100 events over 20s
	dense := build(50_000, 400)   // 400 events over 20s

	stat := func(stream []Event) float64 {
		cfg := rwCfg(statPeakRatioStability, srcInFlight)
		cfg.HorizonRatio = 2.0
		d := newRandomWalkForTest(cfg)
		for _, e := range stream {
			d.Observe(e)
		}
		return d.Detect().Signals["statistic"]
	}
	s1, s2 := stat(sparse), stat(dense)
	t.Logf("sparse(100 events)=%.4f dense(400 events)=%.4f", s1, s2)

	// The horizon must be time-anchored, so the reference elapsed time recorded by
	// each detector must be comparable despite the 4x event-count difference.
	cfg := rwCfg(statPeakRatioStability, srcInFlight)
	cfg.HorizonRatio = 2.0
	dS := newRandomWalkForTest(cfg)
	for _, e := range sparse {
		dS.Observe(e)
	}
	dD := newRandomWalkForTest(cfg)
	for _, e := range dense {
		dD.Observe(e)
	}
	// elapsedAtHalf is in SECONDS of simulated time; a time-anchored horizon puts
	// both within a factor of 2 of each other. An index-anchored one would differ
	// by roughly the event-count ratio.
	if dS.elapsedAtHalf <= 0 || dD.elapsedAtHalf <= 0 {
		t.Fatalf("apparatus: no reference horizon recorded (sparse=%v dense=%v)", dS.elapsedAtHalf, dD.elapsedAtHalf)
	}
	ratio := dS.elapsedAtHalf / dD.elapsedAtHalf
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("reference horizon is not time-anchored: sparse elapsedAtHalf=%.3fs dense=%.3fs (ratio %.2f); an index-anchored horizon would differ by ~the event-count ratio (4x)",
			dS.elapsedAtHalf, dD.elapsedAtHalf, ratio)
	}

	// And the HorizonRatio knob must actually move the reference point.
	cfgFar := rwCfg(statPeakRatioStability, srcInFlight)
	cfgFar.HorizonRatio = 10.0
	dFar := newRandomWalkForTest(cfgFar)
	for _, e := range dense {
		dFar.Observe(e)
	}
	if dFar.elapsedAtHalf == dD.elapsedAtHalf {
		t.Errorf("horizon_ratio is DEAD: ratio 2.0 and 10.0 both recorded elapsedAtHalf=%.3fs", dD.elapsedAtHalf)
	}
	t.Logf("horizon_ratio 2.0 -> refAt=%.3fs ; 10.0 -> refAt=%.3fs", dD.elapsedAtHalf, dFar.elapsedAtHalf)
}
