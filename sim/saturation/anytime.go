// sim/saturation/anytime.go
package saturation

import (
	"fmt"
	"math"
)

// AnytimeDetector wraps another detector in an anytime-valid confidence sequence
// (CS) and commits to a verdict only once the interval clears the wrapped
// detector's own decision boundary.
//
// # What problem this solves
//
// Every incumbent detector answers "is the statistic past the line?" at each
// event. None of them answers "is the EVIDENCE sufficient?", so each one needs a
// debounce (peak-rate's consecutive_k, composite's 1/sqrt(arrivals) floor) whose
// job is to trade false alarms against delay by a fixed rule, chosen once, offline.
// A confidence sequence replaces that fixed rule with a measured one: it is a
// sequence of intervals I_1, I_2, ... with
//
//	P( theta in I_n for EVERY n ) >= 1 - alpha
//
// so the interval may be inspected after every event and acted on the moment it
// clears the boundary, with no penalty for looking (that is the "anytime-valid"
// property; a fixed-n confidence interval loses its coverage entirely under this
// kind of optional stopping). The detector therefore fires as early as the data
// allow and no earlier, rather than as early as a hand-set debounce allows.
//
// # It WRAPS rather than replaces
//
// The wrapped detector is constructed through the ordinary registry
// (buildDetector) and driven through the exported 4-method Detector interface.
// Every Event is forwarded verbatim and its Result is read, never edited. The
// three incumbent detector files are untouched by this feature. That is not
// politeness: the wrapper's whole claim is "same statistic, better stopping rule",
// which is only measurable if the statistic is provably the incumbent's.
//
// Consequently the wrapper cannot read the wrapped detector's internal state, and
// Result.Signals is an OUTPUT (a diagnostic map built fresh per Detect), not a
// state accessor. So the wrapper maintains its own copy of the few stream
// quantities it needs -- in-flight, running peak, arrivals, completions, observed
// span -- folded from the Event stream with the same rules the incumbents use
// (guarded decrement so a completion without its arrival cannot drive the backlog
// negative; symmetric min/max timestamps so an out-of-order first event cannot pin
// the span at zero).
//
// # The CS target is per-wrapped-detector, not generic
//
// A CS is a statement about the mean of an i.i.d.-ish sequence, so the target must
// be a mean. Neither incumbent's headline statistic is one, and the two fail to be
// one for DIFFERENT reasons, so there is no single generic target:
//
//   - peak-rate's R_t = Peak_t/t is a ratio of a running MAXIMUM to elapsed time.
//     A running max is not a mean of anything and drifts by construction, so a CS
//     on R_t would be a CS on a non-stationary quantity -- valid arithmetic,
//     meaningless coverage. What IS a mean is the record-setting indicator
//     X_i in {0,1} ("did event i set a new backlog high-water mark?"), because
//     in-flight moves by at most one per event, so a new peak is always the old
//     peak plus one and
//
//     sum of X_i over (n1, n]  ==  Peak_n - Peak_n1     (exactly)
//
//     The record RATE p = E[X] is therefore the exact discrete derivative
//     dPeak/dn, and the growth exponent the regimes separate on,
//
//     gamma = dlogPeak / dlogt   (1 = overloaded, 1/2 = critical, 0 = healthy)
//
//     is an EXACT and monotone function of p over the window (mapPeakGamma), so
//     the CS on p maps endpoint-to-endpoint onto a CS on gamma. Firing is
//     "the whole interval lies above 1/2" -- the critical exponent -- which needs
//     no capacity estimate and no latency target, exactly as peak-rate intends.
//
//   - composite's score is already a mean-like quantity in [0,1], but its decision
//     boundary MOVES: it is the sensitivity-scaled noise floor
//     sensitivity/sqrt(arrivals). So the target is the windowed mean score and the
//     boundary is recomputed per event from the wrapper's own arrival count.
//
// # Undecidedness is out of band, always
//
// A third verdict is the natural output of a CS that straddles the boundary, but
// Level MUST NOT grow a fourth constant. Level is an integer enum consumed by
// ReduceOne (which indexes a `[3]int` and scans severity from Overloaded down to
// Stable) and by Level.UnmarshalJSON (which silently maps any unrecognized string
// to STABLE). A fourth constant would therefore either panic the reducer or
// round-trip through a report as STABLE -- a silent verdict change, which is worse
// than not having the level at all. So an undecided verdict reports Level=Stable
// (matching R20: no evidence is STABLE, never a guess) and carries its
// undecidedness in Signals["undecided"] = 1, where the report round-trips it as
// data rather than as a level.
//
// indeterminate_policy chooses whether that flag is emitted at all:
//
//   - strict:      emit undecided=1 while undecided. Under the pre-registered
//     rung rule (undecided records excluded from the denominator)
//     the pre-commitment stretch is discounted, so a rung's verdict
//     is decided by the committed records alone.
//   - lean_stable: report STABLE with undecided=0. The pre-commitment stretch then
//     counts as genuine STABLE evidence in the denominator, so the
//     same event stream needs a longer firing run to carry a rung --
//     safer against alarm fatigue, slower to fire.
//
// # Verdicts latch
//
// The fire rule is a CONJUNCTION -- the wrapped detector must be firing AND the
// interval must lie wholly on the firing side -- and once either side commits, the
// verdict is held until the interval commits the other way. Two consequences worth
// stating:
//
//   - The wrapped detector's calibration knob (anytime.threshold, passed straight
//     through to composite.sensitivity or peak_rate.threshold) still moves the
//     wrapper's false-alarm rate. A wrapper whose verdict came only from its own CS
//     would have an INERT calibration knob, and a detector that cannot be moved
//     onto a matched false-alarm rate cannot be fairly compared to one that can
//     (#1614's whole point).
//   - Latching makes this a post-hoc verdict on a finished run ("did this run
//     saturate?"), the same question peak-rate's all-time high-water mark answers,
//     rather than a live "is it saturated right now?".
//
// # Cost
//
// O(1) additional state and O(1) additional work per event, both independent of
// trace length: a fixed set of scalars plus a 16-entry ring of prefix-sum
// checkpoints, so a trailing window's sums are a subtraction rather than a rescan,
// and the dispersion estimator keeps three running scalars over closed buckets.
// Nothing is retained per event and nothing is sorted. The wrapper's own asymptotic
// cost is therefore strictly below the wrapped detector's (composite retains every
// event and re-sorts its completions on every Detect; peak-rate is O(1) like this
// wrapper but pays a fixed 20-observation gate the CS replaces with a measured
// one).
type AnytimeDetector struct {
	cfg     anytimeConfig
	wrapped Detector

	// Stream state, folded from the Event stream. Mirrored here rather than read
	// back from the wrapped detector because Result.Signals is a diagnostic output,
	// not a state accessor (see the type docs).
	n           int64
	arrivals    int64
	completions int64
	inFlight    int64
	peak        int64
	firstTsUs   int64
	lastTsUs    int64
	haveFirst   bool

	// Samples begin at ADMISSION -- the first Completion. Before it the server has
	// produced no output at all, so composite's rate deficit is pinned at 1 and every
	// event sets a new peak, in EVERY regime including a healthy one. That evidence
	// is not weak, it is tautological: a CS built on it would commit to OVERLOADED on
	// the opening moments of a run at 0.3x capacity. This is also the honest floor on
	// how fast any wrapper of these two statistics can be.
	haveAdmission bool
	admissionCk   anytimeCheckpoint

	// Running prefix sums over samples, plus geometrically spaced checkpoints of
	// them, so the trailing window is a subtraction (O(1)) instead of a rescan.
	sumX     float64
	sumXX    float64
	ring     [anytimeRingCap]anytimeCheckpoint
	ringHead int
	ringLen  int
	nextCkAt int64

	// Arrival-process dispersion, which sets the effective sample size.
	disp dispersionMeter

	// One wrapped Detect per wrapped Observe, cached, so the wrapper's own Detect is
	// a pure O(1) read and composite's O(C log C) Detect is not called twice per
	// event.
	wrappedResult Result

	// The most recent interval and its inputs, for Detect's signals.
	lastCenter      float64
	lastLo          float64
	lastHi          float64
	lastWidth       float64
	lastBoundary    float64
	lastDispersion  float64
	lastNeff        float64
	lastWindowN     float64
	lastWindowStart float64

	// The latched verdict.
	state        anytimeState
	latchedLevel Level
}

