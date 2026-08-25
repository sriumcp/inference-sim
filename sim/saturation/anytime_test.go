package saturation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Shared fixtures.
//
// Every property/metamorphic test below sweeps a generated input space rather
// than asserting on hand-picked points: the claims are "for all streams" and
// "for all n", and a single fixture cannot distinguish a monotone bound from one
// that happens to be monotone at the two points chosen.
// ---------------------------------------------------------------------------

// genQueueStream synthesizes a deterministic Arrival/Completion stream from a
// single-server FIFO queue with Poisson-ish batch arrivals, so the regimes the
// detectors are supposed to separate actually appear in the stream:
//
//	lambda > mu  =>  backlog grows without bound (peak climbs ~linearly)
//	lambda < mu  =>  backlog is a finite random variable (peak levels off)
//
// burst > 1 releases arrivals in simultaneous batches at the same mean rate,
// which raises the arrival process's index of dispersion without changing its
// mean -- the exact confound the width correction has to absorb.
//
// Timestamps are microseconds, matching the Event contract.
func genQueueStream(seed int64, nReq int, lambdaPerSec, muPerSec float64, burst int) []Event {
	rng := rand.New(rand.NewSource(seed))
	if burst < 1 {
		burst = 1
	}
	arrivals := make([]float64, 0, nReq)
	tNow := 0.0
	for len(arrivals) < nReq {
		for j := 0; j < burst && len(arrivals) < nReq; j++ {
			arrivals = append(arrivals, tNow)
		}
		tNow += rng.ExpFloat64() / lambdaPerSec * 1e6 * float64(burst)
	}

	events := make([]Event, 0, 2*nReq)
	serverFree := 0.0
	for i, at := range arrivals {
		start := math.Max(at, serverFree)
		serverFree = start + rng.ExpFloat64()/muPerSec*1e6
		id := fmt.Sprintf("r%d", i)
		events = append(events,
			Event{Timestamp: int64(at), Type: Arrival, RequestID: id, InputTokens: 128, OutputTokens: 64},
			Event{Timestamp: int64(serverFree), Type: Completion, RequestID: id, LatencyMs: (serverFree - at) / 1000, InputTokens: 128, OutputTokens: 64},
		)
	}
	// Nondecreasing time, arrivals before completions at a shared timestamp --
	// the order buildSortedEvents delivers.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Timestamp != events[j].Timestamp {
			return events[i].Timestamp < events[j].Timestamp
		}
		return events[i].Type < events[j].Type
	})
	return events
}

// atCfg is the in-process equivalent of a fully specified anytime: block.
func atCfg(wrapped, bound string, alpha float64, dispWin int64, indet string) anytimeConfig {
	return anytimeConfig{
		Wrapped:             wrapped,
		Bound:               bound,
		Alpha:               alpha,
		DispersionWindowUs:  dispWin,
		IndeterminatePolicy: indet,
	}
}

// drive runs one detector over a stream exactly as ReplayOneDetector does (Observe
// then Detect, once per event) and returns the per-event trace.
func drive(t *testing.T, d Detector, events []Event) []TraceRecord {
	t.Helper()
	out := make([]TraceRecord, 0, len(events))
	for _, e := range events {
		d.Observe(e)
		out = append(out, TraceRecord{Timestamp: e.Timestamp, Detector: d.Name(), Result: d.Detect()})
	}
	return out
}

// driveAnytime builds the wrapper from a config and drives it.
func driveAnytime(t *testing.T, cfg anytimeConfig, events []Event) []TraceRecord {
	t.Helper()
	d, err := newAnytimeDetector(cfg)
	if err != nil {
		t.Fatalf("newAnytimeDetector(%+v): %v", cfg, err)
	}
	return drive(t, d, events)
}

// firstFiredIndex is the record index of the first firing verdict, or len(records)
// when the detector never fires -- so "never" compares as later than any fire.
func firstFiredIndex(records []TraceRecord) int {
	for i, r := range records {
		if AnytimeRecordFired(r.Result) {
			return i
		}
	}
	return len(records)
}

// sweepConfigs is the cross-product of the campaign's declared WRAPPED x BOUND x
// INDET levels at the locked alpha, used wherever a property must hold for every
// configuration rather than for one.
func sweepConfigs() []anytimeConfig {
	var out []anytimeConfig
	for _, w := range []string{"composite", "peak-rate"} {
		for _, b := range []string{boundHowardEB, boundMixtureSPRT} {
			for _, ind := range []string{indetStrict, indetLeanStable} {
				out = append(out, atCfg(w, b, 0.05, defaultAnytimeDispersionWindowUs, ind))
			}
		}
	}
	return out
}

// superStream / atHealthyStream are the two regimes, parameterized only by seed.
// The rates are chosen so peak-rate's own default threshold (0.5 backlog/sec) is
// cleared by a wide margin in one and not approached in the other; nothing about
// the mechanism is tuned to them.
func superStream(seed int64, burst int) []Event {
	return genQueueStream(seed, 800, 50, 25, burst)
}

func atHealthyStream(seed int64, burst int) []Event {
	return genQueueStream(seed, 800, 10, 40, burst)
}

// spyDetector is a Detector that records what it was handed and returns a fixed
// Result, so "the wrapper forwards and does not edit" is checkable directly rather
// than inferred.
type spyDetector struct {
	events      []Event
	result      Result
	detectCalls int
	resetCalls  int
}

func (s *spyDetector) Name() string      { return "spy" }
func (s *spyDetector) Observe(e Event)   { s.events = append(s.events, e) }
func (s *spyDetector) Detect() Result    { s.detectCalls++; return s.result }
func (s *spyDetector) Reset()            { s.resetCalls++; s.events = nil }
func (s *spyDetector) recorded() []Event { return s.events }

// wrapSpy builds a wrapper around an injected detector, bypassing the registry so
// the forwarding contract can be observed. Every OTHER test constructs through
// newAnytimeDetector (i.e. through buildDetector) so the registry path is the one
// under test.
func wrapSpy(cfg anytimeConfig, spy Detector) *AnytimeDetector {
	a := &AnytimeDetector{cfg: cfg, wrapped: spy}
	a.Reset()
	return a
}

// parseSaturationYAML parses a config from bytes with the SAME strict decoder
// LoadSaturationConfig uses, so unknown-key rejection is exercised without
// creating files outside the repository.
func parseSaturationYAML(t *testing.T, src string) (SaturationConfig, error) {
	t.Helper()
	var cfg SaturationConfig
	dec := yaml.NewDecoder(bytes.NewReader([]byte(src)))
	dec.KnownFields(true)
	err := dec.Decode(&cfg)
	return cfg, err
}

// ---------------------------------------------------------------------------
// 1. The wrapped detector's statistic must be exactly the bare detector's.
// ---------------------------------------------------------------------------