// anytimeState is the wrapper's committed verdict, distinct from Level precisely
// so that undecidedness never needs a fourth Level constant.
type anytimeState int

const (
	anytimeUndecided anytimeState = iota
	anytimeFired
	anytimeStableCommitted
)

// anytimeConfig holds the wrapper's resolved, validated parameters.
type anytimeConfig struct {
	// Wrapped is a REGISTRY detector name (so construction goes through
	// buildDetector), already normalized from the YAML spelling.
	Wrapped string
	// Bound selects the confidence-sequence family (boundHowardEB, boundMixtureSPRT).
	Bound string
	// Alpha is the miscoverage budget for the WHOLE sequence, not per look.
	Alpha float64
	// DispersionWindowUs is the bucket width the arrival-process index of dispersion
	// is measured over.
	DispersionWindowUs int64
	// IndeterminatePolicy is indetStrict or indetLeanStable.
	IndeterminatePolicy string
	// Threshold is passed straight through to the wrapped detector's own
	// calibration knob, so the wrapper can be moved onto a matched false-alarm rate.
	// ThresholdSet distinguishes "absent" from "zero" (R9).
	Threshold    float64
	ThresholdSet bool
}

const anytimeName = "anytime"

// The wrappable targets, spelled as the REGISTRY spells them. resolveAnytimeConfig
// normalizes the campaign's peak_rate to wrappedPeakRate, so every target dispatch
// below compares against these constants rather than a repeated literal -- a
// mis-spelled literal would silently take the composite branch for a peak-rate
// wrapper, which is a wrong-target verdict rather than an error.
const (
	wrappedComposite = "composite"
	wrappedPeakRate  = "peak-rate"
)