func TestAnytime_ForwardsToWrappedUnchanged(t *testing.T) {
	// Part 1: every event reaches the wrapped detector verbatim -- including an
	// event type NEITHER detector counts, which the wrapper must still forward
	// (whether an event is meaningful is the wrapped detector's judgement).
	events := append(superStream(11, 1),
		Event{Timestamp: 9_000_000, Type: EventType(7), RequestID: "weird"},
		Event{Timestamp: 9_100_000, Type: Arrival, RequestID: "after"},
	)

	sentinel := map[string]float64{"spy_signal": 1.25, "other": -3}
	spyMapCopy := map[string]float64{}
	for k, v := range sentinel {
		spyMapCopy[k] = v
	}
	spy := &spyDetector{result: Result{Level: Backlogged, Score: 0.4, Confidence: 0.5, Signals: sentinel}}

	a := wrapSpy(atCfg("composite", boundHowardEB, 0.05, defaultAnytimeDispersionWindowUs, indetStrict), spy)
	for _, e := range events {
		a.Observe(e)
		// Detect called repeatedly on purpose: it must not consume or mutate anything.
		_ = a.Detect()
		_ = a.Detect()
	}

	if got := spy.recorded(); !reflect.DeepEqual(got, events) {
		t.Fatalf("wrapped detector saw a different event stream: got %d events, want %d (first divergence matters)", len(got), len(events))
	}
	if !reflect.DeepEqual(sentinel, spyMapCopy) {
		t.Errorf("wrapper mutated the wrapped Result's Signals map: got %v, want %v", sentinel, spyMapCopy)
	}
	// Exactly one wrapped Detect per wrapped Observe: the caching claim. Without it
	// composite's O(C log C) Detect would be paid once per wrapper Detect call, so a
	// caller that inspects the verdict twice would double the wrapped cost.
	if spy.detectCalls != len(events) {
		t.Errorf("wrapped Detect called %d times over %d events; want exactly one per Observe", spy.detectCalls, len(events))
	}

	// Part 2: the wrapped detector's VERDICT is unchanged by being wrapped. Driven
	// bare and driven through the wrapper, its per-event Result must agree exactly.
	for _, wrapped := range []string{"composite", "peak-rate"} {
		for _, seed := range []int64{1, 2, 3} {
			for _, stream := range [][]Event{superStream(seed, 1), atHealthyStream(seed, 1)} {
				bare, err := buildDetector(wrapped, SaturationConfig{})
				if err != nil {
					t.Fatalf("buildDetector(%q): %v", wrapped, err)
				}
				bareRecs := drive(t, bare, stream)

				cfg := atCfg(wrapped, boundHowardEB, 0.05, defaultAnytimeDispersionWindowUs, indetStrict)
				wrapRecs := driveAnytime(t, cfg, stream)

				for i := range bareRecs {
					want := bareRecs[i].Result
					got := wrapRecs[i].Result
					if got.Signals["wrapped_level"] != float64(want.Level) {
						t.Fatalf("%s seed %d event %d: wrapped level %v, bare level %v", wrapped, seed, i, got.Signals["wrapped_level"], want.Level)
					}
					if got.Signals["wrapped_score"] != want.Score {
						t.Fatalf("%s seed %d event %d: wrapped score %v, bare score %v", wrapped, seed, i, got.Signals["wrapped_score"], want.Score)
					}
					for k, v := range want.Signals {
						if got.Signals["w_"+k] != v {
							t.Fatalf("%s seed %d event %d: forwarded signal %q = %v, bare = %v", wrapped, seed, i, k, got.Signals["w_"+k], v)
						}
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Absent block changes nothing.
// ---------------------------------------------------------------------------

func TestAnytime_AbsentBlockIsByteIdentical(t *testing.T) {
	// (a) Every roster detector built from an EMPTY config must produce a
	// byte-identical trace to the one its own constructor produces. This is the
	// "adding the block, the resolver, the registry case and the ownership row
	// changed nothing" claim, checked on serialized output rather than on fields.
	direct := map[string]func() Detector{
		"composite":     NewCompositeDetector,
		"threshold":     func() Detector { return NewThresholdDetector(defaultThresholdMs) },
		"backlog-drift": NewBacklogDriftDetector,
		"peak-rate":     NewPeakRateDetector,
	}
	for _, name := range AllDetectorNames() {
		for _, seed := range []int64{7, 8} {
			stream := superStream(seed, 1)

			viaConfig, err := BuildDetector(name, SaturationConfig{})
			if err != nil {
				t.Fatalf("BuildDetector(%q, empty): %v", name, err)
			}
			gotJSON, err := json.Marshal(drive(t, viaConfig, stream))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			wantJSON, err := json.Marshal(drive(t, direct[name](), stream))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("%s seed %d: empty-config trace is not byte-identical to the direct constructor's", name, seed)
			}
		}
	}

	// (b) The wrapper is NOT in the bank roster, so `--detectors all` is unchanged.
	for _, n := range AllDetectorNames() {
		if n == anytimeName {
			t.Fatalf("%q must not be in the bank roster: it would double-count its wrapped detector under --detectors all", anytimeName)
		}
	}

	// (c) Selecting the wrapper with NO block is a loud error, not a defaulted
	// wrapped detector: a silently chosen target would measure a different detector
	// than the operator selected.
	if _, err := BuildDetector(anytimeName, SaturationConfig{}); err == nil {
		t.Errorf("BuildDetector(%q) with no anytime block should error", anytimeName)
	} else if !strings.Contains(err.Error(), "anytime.wrapped") {
		t.Errorf("error should name anytime.wrapped, got: %v", err)
	}

	// (d) An anytime block supplied alongside a DIFFERENT selected detector is a
	// hard error, never a silent drop (R1).
	w := "composite"
	withBlock := SaturationConfig{Anytime: &AnytimeBlock{Wrapped: &w}}
	for _, name := range AllDetectorNames() {
		if _, err := BuildDetector(name, withBlock); err == nil {
			t.Errorf("anytime block + --detectors %s should be rejected", name)
		}
	}
	if _, err := NewBank([]string{anytimeName, "composite"}, SaturationConfig{}, NewInMemoryCollector()); err == nil {
		t.Errorf("--detectors %s,composite should be rejected: the wrapper is not a bank member", anytimeName)
	}
}

// ---------------------------------------------------------------------------
// 3. The CS target is score-specific.
// ---------------------------------------------------------------------------

func TestAnytime_TargetIsPerWrappedDetector(t *testing.T) {
	for _, seed := range []int64{21, 22, 23} {
		stream := superStream(seed, 1)

		peakRecs := driveAnytime(t, atCfg("peak-rate", boundHowardEB, 0.05, defaultAnytimeDispersionWindowUs, indetStrict), stream)
		compRecs := driveAnytime(t, atCfg("composite", boundHowardEB, 0.05, defaultAnytimeDispersionWindowUs, indetStrict), stream)

		// peak-rate's boundary is the CRITICAL GROWTH EXPONENT: a constant 1/2, with
		// no capacity estimate and no latency target in it.
		for i, r := range peakRecs {
			if got := r.Result.Signals["anytime_boundary"]; got != anytimePeakGammaBoundary {
				t.Fatalf("seed %d event %d: peak-rate boundary %v, want the critical exponent %v", seed, i, got, anytimePeakGammaBoundary)
			}
		}

		// composite's boundary is its own MOVING noise floor, so it must vary over
		// the stream -- if it were also 1/2 the wrapper would be measuring one
		// generic target for two different statistics.
		distinct := map[float64]bool{}
		for _, r := range compRecs {
			distinct[r.Result.Signals["anytime_boundary"]] = true
		}
		if len(distinct) < 5 {
			t.Errorf("seed %d: composite boundary took only %d distinct values; it should track 1/sqrt(arrivals)", seed, len(distinct))
		}

		// And it must equal composite's floor exactly, recomputed from the wrapper's
		// own arrival count.
		for i, r := range compRecs {
			arrivals := r.Result.Signals["arrivals"]
			want := 1.0
			if arrivals > 0 {
				want = math.Min(1, defaultCompositeSensitivity/math.Sqrt(arrivals))
			}
			if got := r.Result.Signals["anytime_boundary"]; math.Abs(got-want) > 1e-12 {
				t.Fatalf("seed %d event %d: composite boundary %v, want the sensitivity-scaled noise floor %v", seed, i, got, want)
			}
		}

		// The centers are different statistics, so they must not coincide.
		same := 0
		for i := range peakRecs {
			if peakRecs[i].Result.Signals["anytime_center"] == compRecs[i].Result.Signals["anytime_center"] {
				same++
			}
		}
		if same == len(peakRecs) {
			t.Errorf("seed %d: the two wrappings produced an identical center at every event; the target is not per-detector", seed)
		}
	}

	// The knob is passed through to the WRAPPED detector's own dial, which is what
	// makes the wrapper calibratable at all: a larger composite sensitivity must
	// raise the boundary proportionally.
	stream := superStream(31, 1)
	base := atCfg("composite", boundHowardEB, 0.05, defaultAnytimeDispersionWindowUs, indetStrict)
	loose := base
	loose.Threshold, loose.ThresholdSet = 4.0, true
	baseRecs := driveAnytime(t, base, stream)
	looseRecs := driveAnytime(t, loose, stream)
	for i := range baseRecs {
		lo := baseRecs[i].Result.Signals["anytime_boundary"]
		hi := looseRecs[i].Result.Signals["anytime_boundary"]
		if hi < lo {
			t.Fatalf("event %d: sensitivity 4.0 gave a LOWER boundary (%v) than 1.0 (%v); the knob is inverted or inert", i, hi, lo)
		}
	}
	if firstFiredIndex(looseRecs) < firstFiredIndex(baseRecs) {
		t.Errorf("a 4x less sensitive knob fired EARLIER (%d vs %d); the pass-through knob is not moving the false-alarm rate",
			firstFiredIndex(looseRecs), firstFiredIndex(baseRecs))
	}
}

// ---------------------------------------------------------------------------
// 4. Interval hygiene at every look, for every configuration.
// ---------------------------------------------------------------------------

func TestAnytime_IntervalWellFormedAtEveryN(t *testing.T) {
	for _, cfg := range sweepConfigs() {
		for _, seed := range []int64{41, 42, 43} {
			for _, burst := range []int{1, 4} {
				for regime, stream := range map[string][]Event{
					"super":   superStream(seed, burst),
					"healthy": atHealthyStream(seed, burst),
				} {
					for i, rec := range driveAnytime(t, cfg, stream) {
						s := rec.Result.Signals
						lo, center, hi := s["anytime_lo"], s["anytime_center"], s["anytime_hi"]
						for name, v := range map[string]float64{"lo": lo, "center": center, "hi": hi, "width": s["anytime_width"], "boundary": s["anytime_boundary"], "n_eff": s["n_eff"], "dispersion": s["dispersion"]} {
							if math.IsNaN(v) || math.IsInf(v, 0) {
								t.Fatalf("%s/%s/burst%d seed %d event %d: %s is not finite (%v)", cfg.Wrapped, cfg.Bound, burst, seed, i, name, v)
							}
						}
						if !(lo <= center && center <= hi) {
							t.Fatalf("%s/%s/%s/burst%d seed %d event %d: interval not ordered: lo=%v center=%v hi=%v",
								cfg.Wrapped, cfg.Bound, regime, burst, seed, i, lo, center, hi)
						}
						if s["dispersion"] < 0 {
							t.Fatalf("%s seed %d event %d: dispersion is negative (%v)", cfg.Wrapped, seed, i, s["dispersion"])
						}
					}
				}
			}
		}
	}
}

func TestAnytime_IntervalRespectsSupport(t *testing.T) {
	// Both targets live on [0,1] -- a growth exponent (0 healthy, 1 overloaded) and
	// a mean composite score. An endpoint outside the support is not conservatism,
	// it is a statement the statistic cannot make.
	for _, cfg := range sweepConfigs() {
		for _, seed := range []int64{51, 52} {
			for _, stream := range [][]Event{superStream(seed, 1), atHealthyStream(seed, 8), superStream(seed, 16)} {
				for i, rec := range driveAnytime(t, cfg, stream) {
					s := rec.Result.Signals
					for name, v := range map[string]float64{"lo": s["anytime_lo"], "center": s["anytime_center"], "hi": s["anytime_hi"], "boundary": s["anytime_boundary"], "score": rec.Result.Score} {
						if v < 0 || v > 1 {
							t.Fatalf("%s/%s seed %d event %d: %s = %v is outside the [0,1] support", cfg.Wrapped, cfg.Bound, seed, i, name, v)
						}
					}
					if c := rec.Result.Confidence; c < 0 || c > 1 {
						t.Fatalf("%s seed %d event %d: confidence %v outside [0,1]", cfg.Wrapped, seed, i, c)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 5. The two monotonicities the mechanism's behaviour rests on.
// ---------------------------------------------------------------------------

func TestAnytime_WidthShrinksWithObservations(t *testing.T) {
	// More evidence never widens the interval -- for EVERY bound family, coverage
	// level and measured variance, not just at a convenient pair of points.
	neffs := []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 4096, 20000}
	for _, bound := range []string{boundHowardEB, boundMixtureSPRT} {
		for _, alpha := range []float64{0.001, 0.01, 0.05, 0.10, 0.5} {
			for _, vhat := range []float64{0, 0.01, 0.05, 0.1, 0.25} {
				prev := math.Inf(1)
				for _, neff := range neffs {
					w := anytimeWidth(bound, vhat, neff, alpha)
					if math.IsNaN(w) || w < 0 {
						t.Fatalf("%s alpha=%v vhat=%v neff=%v: width %v is not a usable half-width", bound, alpha, vhat, neff, w)
					}
					if w > prev+1e-12 {
						t.Fatalf("%s alpha=%v vhat=%v: width grew with evidence (%v at neff=%v after %v)", bound, alpha, vhat, w, neff, prev)
					}
					prev = w
				}
				// And it must shrink SUBSTANTIALLY, not merely fail to grow: a bound
				// that flattens out never commits, which is the indeterminate-forever
				// failure mode.
				wide := anytimeWidth(bound, vhat, 8, alpha)
				tight := anytimeWidth(bound, vhat, 4096, alpha)
				if tight >= wide/4 {
					t.Errorf("%s alpha=%v vhat=%v: width only went %v -> %v from neff 8 -> 4096; too flat to ever commit", bound, alpha, vhat, wide, tight)
				}
			}
		}
	}
}

func TestAnytime_CoverageHoldsUnderAlpha(t *testing.T) {
	// The defining property: the interval must contain the true mean at EVERY look,
	// with probability at least 1-alpha over the whole sequence. Checked by Monte
	// Carlo over i.i.d. Bernoulli streams, counting a trial as a miss if the true
	// mean ever escapes -- which is the union over looks that a fixed-n interval
	// would fail and an anytime-valid one must not.
	const trials = 300
	const nMax = 400
	for _, bound := range []string{boundHowardEB, boundMixtureSPRT} {
		for _, alpha := range []float64{0.05, 0.10} {
			for _, p := range []float64{0.05, 0.3, 0.5, 0.9} {
				misses := 0
				for trial := 0; trial < trials; trial++ {
					rng := rand.New(rand.NewSource(int64(trial)*1_000_003 + int64(p*1000)))
					var sum, sumSq float64
					escaped := false
					for n := 1; n <= nMax; n++ {
						x := 0.0
						if rng.Float64() < p {
							x = 1
						}
						sum += x
						sumSq += x * x
						if n < anytimeMinWindow {
							continue
						}
						fn := float64(n)
						mean := sum / fn
						vhat := math.Max(0, sumSq/fn-mean*mean)
						w := anytimeWidth(bound, vhat, fn, alpha)
						if math.Abs(mean-p) > w {
							escaped = true
							break
						}
					}
					if escaped {
						misses++
					}
				}
				if rate := float64(misses) / trials; rate > alpha {
					t.Errorf("%s alpha=%v p=%v: empirical miscoverage %.3f exceeds the budget %.3f (%d/%d trials escaped)", bound, alpha, p, rate, alpha, misses, trials)
				}
			}
		}
	}
}

func TestAnytime_SmallerAlphaNeverCommitsEarlier(t *testing.T) {
	// Unit level: demanding more coverage cannot narrow the interval.
	for _, bound := range []string{boundHowardEB, boundMixtureSPRT} {
		for _, vhat := range []float64{0, 0.05, 0.25} {
			for _, neff := range []float64{1, 5, 40, 300, 5000} {
				prev := 0.0
				for _, alpha := range []float64{0.5, 0.10, 0.05, 0.01, 0.001} { // decreasing
					w := anytimeWidth(bound, vhat, neff, alpha)
					if w < prev-1e-12 {
						t.Fatalf("%s vhat=%v neff=%v: alpha=%v gave a NARROWER width (%v) than a looser alpha (%v)", bound, vhat, neff, alpha, w, prev)
					}
					prev = w
				}
			}
		}
	}

	// End to end: on the same stream, the stricter coverage level must not fire
	// before the looser one.
	for _, wrapped := range []string{"composite", "peak-rate"} {
		for _, bound := range []string{boundHowardEB, boundMixtureSPRT} {
			for _, seed := range []int64{61, 62, 63, 64} {
				for _, burst := range []int{1, 4} {
					stream := superStream(seed, burst)
					loose := firstFiredIndex(driveAnytime(t, atCfg(wrapped, bound, 0.10, defaultAnytimeDispersionWindowUs, indetStrict), stream))
					strict := firstFiredIndex(driveAnytime(t, atCfg(wrapped, bound, 0.01, defaultAnytimeDispersionWindowUs, indetStrict), stream))
					if strict < loose {
						t.Errorf("%s/%s seed %d burst %d: alpha=0.01 fired at %d, EARLIER than alpha=0.10 at %d", wrapped, bound, seed, burst, strict, loose)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 6. Dispersion is MEASURED, and it is what widens the interval.
// ---------------------------------------------------------------------------

// poissonArrivals returns arrival timestamps (us) from a Poisson process.
func poissonArrivals(seed int64, n int, ratePerSec float64) []int64 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int64, 0, n)
	tNow := 0.0
	for i := 0; i < n; i++ {
		tNow += rng.ExpFloat64() / ratePerSec * 1e6
		out = append(out, int64(tNow))
	}
	return out
}

// burstyArrivals releases arrivals in simultaneous batches of size b at the same
// MEAN rate, so only the dispersion changes.
func burstyArrivals(seed int64, n int, ratePerSec float64, b int) []int64 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int64, 0, n)
	tNow := 0.0
	for len(out) < n {
		tNow += rng.ExpFloat64() / ratePerSec * 1e6 * float64(b)
		for j := 0; j < b && len(out) < n; j++ {
			out = append(out, int64(tNow))
		}
	}
	return out
}

func measureDispersion(windowUs int64, ts []int64) float64 {
	d := dispersionMeter{windowUs: windowUs}
	for _, t := range ts {
		d.observeArrival(t)
	}
	return d.index()
}

func TestAnytime_DispersionIsOneOnPoisson(t *testing.T) {
	// The index of dispersion of a Poisson counting process is exactly 1. Anything
	// else means the meter is measuring something other than Var(N_T)/E[N_T], and the
	// width correction would then be scaled by a number with no interpretation.
	for _, seed := range []int64{71, 72, 73, 74, 75} {
		for _, rate := range []float64{10, 40, 200} {
			for _, perBucket := range []float64{5, 20} {
				// Bucket width chosen for a target mean count; ~400 buckets of evidence.
				windowUs := int64(perBucket / rate * 1e6)
				n := int(perBucket * 400)
				got := measureDispersion(windowUs, poissonArrivals(seed, n, rate))
				if got < 0.7 || got > 1.4 {
					t.Errorf("seed %d rate %v mean-count %v: measured dispersion %.3f, want ~1 for Poisson", seed, rate, perBucket, got)
				}
			}
		}
	}

	// A near-deterministic (constant-rate) process is UNDER-dispersed, and the meter
	// must say so rather than reporting the neutral 1 -- otherwise "measured" would be
	// indistinguishable from "assumed".
	const rate = 50.0
	windowUs := int64(20 / rate * 1e6)
	constant := make([]int64, 0, 8000)
	for i := 0; i < 8000; i++ {
		constant = append(constant, int64(float64(i)/rate*1e6))
	}
	if got := measureDispersion(windowUs, constant); got > 0.2 {
		t.Errorf("constant arrivals: measured dispersion %.3f, want well below 1", got)
	}
}

func TestAnytime_DispersionRisesWithBurstiness(t *testing.T) {
	// Same mean rate, increasing batch size: the measured index must rise
	// monotonically. This is the ordering the width correction depends on; if it
	// inverted, bursty traffic would get a NARROWER interval than Poisson.
	const rate = 50.0
	windowUs := int64(20 / rate * 1e6)
	for _, seed := range []int64{81, 82, 83} {
		prev := 0.0
		for _, b := range []int{1, 2, 4, 8, 16} {
			got := measureDispersion(windowUs, burstyArrivals(seed, 8000, rate, b))
			if b > 1 && got < prev {
				t.Errorf("seed %d: burst %d measured dispersion %.3f, below burst-%d's %.3f", seed, b, got, b/2, prev)
			}
			prev = got
		}
		plain := measureDispersion(windowUs, burstyArrivals(seed, 8000, rate, 1))
		heavy := measureDispersion(windowUs, burstyArrivals(seed, 8000, rate, 16))
		if heavy <= plain*2 {
			t.Errorf("seed %d: burst-16 dispersion %.3f is not materially above burst-1's %.3f", seed, heavy, plain)
		}
	}
}

func TestAnytime_HigherDispersionWidensInterval(t *testing.T) {
	// The width must scale like sqrt(I * loglog(n) / n) with the MEASURED index of
	// dispersion I entering through the effective sample size. Two bounds are
	// provable and are asserted instead of a single fitted constant:
	//
	//   lower: at least ~sqrt(I) -- it is genuinely a sqrt law, not a token bump.
	//          Not EXACTLY sqrt(I) because the time-uniform loglog penalty also
	//          shrinks with the (smaller) effective sample size.
	//   upper: at most I -- both terms of every bound scale by less than I, so a
	//          larger response would mean the correction is not entering as an
	//          effective sample size at all.
	//
	// The sweep includes the campaign's MEASURED ladder values, so the test speaks
	// about the dispersions the apparatus actually produces.
	dispersions := []float64{1.0, 1.08, 2.08, 4.0, 8.49, 9.40}
	for _, bound := range []string{boundHowardEB, boundMixtureSPRT} {
		for _, alpha := range []float64{0.01, 0.05, 0.10} {
			for _, vhat := range []float64{0, 0.05, 0.25} {
				for _, windowN := range []float64{200, 800, 4000} {
					base := anytimeWidth(bound, vhat, windowN/math.Max(1, dispersions[0]), alpha)
					prev := base
					for _, disp := range dispersions {
						w := anytimeWidth(bound, vhat, windowN/math.Max(1, disp), alpha)
						if w < prev-1e-12 {
							t.Fatalf("%s alpha=%v vhat=%v N=%v: dispersion %v NARROWED the interval (%v after %v)", bound, alpha, vhat, windowN, disp, w, prev)
						}
						prev = w
						ratio := w / base
						if ratio < 0.8*math.Sqrt(disp) {
							t.Errorf("%s alpha=%v vhat=%v N=%v: dispersion %v widened by only %.3fx; a sqrt law needs ~%.3fx", bound, alpha, vhat, windowN, disp, ratio, math.Sqrt(disp))
						}
						if ratio > disp+1e-9 {
							t.Errorf("%s alpha=%v vhat=%v N=%v: dispersion %v widened by %.3fx, more than the effective-sample-size limit %.3fx", bound, alpha, vhat, windowN, disp, ratio, disp)
						}
					}
				}
			}
		}
	}

	// End to end: only the max(1, I) correction may widen, never narrow. Constant
	// (under-dispersed) arrivals must not get a TIGHTER interval than the i.i.d.
	// bound allows, because the martingale argument does not license it.
	if got := anytimeWidth(boundHowardEB, 0.25, 500/math.Max(1, 0.002), 0.05); got != anytimeWidth(boundHowardEB, 0.25, 500, 0.05) {
		t.Errorf("an under-dispersed arrival process narrowed the interval; max(1, I) must only widen")
	}
}

// ---------------------------------------------------------------------------
// 7. The pre-registered rung rules.
// ---------------------------------------------------------------------------

// rec builds one synthetic trace record at a given verdict.
func atRec(level Level, undecided bool) TraceRecord {
	u := 0.0
	if undecided {
		u = 1
	}
	return TraceRecord{Detector: anytimeName, Result: Result{Level: level, Signals: map[string]float64{anytimeUndecidedSignal: u}}}
}

func TestAnytime_UndecidedExcludedFromDenominator(t *testing.T) {
	// The declared rule, on a composition where the two candidate denominators
	// disagree about the VERDICT rather than merely about the number: 6 fired out of
	// 10 decided is 0.60 (fires at the 0.5 threshold), while charging the 10
	// undecided records as STABLE gives 6/20 = 0.30 (does not fire).
	var records []TraceRecord
	for i := 0; i < 10; i++ {
		records = append(records, atRec(Stable, true))
	}
	for i := 0; i < 6; i++ {
		records = append(records, atRec(Overloaded, false))
	}
	for i := 0; i < 4; i++ {
		records = append(records, atRec(Stable, false))
	}

	frac, decided := AnytimeFiredFraction(records)
	if decided != 10 {
		t.Fatalf("decided count %d, want 10 (the 10 undecided records must not be in the denominator)", decided)
	}
	if math.Abs(frac-0.6) > 1e-12 {
		t.Fatalf("fired fraction %v, want 0.6", frac)
	}
	if !AnytimeRungFired(records, 0.5, 10) {
		t.Errorf("rung should fire: 0.60 of decided records fired at threshold 0.5")
	}

	// The min-decided floor. Without it a single decided-and-firing record carries a
	// rung at fraction 1.0, so the detector that commits least often would score as
	// the most decisive one.
	if AnytimeRungFired(records, 0.5, 11) {
		t.Errorf("rung should NOT fire with only 10 decided records against a floor of 11")
	}
	lone := []TraceRecord{atRec(Stable, true), atRec(Stable, true), atRec(Overloaded, false)}
	if f, d := AnytimeFiredFraction(lone); f != 1.0 || d != 1 {
		t.Fatalf("lone decided record: got fraction %v over %d decided, want 1.0 over 1", f, d)
	}
	if AnytimeRungFired(lone, 0.5, 20) {
		t.Errorf("a single decided record must not carry a rung against a floor of 20")
	}

	// Swept: for randomized compositions the fraction is exactly fired/decided, and
	// adding undecided records never changes it.
	for seed := int64(0); seed < 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		nFired, nStable, nUndecided := rng.Intn(30), rng.Intn(30), rng.Intn(40)
		var rs []TraceRecord
		for i := 0; i < nFired; i++ {
			rs = append(rs, atRec(Overloaded, false))
		}
		for i := 0; i < nStable; i++ {
			rs = append(rs, atRec(Stable, false))
		}
		base, baseDecided := AnytimeFiredFraction(rs)
		for i := 0; i < nUndecided; i++ {
			rs = append(rs, atRec(Stable, true))
		}
		rng.Shuffle(len(rs), func(i, j int) { rs[i], rs[j] = rs[j], rs[i] })
		got, gotDecided := AnytimeFiredFraction(rs)
		if gotDecided != baseDecided || math.Abs(got-base) > 1e-12 {
			t.Fatalf("seed %d: %d undecided records changed the verdict from %v/%d to %v/%d", seed, nUndecided, base, baseDecided, got, gotDecided)
		}
		if baseDecided > 0 {
			want := float64(nFired) / float64(nFired+nStable)
			if math.Abs(got-want) > 1e-12 {
				t.Fatalf("seed %d: fraction %v, want fired/decided = %v", seed, got, want)
			}
		}
	}
}

func TestAnytime_FlipCountIgnoresUndecided(t *testing.T) {
	// Declining to speak is not a retraction. Committing to the opposite verdict is.
	cases := []struct {
		name  string
		recs  []TraceRecord
		flips int
	}{
		{"fired -> undecided -> fired", []TraceRecord{atRec(Overloaded, false), atRec(Stable, true), atRec(Overloaded, false)}, 0},
		{"fired -> stable -> fired", []TraceRecord{atRec(Overloaded, false), atRec(Stable, false), atRec(Overloaded, false)}, 1},
		{"fired -> undecided -> stable", []TraceRecord{atRec(Overloaded, false), atRec(Stable, true), atRec(Stable, false)}, 1},
		{"all undecided", []TraceRecord{atRec(Stable, true), atRec(Stable, true)}, 0},
		{"backlogged counts as fired", []TraceRecord{atRec(Backlogged, false), atRec(Stable, false)}, 1},
		{"empty", nil, 0},
	}
	for _, c := range cases {
		if got := AnytimeFlipCount(c.recs); got != c.flips {
			t.Errorf("%s: flip count %d, want %d", c.name, got, c.flips)
		}
	}

	// Swept metamorphic property: splicing undecided records ANYWHERE into a decided
	// sequence must leave the flip count unchanged.
	for seed := int64(0); seed < 80; seed++ {
		rng := rand.New(rand.NewSource(seed + 500))
		n := 5 + rng.Intn(40)
		decided := make([]TraceRecord, 0, n)
		for i := 0; i < n; i++ {
			if rng.Intn(2) == 0 {
				decided = append(decided, atRec(Stable, false))
			} else {
				decided = append(decided, atRec(Level(1+rng.Intn(2)), false))
			}
		}
		want := AnytimeFlipCount(decided)

		spliced := make([]TraceRecord, 0, 2*n)
		for _, r := range decided {
			for k := rng.Intn(3); k > 0; k-- {
				spliced = append(spliced, atRec(Stable, true))
			}
			spliced = append(spliced, r)
		}
		for k := rng.Intn(3); k > 0; k-- {
			spliced = append(spliced, atRec(Stable, true))
		}
		if got := AnytimeFlipCount(spliced); got != want {
			t.Fatalf("seed %d: splicing undecided records changed the flip count from %d to %d", seed, want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. Undecidedness never becomes a fourth Level.
// ---------------------------------------------------------------------------

func TestAnytime_ReportRoundTripsWithoutLevelWidening(t *testing.T) {
	for _, cfg := range sweepConfigs() {
		for _, seed := range []int64{91, 92} {
			for _, stream := range [][]Event{superStream(seed, 1), atHealthyStream(seed, 4)} {
				records := driveAnytime(t, cfg, stream)

				// (a) Only the three declared levels are ever emitted. A fourth constant
				// would be indexed out of range by ReduceOne's [3]int and would come
				// back from a report as STABLE (Level.UnmarshalJSON's default) -- a
				// silent verdict change.
				sawUndecided := false
				for i, r := range records {
					if r.Result.Level < Stable || r.Result.Level > Overloaded {
						t.Fatalf("%s/%s seed %d event %d: level %d is outside the three declared levels", cfg.Wrapped, cfg.Bound, seed, i, r.Result.Level)
					}
					if !AnytimeRecordDecided(r.Result) {
						sawUndecided = true
						if r.Result.Level != Stable {
							t.Fatalf("event %d: an undecided verdict reported level %v; it must report STABLE and carry undecidedness in Signals", i, r.Result.Level)
						}
					}
				}
				if cfg.IndeterminatePolicy == indetStrict && !sawUndecided {
					t.Errorf("%s/%s seed %d: strict policy never reported an undecided record; the out-of-band flag is not being emitted", cfg.Wrapped, cfg.Bound, seed)
				}
				if cfg.IndeterminatePolicy == indetLeanStable && sawUndecided {
					t.Errorf("%s/%s seed %d: lean_stable reported an undecided record; it must report STABLE instead", cfg.Wrapped, cfg.Bound, seed)
				}

				// (b) The report round-trips: levels AND the out-of-band flag survive
				// serialization, so a downstream reader reaches the same verdict.
				blob, err := json.Marshal(records)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				var back []TraceRecord
				if err := json.Unmarshal(blob, &back); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(back) != len(records) {
					t.Fatalf("round trip changed the record count: %d -> %d", len(records), len(back))
				}
				for i := range records {
					if back[i].Result.Level != records[i].Result.Level {
						t.Fatalf("event %d: level %v -> %v across the report", i, records[i].Result.Level, back[i].Result.Level)
					}
					if AnytimeRecordDecided(back[i].Result) != AnytimeRecordDecided(records[i].Result) {
						t.Fatalf("event %d: decidedness did not survive the report", i)
					}
				}

				// (c) The existing reducer consumes the trace unchanged and returns one of
				// the three levels.
				if lvl := ReduceOne(records, 30_000_000); lvl < Stable || lvl > Overloaded {
					t.Fatalf("ReduceOne returned %d, outside the three declared levels", lvl)
				}
				// And the pre-registered rules agree with the reducer's own inputs.
				if _, decided := AnytimeFiredFraction(records); decided > len(records) {
					t.Fatalf("decided count %d exceeds record count %d", decided, len(records))
				}
			}
		}
	}

	// An undecided record specifically: it marshals as STABLE, and the flag is what
	// carries the distinction.
	blob, err := json.Marshal(atRec(Stable, true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"level":"STABLE"`) {
		t.Errorf("an undecided record should marshal with level STABLE, got %s", blob)
	}
	if !strings.Contains(string(blob), `"undecided":1`) {
		t.Errorf("an undecided record should carry undecided=1 in signals, got %s", blob)
	}
}

// ---------------------------------------------------------------------------
// 9. Loud failure, and every declared factor level reachable through the block.
// ---------------------------------------------------------------------------

func TestAnytime_InvalidConfigFailsLoudly(t *testing.T) {
	str := func(s string) *string { return &s }
	f := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }

	cases := []struct {
		name  string
		block *AnytimeBlock
		field string
	}{
		{"nil block", nil, "anytime.wrapped"},
		{"missing wrapped", &AnytimeBlock{Bound: str(boundHowardEB)}, "anytime.wrapped"},
		{"unknown wrapped", &AnytimeBlock{Wrapped: str("backlog-drift")}, "anytime.wrapped"},
		{"wrapped is itself", &AnytimeBlock{Wrapped: str(anytimeName)}, "anytime.wrapped"},
		{"unknown bound", &AnytimeBlock{Wrapped: str("composite"), Bound: str("bootstrap")}, "anytime.bound"},
		{"alpha zero", &AnytimeBlock{Wrapped: str("composite"), Alpha: f(0)}, "anytime.alpha"},
		{"alpha one", &AnytimeBlock{Wrapped: str("composite"), Alpha: f(1)}, "anytime.alpha"},
		{"alpha negative", &AnytimeBlock{Wrapped: str("composite"), Alpha: f(-0.1)}, "anytime.alpha"},
		{"alpha NaN", &AnytimeBlock{Wrapped: str("composite"), Alpha: f(math.NaN())}, "anytime.alpha"},
		{"dispersion window zero", &AnytimeBlock{Wrapped: str("composite"), DispersionWindowUs: i64(0)}, "anytime.dispersion_window_us"},
		{"dispersion window negative", &AnytimeBlock{Wrapped: str("composite"), DispersionWindowUs: i64(-1)}, "anytime.dispersion_window_us"},
		{"unknown indeterminate policy", &AnytimeBlock{Wrapped: str("composite"), IndeterminatePolicy: str("lean_overloaded")}, "anytime.indeterminate_policy"},
		{"threshold zero", &AnytimeBlock{Wrapped: str("composite"), Threshold: f(0)}, "anytime.threshold"},
		{"threshold negative", &AnytimeBlock{Wrapped: str("composite"), Threshold: f(-1)}, "anytime.threshold"},
		{"threshold subnormal", &AnytimeBlock{Wrapped: str("composite"), Threshold: f(1e-12)}, "anytime.threshold"},
		{"threshold NaN", &AnytimeBlock{Wrapped: str("composite"), Threshold: f(math.NaN())}, "anytime.threshold"},
		{"threshold Inf", &AnytimeBlock{Wrapped: str("composite"), Threshold: f(math.Inf(1))}, "anytime.threshold"},
	}
	for _, c := range cases {
		_, err := BuildDetector(anytimeName, SaturationConfig{Anytime: c.block})
		if err == nil {
			t.Errorf("%s: should be rejected, got no error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: error should name %s, got: %v", c.name, c.field, err)
		}
	}

	// An unknown key inside the block is rejected by the strict decoder, so a
	// misspelled factor cannot silently measure the default corner.
	if _, err := parseSaturationYAML(t, "anytime:\n  wrapped: composite\n  dispersion_windows_us: 100\n"); err == nil {
		t.Errorf("a misspelled key inside anytime: should be rejected by strict parsing")
	}
}

func TestAnytime_EveryDeclaredLevelIsReachable(t *testing.T) {
	// Every factor level the campaign declares must be reachable through the
	// anytime: block ALONE, and must survive into the resolved config. A level that
	// parses but resolves to something else would silently measure a corner nobody
	// selected -- and the resolved value is what the manipulation check reads.
	type want struct {
		wrapped string
		bound   string
		alpha   float64
		dispWin int64
		indet   string
	}
	base := want{"composite", boundHowardEB, 0.05, 2_000_000, indetStrict}

	var cases []struct {
		yaml string
		want want
	}
	add := func(y string, w want) {
		cases = append(cases, struct {
			yaml string
			want want
		}{y, w})
	}

	for _, w := range []string{"composite", "peak_rate"} {
		exp := base
		exp.wrapped = w
		if w == "peak_rate" {
			exp.wrapped = "peak-rate"
		}
		add(fmt.Sprintf("anytime:\n  wrapped: %s\n  bound: howard_eb\n  alpha: 0.05\n  dispersion_window_us: 2000000\n  indeterminate_policy: strict\n", w), exp)
	}
	for _, b := range []string{boundHowardEB, boundMixtureSPRT} {
		exp := base
		exp.bound = b
		add(fmt.Sprintf("anytime:\n  wrapped: composite\n  bound: %s\n  alpha: 0.05\n  dispersion_window_us: 2000000\n  indeterminate_policy: strict\n", b), exp)
	}
	for _, a := range []float64{0.01, 0.05, 0.10} {
		exp := base
		exp.alpha = a
		add(fmt.Sprintf("anytime:\n  wrapped: composite\n  bound: howard_eb\n  alpha: %v\n  dispersion_window_us: 2000000\n  indeterminate_policy: strict\n", a), exp)
	}
	for _, d := range []int64{500_000, 2_000_000, 5_000_000, 10_000_000} {
		exp := base
		exp.dispWin = d
		add(fmt.Sprintf("anytime:\n  wrapped: composite\n  bound: howard_eb\n  alpha: 0.05\n  dispersion_window_us: %d\n  indeterminate_policy: strict\n", d), exp)
	}
	for _, ind := range []string{indetStrict, indetLeanStable} {
		exp := base
		exp.indet = ind
		add(fmt.Sprintf("anytime:\n  wrapped: composite\n  bound: howard_eb\n  alpha: 0.05\n  dispersion_window_us: 2000000\n  indeterminate_policy: %s\n", ind), exp)
	}

	for _, c := range cases {
		cfg, err := parseSaturationYAML(t, c.yaml)
		if err != nil {
			t.Fatalf("parse %q: %v", c.yaml, err)
		}
		resolved, err := resolveAnytimeConfig(cfg.Anytime)
		if err != nil {
			t.Fatalf("resolve %q: %v", c.yaml, err)
		}
		if resolved.Wrapped != c.want.wrapped || resolved.Bound != c.want.bound ||
			resolved.Alpha != c.want.alpha || resolved.DispersionWindowUs != c.want.dispWin ||
			resolved.IndeterminatePolicy != c.want.indet {
			t.Errorf("resolved %+v, want %+v, from:\n%s", resolved, c.want, c.yaml)
		}
		// Constructible by name through the registry, which is the only path the CLI
		// has.
		if _, err := BuildDetector(anytimeName, cfg); err != nil {
			t.Errorf("BuildDetector(%q) from:\n%s\n: %v", anytimeName, c.yaml, err)
		}
	}

	// The threshold knob is the sixth pointer the harness rewrites; it must reach the
	// wrapped detector's own dial rather than being parsed and dropped.
	for _, w := range []string{"composite", "peak_rate"} {
		src := fmt.Sprintf("anytime:\n  wrapped: %s\n  threshold: 16.0\n", w)
		cfg, err := parseSaturationYAML(t, src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		resolved, err := resolveAnytimeConfig(cfg.Anytime)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !resolved.ThresholdSet || resolved.Threshold != 16.0 {
			t.Errorf("%s: threshold resolved to (%v, set=%v), want (16, true)", w, resolved.Threshold, resolved.ThresholdSet)
		}
		if _, err := newAnytimeDetector(resolved); err != nil {
			t.Errorf("%s: constructing with threshold 16.0: %v", w, err)
		}
	}
	// An absent threshold must leave the wrapped detector on its OWN default, not on
	// a zero (which every calibration floor rejects).
	cfg, err := parseSaturationYAML(t, "anytime:\n  wrapped: peak_rate\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolved, err := resolveAnytimeConfig(cfg.Anytime)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ThresholdSet {
		t.Errorf("an absent anytime.threshold must stay unset (R9), got %v", resolved.Threshold)
	}
	if _, err := newAnytimeDetector(resolved); err != nil {
		t.Errorf("constructing with an absent threshold: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 10. Streaming contract: pure query, reusable across legs.
// ---------------------------------------------------------------------------

func TestAnytime_DetectIsPureQueryAndResetIsComplete(t *testing.T) {
	for _, cfg := range sweepConfigs() {
		stream := superStream(101, 2)
		d, err := newAnytimeDetector(cfg)
		if err != nil {
			t.Fatalf("newAnytimeDetector: %v", err)
		}

		// Repeated Detect without an intervening Observe returns the same verdict: the
		// verdict is a function of the event stream, not of how often it is asked for.
		for i, e := range stream {
			d.Observe(e)
			first := d.Detect()
			for k := 0; k < 3; k++ {
				again := d.Detect()
				if again.Level != first.Level || again.Score != first.Score || again.Confidence != first.Confidence {
					t.Fatalf("%s event %d: repeated Detect changed the verdict", cfg.Wrapped, i)
				}
				if !reflect.DeepEqual(again.Signals, first.Signals) {
					t.Fatalf("%s event %d: repeated Detect changed the signals", cfg.Wrapped, i)
				}
			}
		}
		firstLeg, err := json.Marshal(drive(t, d, nil))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_ = firstLeg

		// Reset must clear the wrapper AND the wrapped detector, so a second replay leg
		// is byte-identical to the first.
		legA, err := json.Marshal(drive(t, mustAnytime(t, cfg), stream))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		d.Reset()
		legB, err := json.Marshal(drive(t, d, stream))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Equal(legA, legB) {
			t.Errorf("%s/%s: a replay leg after Reset is not byte-identical to a fresh detector's", cfg.Wrapped, cfg.Bound)
		}
	}
}

func mustAnytime(t *testing.T, cfg anytimeConfig) Detector {
	t.Helper()
	d, err := newAnytimeDetector(cfg)
	if err != nil {
		t.Fatalf("newAnytimeDetector: %v", err)
	}
	return d
}

// TestAnytime_SeparatesRegimes is the sanity check that the mechanism is worth
// enabling at all: the interval must commit to a firing verdict under
// super-capacity traffic. Without it every property above could be satisfied by a
// detector that never decides anything.
//
// The sub-capacity leg asserts the CONJUNCTION, which is the wrapper's own exact
// guarantee, rather than an absolute false-alarm rate. The distinction matters and
// an earlier version of this test got it wrong: composite by itself reports
// non-STABLE on 87-336 of 1600 records on several of these sub-capacity streams
// (its quartile filter validates a rising latency trend at rho=0.25), so a test
// demanding the wrapper stay silent there was really testing composite's
// calibration, which this feature does not change and must not be credited or
// blamed for. What the wrapper DOES guarantee is that it cannot manufacture an
// alarm the wrapped detector never raised -- the fire rule reads
// `lo > boundary && wrappedResult.Level != Stable`, so a stream the wrapped
// detector never flags can never latch. That is exact, falsifiable, and the
// property the wrapper is responsible for. Measured across the 72 cells below: 0
// violations.
//
// The inherited alarms are LOGGED rather than swallowed, so a run of this test
// still shows which sub-capacity cells carry a false alarm and how large the
// wrapped detector's own contribution was. Suppressing them silently is how a
// wrapper gets credited for its host's calibration.
func TestAnytime_SeparatesRegimes(t *testing.T) {
	inherited := 0
	for _, cfg := range sweepConfigs() {
		for _, seed := range []int64{111, 112, 113} {
			for _, burst := range []int{1, 4, 8} {
				superRecs := driveAnytime(t, cfg, superStream(seed, burst))
				if firstFiredIndex(superRecs) >= len(superRecs) {
					t.Errorf("%s/%s/%s seed %d burst %d: never fired on super-capacity traffic",
						cfg.Wrapped, cfg.Bound, cfg.IndeterminatePolicy, seed, burst)
				}

				// The wrapped detector's own verdicts on the same stream, from a bare
				// instance built through the same registry.
				stream := atHealthyStream(seed, burst)
				bare, err := BuildDetector(cfg.Wrapped, SaturationConfig{})
				if err != nil {
					t.Fatalf("BuildDetector(%q): %v", cfg.Wrapped, err)
				}
				bareNonStable := 0
				for _, e := range stream {
					bare.Observe(e)
					if bare.Detect().Level != Stable {
						bareNonStable++
					}
				}

				healthyRecs := driveAnytime(t, cfg, stream)
				wrapperFired := firstFiredIndex(healthyRecs) < len(healthyRecs)

				switch {
				case wrapperFired && bareNonStable == 0:
					t.Errorf("%s/%s/%s seed %d burst %d: wrapper fired on sub-capacity traffic the wrapped detector NEVER flagged; the conjunction in decide() is broken",
						cfg.Wrapped, cfg.Bound, cfg.IndeterminatePolicy, seed, burst)
				case wrapperFired:
					inherited++
					t.Logf("inherited alarm: %s/%s/%s seed %d burst %d -- wrapped detector itself flagged %d/%d records",
						cfg.Wrapped, cfg.Bound, cfg.IndeterminatePolicy, seed, burst, bareNonStable, len(stream))
				}
			}
		}
	}
	t.Logf("sub-capacity cells with an inherited (wrapped-detector) alarm: %d", inherited)
}