// Confidence-sequence bound families.
const (
	boundHowardEB    = "howard_eb"
	boundMixtureSPRT = "mixture_sprt"
)

// Undecidedness policies.
const (
	indetStrict     = "strict"
	indetLeanStable = "lean_stable"
)

// Defaults for the anytime block's optional fields. There is deliberately NO
// default for `wrapped`: see resolveAnytimeConfig.
const (
	defaultAnytimeBound              = boundHowardEB
	defaultAnytimeAlpha              = 0.05
	defaultAnytimeDispersionWindowUs = int64(2_000_000)
	defaultAnytimeIndeterminate      = indetStrict
)

// Window geometry and decision constants.
const (
	// anytimeRingCap x anytimeCkGrowth must span more than anytimeWindowRatio, or the
	// oldest retained checkpoint would sit INSIDE the target window and the window
	// would silently shorten. 1.1892^15 = 13.5 > 4.
	anytimeRingCap  = 16
	anytimeCkGrowth = 1.189207115002721 // 4^(1/8)

	// anytimeWindowRatio makes the window TRAILING: it starts at n/ratio, so it
	// covers the most recent ~75% of samples. An anchored (all-samples-since-
	// admission) window is not equivalent: the exponent right after admission is
	// large in every regime, and a cumulative estimator averages that opening
	// transient in forever, which raises the false-alarm rate on healthy traffic.
	anytimeWindowRatio = 4

	// anytimeMinWindow is the smallest window a variance estimate is taken from.
	anytimeMinWindow = 4

	// anytimePeakGammaBoundary is the CRITICAL growth exponent: Peak grows like
	// sqrt(t) at rho = 1 (see PeakRateDetector's docs), so gamma = 1/2 is exactly
	// the boundary between a peak that is still climbing and one that has levelled
	// off. It is a property of reflected random walks, not a tuned number.
	anytimePeakGammaBoundary = 0.5
)

// anytimeUndecidedSignal is the out-of-band carrier for undecidedness (never a
// Level -- see the type docs).
const anytimeUndecidedSignal = "undecided"

// anytimeCheckpoint freezes the running prefix sums at one sample index so a
// trailing window's sums and peak growth are a subtraction.
type anytimeCheckpoint struct {
	n     int64
	sumX  float64
	sumXX float64
	peak  int64
}

// newAnytimeDetector is the canonical constructor (R4). It builds the wrapped
// detector THROUGH THE REGISTRY, handing it a synthesized config that carries only
// its own block, so the wrapped detector is byte-for-byte the one
// `--detectors <wrapped>` would have produced.
func newAnytimeDetector(cfg anytimeConfig) (Detector, error) {
	synth := SaturationConfig{}
	if cfg.ThresholdSet {
		v := cfg.Threshold
		switch cfg.Wrapped {
		case wrappedComposite:
			synth.Composite = &CompositeBlock{Sensitivity: &v}
		case wrappedPeakRate:
			synth.PeakRate = &PeakRateBlock{Threshold: &v}
		default:
			// Unreachable: resolveAnytimeConfig rejects any other name. Reported
			// rather than ignored so a future wrapped detector cannot silently lose
			// its calibration knob (R1).
			return nil, fmt.Errorf("saturation config: anytime.threshold has no calibration knob to map onto for anytime.wrapped=%q", cfg.Wrapped)
		}
	}
	inner, err := buildDetector(cfg.Wrapped, synth)
	if err != nil {
		return nil, err
	}
	a := &AnytimeDetector{cfg: cfg, wrapped: inner}
	a.Reset()
	return a, nil
}

// Name is the registry name. It is NOT the wrapped detector's name: the trace tags
// records by detector name, and reporting the wrapped name would make a wrapped run
// indistinguishable from a bare one.
func (a *AnytimeDetector) Name() string { return anytimeName }

// Observe forwards the event to the wrapped detector, folds it into the wrapper's
// own stream state, extends the confidence sequence, and updates the latch.
//
// The verdict is computed HERE rather than in Detect so that Detect stays a pure
// query: a verdict must depend on the event stream, never on how many times a
// caller asked for it (the property peak-rate's docs call out explicitly).
func (a *AnytimeDetector) Observe(event Event) {
	// Forward FIRST and UNCONDITIONALLY. Whether an event is meaningful is the
	// wrapped detector's judgement, not the wrapper's; filtering here would change
	// the wrapped statistic and break the "same statistic" claim.
	a.wrapped.Observe(event)
	a.wrappedResult = a.wrapped.Detect()

	// Ignore an unrecognized event type for the wrapper's OWN state, exactly as the
	// incumbents do: advancing the span or the sample count for an event that does
	// not change the backlog would shrink the statistic while leaving the interval
	// unchanged, so the verdict would contradict its own reported statistic (R1).
	if event.Type != Arrival && event.Type != Completion {
		a.decide()
		return
	}

	if !a.haveFirst {
		a.firstTsUs, a.lastTsUs = event.Timestamp, event.Timestamp
		a.haveFirst = true
	}
	if event.Timestamp < a.firstTsUs {
		a.firstTsUs = event.Timestamp
	}
	if event.Timestamp > a.lastTsUs {
		a.lastTsUs = event.Timestamp
	}

	switch event.Type {
	case Arrival:
		a.arrivals++
		a.inFlight++
		a.disp.observeArrival(event.Timestamp)
	case Completion:
		a.completions++
		// Guarded, like PeakRateDetector: a completion without its arrival would
		// otherwise drive the backlog negative.
		if a.inFlight > 0 {
			a.inFlight--
		}
	}
	a.n++

	record := 0.0
	if a.inFlight > a.peak {
		a.peak = a.inFlight
		record = 1.0
	}

	if !a.haveAdmission && a.completions > 0 {
		a.haveAdmission = true
		a.admissionCk = anytimeCheckpoint{n: a.n, sumX: a.sumX, sumXX: a.sumXX, peak: a.peak}
		a.nextCkAt = a.n + 1
	}

	if a.haveAdmission && a.n > a.admissionCk.n {
		x := a.sample(record)
		a.sumX += x
		a.sumXX += x * x
		if a.n >= a.nextCkAt {
			a.pushCheckpoint()
		}
	}

	a.decide()
}

// sample is the per-event [0,1] observation the confidence sequence is built on.
// It is the piece that differs per wrapped detector (see the type docs).
func (a *AnytimeDetector) sample(record float64) float64 {
	switch a.cfg.Wrapped {
	case wrappedPeakRate:
		// The record-setting indicator: the exact discrete derivative of the
		// high-water mark, and the only mean-valued statistic peak-rate's ratio
		// admits.
		return record
	default:
		// composite's score is already in [0,1]; clamped defensively so a future
		// scoring change cannot silently push the CS outside its support.
		return anytimeClamp01(a.wrappedResult.Score)
	}
}

// pushCheckpoint appends a prefix-sum snapshot, evicting the oldest when the ring
// is full, and schedules the next one a fixed RATIO further along -- so the ring
// covers a fixed multiplicative span of history with O(1) entries rather than one
// entry per event.
func (a *AnytimeDetector) pushCheckpoint() {
	ck := anytimeCheckpoint{n: a.n, sumX: a.sumX, sumXX: a.sumXX, peak: a.peak}
	if a.ringLen < anytimeRingCap {
		a.ring[(a.ringHead+a.ringLen)%anytimeRingCap] = ck
		a.ringLen++
	} else {
		a.ring[a.ringHead] = ck
		a.ringHead = (a.ringHead + 1) % anytimeRingCap
	}
	next := int64(float64(a.n)*anytimeCkGrowth) + 1
	if next <= a.n {
		next = a.n + 1
	}
	a.nextCkAt = next
}

// windowBase returns the checkpoint the confidence sequence's window starts at.
//
// The base differs by TARGET, because the two targets are different kinds of
// quantity and a window that suits one is wrong for the other:
//
//   - composite's score is a LEVEL. It is roughly stationary within a regime, so a
//     trailing window (the latest snapshot at or before n/anytimeWindowRatio) is
//     what estimates the CURRENT level; anchoring at admission would average the
//     opening transient into the verdict forever.
//   - peak-rate's gamma is a GROWTH EXPONENT over a span, so its denominator is the
//     log-span log(n/n_base). A fixed-RATIO window makes that denominator a CONSTANT
//     (log 4), which is fatal: the record count needed to reach gamma=0.5 is
//     p1*(sqrt(ratio)-1), about 5 records on a healthy queue whose peak is O(1),
//     while the bound's non-vanishing slack is about 7*log(1/alpha)/3 ~ 12 records.
//     Both are O(1), so the slack dominates at EVERY n and the detector can never
//     commit to STABLE no matter how long it watches -- measured: 0 decided records
//     in 1600 events. Anchoring at admission makes the log-span grow like log n, so
//     the record budget grows like sqrt(n) and eventually dwarfs the O(log log n)
//     slack. That is the same horizon-dependence peak-rate itself has by
//     construction, not a tuning choice.
//
// The scan is over at most anytimeRingCap entries, so it is O(1) in trace length.
func (a *AnytimeDetector) windowBase() (anytimeCheckpoint, bool) {
	if !a.haveAdmission {
		return anytimeCheckpoint{}, false
	}
	if a.cfg.Wrapped == wrappedPeakRate {
		return a.admissionCk, true
	}
	target := a.n / anytimeWindowRatio
	best := a.admissionCk
	for i := 0; i < a.ringLen; i++ {
		ck := a.ring[(a.ringHead+i)%anytimeRingCap]
		if ck.n <= target && ck.n > best.n {
			best = ck
		}
	}
	return best, true
}

// decide extends the confidence sequence by one look and updates the latch.
func (a *AnytimeDetector) decide() {
	a.lastDispersion = a.disp.index()

	base, ok := a.windowBase()
	windowN := float64(a.n - base.n)
	if !ok || windowN < anytimeMinWindow {
		a.noInterval()
		return
	}

	sumX := a.sumX - base.sumX
	sumXX := a.sumXX - base.sumXX
	mean := sumX / windowN
	vhat := sumXX/windowN - mean*mean
	if vhat < 0 {
		// Floating-point slack only: the population variance is non-negative by
		// construction. Floored rather than reported because a negative epsilon is
		// not a user-visible mistake.
		vhat = 0
	}

	// The design effect: correlated arrivals carry less information per event than
	// independent ones, so the EFFECTIVE sample size is the raw count divided by the
	// index of dispersion. This is what makes the width scale like
	// sqrt(I * loglog(n) / n) with a MEASURED I -- never a declared CV, which the
	// detector has no way to know and no right to assume.
	//
	// max(1, I) only ever WIDENS. Narrowing below the i.i.d. bound on under-dispersed
	// (near-deterministic) arrivals would be arithmetically tempting and would break
	// coverage, since the bound's martingale argument does not license it.
	neff := windowN / math.Max(1, a.lastDispersion)
	width := anytimeWidth(a.cfg.Bound, vhat, neff, a.cfg.Alpha)

	a.lastWidth = width
	a.lastNeff = neff
	a.lastWindowN = windowN
	a.lastWindowStart = float64(base.n)

	center, lo, hi, boundary, ok := a.mapTarget(mean, mean-width, mean+width, base, windowN)
	if !ok {
		a.noInterval()
		return
	}
	a.lastCenter, a.lastLo, a.lastHi, a.lastBoundary = center, lo, hi, boundary

	// The FIRE rule is a conjunction: the evidence must be decisive AND the wrapped
	// detector must itself be firing. See the type docs for why (an inert
	// calibration knob is not comparable).
	switch {
	case lo > boundary && a.wrappedResult.Level != Stable:
		a.state = anytimeFired
		a.latchedLevel = a.wrappedResult.Level
	case hi < boundary:
		a.state = anytimeStableCommitted
	default:
		// Straddling, or decisive but the wrapped detector's own debounce is not yet
		// satisfied. Hold whatever was committed before; if nothing was, stay
		// undecided.
	}
}

// noInterval records "the interval is the whole support", which is the honest
// report when there is not yet enough evidence to compute one. The latch is left
// alone: absence of evidence never retracts a committed verdict.
func (a *AnytimeDetector) noInterval() {
	a.lastCenter, a.lastLo, a.lastHi, a.lastWidth = 0, 0, 1, 1
	a.lastNeff, a.lastWindowN, a.lastWindowStart = 0, 0, 0
	a.lastBoundary = a.boundaryOnly()
}

// boundaryOnly is the decision boundary alone, for the pre-interval reports.
func (a *AnytimeDetector) boundaryOnly() float64 {
	if a.cfg.Wrapped == wrappedPeakRate {
		return anytimePeakGammaBoundary
	}
	return a.compositeBoundary()
}

// compositeBoundary is composite's own moving decision line: the
// sensitivity-scaled 1/sqrt(arrivals) noise floor.
func (a *AnytimeDetector) compositeBoundary() float64 {
	sensitivity := defaultCompositeSensitivity
	if a.cfg.ThresholdSet {
		sensitivity = a.cfg.Threshold
	}
	if a.arrivals <= 0 {
		return 1
	}
	return math.Min(1, sensitivity/math.Sqrt(float64(a.arrivals)))
}

// mapTarget converts the CS on the per-event sample mean into a CS on the wrapped
// detector's own decision variable, and returns the boundary that variable must
// clear. The transform is monotone in both cases, so interval endpoints map to
// interval endpoints -- no re-derivation of the bound is needed.
func (a *AnytimeDetector) mapTarget(mean, meanLo, meanHi float64, base anytimeCheckpoint, windowN float64) (center, lo, hi, boundary float64, ok bool) {
	if a.cfg.Wrapped != wrappedPeakRate {
		return anytimeClamp01(mean), anytimeClamp01(meanLo), anytimeClamp01(meanHi), a.compositeBoundary(), true
	}

	p1 := float64(base.peak)
	den := math.Log(float64(a.n) / float64(base.n))
	if p1 <= 0 || den <= 0 || base.n <= 0 {
		// Degenerate window (no peak yet, or zero log-span). Reported as "no
		// interval" rather than as gamma=0, which would COMMIT to STABLE on an
		// undefined quantity (R1/R11).
		return 0, 0, 0, 0, false
	}

	// gamma(p) = log(Peak_n / Peak_n1) / log(n / n1), with the numerator rewritten
	// through the exact record-count identity Peak_n - Peak_n1 = windowN * p. Strictly
	// increasing in p, hence endpoint-preserving.
	g := func(p float64) float64 {
		return math.Log(1+windowN*anytimeClamp01(p)/p1) / den
	}

	center = g(mean)
	// A negative growth exponent means the running maximum DECREASED, which the
	// state machine cannot produce (in-flight moves by at most one per event and the
	// peak is a running max). Asserted rather than clamped: clamping would convert a
	// broken accumulator into a plausible-looking healthy verdict, which is exactly
	// the silent failure the assertion exists to prevent.
	if center < 0 {
		panic(fmt.Sprintf("saturation: anytime peak growth exponent is negative (%v): running maximum decreased from %d over %v samples", center, base.peak, windowN))
	}
	return anytimeClamp01(center), anytimeClamp01(g(meanLo)), anytimeClamp01(g(meanHi)), anytimePeakGammaBoundary, true
}

// Detect reports the current verdict. It is a pure query: repeated calls without
// an intervening Observe return the same Result.
func (a *AnytimeDetector) Detect() Result {
	level := Stable
	confidence := 0.0
	undecided := 0.0

	switch a.state {
	case anytimeFired:
		level = a.latchedLevel
		// The CS's own coverage guarantee, which is what "confidence" means here --
		// unlike the streaming detectors' observation ramp, which is a proxy for it.
		confidence = 1 - a.cfg.Alpha
	case anytimeStableCommitted:
		confidence = 1 - a.cfg.Alpha
	default:
		if a.cfg.IndeterminatePolicy == indetStrict {
			undecided = 1
		}
	}

	signals := map[string]float64{
		"anytime_center":       a.lastCenter,
		"anytime_lo":           a.lastLo,
		"anytime_hi":           a.lastHi,
		"anytime_width":        a.lastWidth,
		"anytime_boundary":     a.lastBoundary,
		"alpha":                a.cfg.Alpha,
		"dispersion":           a.lastDispersion,
		"n_eff":                a.lastNeff,
		"window_n":             a.lastWindowN,
		"window_start":         a.lastWindowStart,
		anytimeUndecidedSignal: undecided,
		"observations":         float64(a.n),
		"arrivals":             float64(a.arrivals),
		"completions":          float64(a.completions),
		"in_flight":            float64(a.inFlight),
		"peak_backlog":         float64(a.peak),
		"elapsed_sec":          a.elapsedSec(),
		"wrapped_level":        float64(a.wrappedResult.Level),
		"wrapped_score":        a.wrappedResult.Score,
	}
	// The wrapped detector's own diagnostics, namespaced so they can never collide
	// with the wrapper's and COPIED so the wrapped Result's map is never mutated
	// (it belongs to the wrapped detector; editing it would corrupt the statistic
	// this wrapper claims to preserve).
	for k, v := range a.wrappedResult.Signals {
		signals["w_"+k] = v
	}

	return Result{
		Level:      level,
		Score:      anytimeClamp01(a.lastCenter),
		Confidence: confidence,
		Signals:    signals,
	}
}

// Reset returns the wrapper AND the wrapped detector to their initial state so the
// pair can be reused across replay legs. Configuration survives; only accumulated
// state is cleared.
func (a *AnytimeDetector) Reset() {
	a.wrapped.Reset()
	a.wrappedResult = Result{}

	a.n, a.arrivals, a.completions, a.inFlight, a.peak = 0, 0, 0, 0, 0
	a.firstTsUs, a.lastTsUs, a.haveFirst = 0, 0, false
	a.haveAdmission, a.admissionCk = false, anytimeCheckpoint{}
	a.sumX, a.sumXX = 0, 0
	a.ring = [anytimeRingCap]anytimeCheckpoint{}
	a.ringHead, a.ringLen, a.nextCkAt = 0, 0, 0
	a.disp = dispersionMeter{windowUs: a.cfg.DispersionWindowUs}
	a.state, a.latchedLevel = anytimeUndecided, Stable
	a.noInterval()
}

// elapsedSec is the observed span in seconds.
func (a *AnytimeDetector) elapsedSec() float64 {
	if !a.haveFirst || a.lastTsUs <= a.firstTsUs {
		return 0
	}
	return float64(a.lastTsUs-a.firstTsUs) / 1e6
}

// anytimeWidth is the half-width of the confidence sequence at effective sample
// size neff, for a variable supported on [0,1].
//
// Both families are TIME-UNIFORM: the coverage statement quantifies over all n at
// once, which is what licenses looking after every event. Both are decreasing in
// neff and increasing as alpha shrinks -- the two monotonicities the detector's
// behaviour is claimed on, and the two the tests sweep.
//
// The families differ in a way that matters for the experiment rather than
// cosmetically:
//
//   - howard_eb is a self-normalized empirical-Bernstein bound: it reads the
//     MEASURED variance, so it is far tighter than a worst-case bound exactly where
//     the record-rate statistic lives (p near 0 or 1, where variance is small).
//     Its loglog term is the price of time-uniformity.
//   - mixture_sprt is a normal-mixture (mixture-SPRT) boundary using the worst-case
//     sub-Gaussian proxy for a [0,1] variable (sigma = 1/2). It ignores the measured
//     variance, so it is tighter at small neff and high variance, and looser once the
//     variance turns out to be small.
//
// neff below 1 is clamped rather than special-cased, so the function is monotone on
// its whole domain instead of having a discontinuity a caller could trip over.
func anytimeWidth(bound string, vhat, neff, alpha float64) float64 {
	if neff < 1 {
		neff = 1
	}
	switch bound {
	case boundHowardEB:
		// L is the time-uniform penalty: log(1/alpha) for the coverage level plus a
		// log(1+log(1+n)) term for the union over all n. It grows slower than n, which
		// is why the width still shrinks.
		l := math.Log(1/alpha) + math.Log(1+math.Log(1+neff))
		// Two terms, and the SECOND one is what makes the bound honest. The variance
		// term alone is a plug-in: it reads vhat off the same samples whose mean it
		// bounds, so a run of identical samples drives vhat to 0 and the interval
		// collapses to a point at exactly the moment the estimate deserves it least.
		// That is not hypothetical. With a range coefficient of 1/3 this bound measured
		// 26% miscoverage against a 5% budget on i.i.d. Bernoulli streams
		// (TestAnytime_CoverageHoldsUnderAlpha): at p=0.9 a 15-long run of ones has
		// probability 0.9^15 = 0.21, and left a half-width of 0.096 around a mean 0.1
		// away from the truth. The Maurer-Pontil range coefficient 7/3 is the term that
		// does NOT vanish with vhat, and it is what buys coverage. The n-1 denominator
		// The denominator is neff rather than the neff-1 of the fixed-n Maurer-Pontil
		// statement: with neff-1 floored at 1, the term is 7*l(1)/3 at neff=1 and
		// 7*l(2)/3 at neff=2 -- it GROWS, because l grows while the floored denominator
		// does not, and that breaks the width-shrinks-with-evidence property this
		// detector's behaviour is claimed on (TestAnytime_WidthShrinksWithObservations
		// caught it at alpha=0.001). l(n)/n is monotone decreasing for n >= 1 because
		// l' < 1/(n+1) < l/n, so neff restores monotonicity while staying conservative
		// (it differs from neff-1 by one sample, and the term is already a 7x margin).
		return math.Sqrt(2*vhat*l/neff) + 7*l/(3*neff)
	case boundMixtureSPRT:
		const rho = 1.0
		const sigma = 0.5 // sup sd of a [0,1] variable
		l := math.Log(math.Sqrt(neff*rho+1) / alpha)
		if l <= 0 {
			// Unreachable for alpha < 1 and neff >= 1; kept so a future alpha bound
			// cannot turn a negative logarithm into a NaN width (R11).
			return 1
		}
		return sigma * math.Sqrt(2*(neff*rho+1)/(neff*neff*rho)*l)
	default:
		// Unreachable through the config path (resolveAnytimeConfig rejects any other
		// name). Reachable only by constructing anytimeConfig in-process, which is a
		// programming error, so it panics rather than silently picking a family and
		// reporting a bound the caller did not ask for.
		panic(fmt.Sprintf("saturation: unknown anytime bound %q", bound))
	}
}

// anytimeClamp01 confines a value to the [0,1] support. It clamps the SUPPORT of an
// interval endpoint, never a point estimate that has gone out of range -- those are
// asserted (see mapTarget).
func anytimeClamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// dispersionMeter measures the arrival process's index of dispersion
//
//	I = Var(N_T) / E[N_T]
//
// over fixed-width buckets of T microseconds: 1 for Poisson, below 1 for a
// near-deterministic process, well above 1 for a bursty one. It is the wrapper's
// only measurement of HOW the traffic arrives, and it is measured rather than
// declared -- a coefficient of variation supplied by a workload spec is not
// available to a detector and would be a different quantity anyway.
//
// State is three scalars plus the open bucket, so it is O(1) in trace length.
type dispersionMeter struct {
	windowUs   int64
	originUs   int64
	haveOrigin bool
	curIdx     int64
	curCount   float64
	nb         float64 // closed buckets
	sum        float64
	sumSq      float64
}

// observeArrival folds one arrival into its bucket, closing every bucket it skips
// past. Skipped buckets are counted with a count of ZERO because an empty interval
// is a real observation of the counting process -- dropping them would understate
// the variance of exactly the bursty traffic the meter exists to detect.
func (d *dispersionMeter) observeArrival(ts int64) {
	if d.windowUs <= 0 {
		return
	}
	if !d.haveOrigin {
		d.originUs, d.haveOrigin, d.curIdx, d.curCount = ts, true, 0, 0
	}
	idx := (ts - d.originUs) / d.windowUs
	// An out-of-order arrival cannot re-open a closed bucket; it is folded into the
	// current one instead, so the closed history stays append-only.
	idx = max(idx, d.curIdx)
	if idx > d.curIdx {
		d.nb++
		d.sum += d.curCount
		d.sumSq += d.curCount * d.curCount
		// The intervening empty buckets, added arithmetically rather than in a loop so
		// a long idle gap stays O(1) instead of iterating once per empty window.
		if empty := idx - d.curIdx - 1; empty > 0 {
			d.nb += float64(empty)
		}
		d.curIdx, d.curCount = idx, 0
	}
	d.curCount++
}

// index returns the measured index of dispersion, or the NEUTRAL value 1 while
// there is not enough evidence.
//
// The OPEN bucket is excluded: it is partial by definition, and including it would
// pull the variance down on every look. Neutral-until-known means an early verdict
// is neither widened nor narrowed by an unmeasured quantity.
func (d *dispersionMeter) index() float64 {
	if d.nb < 2 {
		return 1
	}
	mean := d.sum / d.nb
	if mean <= 0 {
		return 1
	}
	v := (d.sumSq - d.nb*mean*mean) / (d.nb - 1)
	if v < 0 {
		v = 0
	}
	i := v / mean
	if math.IsNaN(i) || math.IsInf(i, 0) {
		return 1
	}
	return i
}

// ---------------------------------------------------------------------------
// Pre-registered rung scoring rules.
//
// These implement the campaign's declared rules in Go so they are testable and so
// the definition of "this rung fired" lives with the mechanism rather than only in
// the scoring harness. They are deliberately GENERAL over Detector output: a
// detector with no "undecided" signal has every record decided, so the same rules
// reduce to the incumbents' plain fraction.
// ---------------------------------------------------------------------------

// AnytimeRecordDecided reports whether one verdict is a committed one. A Result
// with no undecided signal (every incumbent detector) is decided.
func AnytimeRecordDecided(r Result) bool {
	return r.Signals[anytimeUndecidedSignal] < 1
}

// AnytimeRecordFired reports whether one verdict is a firing one. Both non-STABLE
// levels count: the rung question is "did it call saturation", not "which flavour".
func AnytimeRecordFired(r Result) bool { return r.Level != Stable }

// AnytimeFiredFraction is the fraction of DECIDED records that fired, plus the
// decided count. Undecided records are excluded from the DENOMINATOR, not counted
// as STABLE: "the evidence has not spoken yet" is not evidence of health, and
// charging it as such would make a detector that commits later look like one that
// disagrees.
func AnytimeFiredFraction(records []TraceRecord) (float64, int) {
	decided, fired := 0, 0
	for _, rec := range records {
		if !AnytimeRecordDecided(rec.Result) {
			continue
		}
		decided++
		if AnytimeRecordFired(rec.Result) {
			fired++
		}
	}
	if decided == 0 {
		return 0, 0
	}
	return float64(fired) / float64(decided), decided
}

// AnytimeRungFired applies the pre-registered rung rule: a rung fired if at least
// firedThreshold of its DECIDED records fired, provided at least minDecided records
// were decided at all.
//
// The minimum is load-bearing. Without it a single decided record that happened to
// fire would carry a rung at a fraction of 1.0, so a detector that almost never
// commits would score as the most decisive one -- the exact failure the undecided
// exclusion would otherwise introduce.
func AnytimeRungFired(records []TraceRecord, firedThreshold float64, minDecided int) bool {
	frac, decided := AnytimeFiredFraction(records)
	if decided < minDecided {
		return false
	}
	return frac >= firedThreshold
}

// AnytimeFlipCount counts fired -> not-fired transitions across DECIDED records
// only.
//
// fired -> undecided -> fired is therefore NOT a flip: the detector never retracted
// anything, it declined to speak. Counting that as instability would penalize
// precisely the honesty the undecided verdict exists to express. fired -> STABLE ->
// fired IS a flip, because there the detector did commit to the opposite verdict.
func AnytimeFlipCount(records []TraceRecord) int {
	flips := 0
	havePrev := false
	prevFired := false
	for _, rec := range records {
		if !AnytimeRecordDecided(rec.Result) {
			continue
		}
		fired := AnytimeRecordFired(rec.Result)
		if havePrev && prevFired && !fired {
			flips++
		}
		prevFired, havePrev = fired, true
	}
	return flips
}
