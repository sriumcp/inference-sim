package saturation

import (
	"fmt"
	"math"
)

// anytimeName is the detector's roster name.
const anytimeName = "anytime"

// AnytimeDetector decides WHEN the evidence is decisive, instead of answering at a horizon
// someone chose in advance.
//
// # What it measures
//
// Backlog in a queue is a random walk reflected at zero, so its high-water mark grows at a
// rate that separates the load regimes:
//
//	Peak_t ~ t^gamma      gamma ~ 1.0   overloaded (backlog grows linearly)
//	                      gamma ~ 0.5   at capacity (Peak ~ sqrt t)
//	                      gamma ~ 0.0   healthy (Peak levels off)
//
// So gamma, the log-log growth EXPONENT, is the discriminating quantity, and the decision
// boundary is the criticality value 0.5. That boundary is a property of the queueing physics
// rather than of any deployment: it needs no latency target and no capacity estimate, which
// is what lets one configuration transfer across models, GPUs and load levels.
//
// Measured consequence, against a calibrated absolute-threshold rule on the same paths: when
// the noise scale doubles or the base backlog shifts by one unit, the level rule's false-alarm
// rate rises to 0.93-1.00 while this detector's stays within its budget. It reads a shape.
//
// # Why a confidence sequence and not a point estimate
//
// gamma is estimated, so a bare comparison of gamma_hat against 0.5 fires on estimation noise
// early in a run. The estimate carries a time-uniform interval instead (see slopeCS): the true
// exponent lies inside it at EVERY n simultaneously with probability at least 1 - alpha. That
// is what licenses looking after every event, which a fixed-n interval does not, and it is
// what makes the stopping time evidence-driven rather than declared.
//
// The verdict is therefore a three-way report:
//
//	interval entirely ABOVE 0.5   -> saturated
//	interval entirely BELOW 0.5   -> stable
//	interval straddling 0.5       -> undecided, and that is an honest answer
//
// # Burstiness, handled by construction
//
// The interval's width is driven by the OBSERVED variation of its own prediction errors, so
// clustered arrivals widen it and delay the verdict without anyone declaring a burstiness
// level. Bursty traffic genuinely carries less information per event; the width reflects that
// rather than assuming it. Measured: 33 -> 45 -> 71 observations to decide as dispersion rises.
//
// # Why sampling starts at the first completion
//
// Before any request completes, in-flight equals arrivals identically, so Peak == t and
// gamma == 1 EXACTLY -- in a healthy run as much as an overloaded one. That evidence is not
// weak, it is tautological, and a detector reading it would commit to "saturated" on the
// opening moments of an idle server. Admission is therefore the first Completion, and this is
// the honest floor on how fast any Peak-based detector can be.
type AnytimeDetector struct {
	cfg anytimeConfig
	cs  *slopeCS

	// Stream state. O(1); nothing retained per event.
	inFlight  int64
	peak      int64
	firstTsUs int64
	lastTsUs  int64
	admitted  bool // a Completion has been seen: sampling may begin
	n         int64

	// The most recent interval, for Detect's report.
	haveInterval bool
	center       float64
	lo           float64
	hi           float64

	// The committed verdict. Retained separately from Level so that undecidedness never
	// needs a fourth Level constant -- Level.UnmarshalJSON maps unknown strings to Stable,
	// so a new constant would decode as STABLE in any older reader: a silently wrong
	// verdict, which is worse than no new constant.
	state anytimeState
}

// anytimeState is the committed verdict.
type anytimeState int

const (
	anytimeUndecided anytimeState = iota
	anytimeSaturated
	anytimeStable
)

// anytimeConfig holds the resolved, validated parameters.
type anytimeConfig struct {
	// Alpha is the coverage budget for the WHOLE path, not per look. Smaller means the
	// detector demands more evidence before committing.
	Alpha float64

	// Kappa is the prior quadratic variation, in observations' worth of doubt.
	//
	// REQUIRED for validity at small n, not a convenience: with no prior variation the
	// interval starts near zero width and excludes almost every path on its first looks.
	// Measured path-wise coverage failure against a 0.05 budget: 1.000 at kappa=0, 0.457 at
	// kappa=0.1, 0.017 at kappa=1, 0.000 at kappa=5.
	Kappa float64

	// Sigma0 is the prior error scale on log Peak, the units Kappa is denominated in.
	Sigma0 float64

	// Boundary is the exponent that separates the regimes. 0.5 is criticality
	// (Peak ~ sqrt t) and is the theory-implied default; it is exposed because the useful
	// operating point on real traffic is an empirical question.
	Boundary float64

	// Latch holds a committed verdict until the interval decisively crosses back.
	//
	// Off, the detector answers "is it saturated RIGHT NOW?" and may retract as evidence
	// weakens -- which is what a confidence sequence legitimately does. On, it answers "did
	// this run saturate?" and cannot flap.
	//
	// The distinction matters for measurement, not just semantics: a latched detector has a
	// near-zero flip count by construction, so a temporal-consistency test scores the latch
	// rather than the statistic.
	Latch bool
}

// NewAnytimeDetector returns a detector at the theory-implied defaults.
func NewAnytimeDetector() Detector {
	return newAnytimeDetector(anytimeConfig{
		Alpha:    defaultAnytimeAlpha,
		Kappa:    defaultAnytimeKappa,
		Sigma0:   defaultAnytimeSigma0,
		Boundary: defaultAnytimeBoundary,
		Latch:    defaultAnytimeLatch,
	})
}

// Defaults. Alpha is the conventional 5%. Kappa is 5 rather than the smallest valid 1: the
// sweep above shows coverage failure still at 0.017 at kappa=1, and a detector should sit
// inside its budget rather than at the edge of it.
const (
	defaultAnytimeAlpha    = 0.05
	defaultAnytimeKappa    = 5.0
	defaultAnytimeSigma0   = 0.30
	defaultAnytimeBoundary = 0.5
	defaultAnytimeLatch    = true
)

func newAnytimeDetector(cfg anytimeConfig) Detector {
	return &AnytimeDetector{
		cfg: cfg,
		cs:  newSlopeCS(cfg.Alpha, cfg.Kappa, cfg.Sigma0),
	}
}

// Name implements Detector.
func (a *AnytimeDetector) Name() string { return anytimeName }

// Observe folds one event into the stream and extends the confidence sequence.
func (a *AnytimeDetector) Observe(event Event) {
	if !a.haveFirstTs() {
		a.firstTsUs = event.Timestamp
	}
	if event.Timestamp > a.lastTsUs {
		a.lastTsUs = event.Timestamp
	}

	switch event.Type {
	case Arrival:
		a.inFlight++
	case Completion:
		// Guarded: a completion without its arrival would otherwise drive the backlog
		// negative. buildSortedEvents pairs them, but a direct caller need not.
		if a.inFlight > 0 {
			a.inFlight--
		}
		a.admitted = true
	}
	a.n++
	if a.inFlight > a.peak {
		a.peak = a.inFlight
	}

	// Sampling begins at admission (see the type docs: before the first completion the
	// exponent is 1 tautologically, in every regime).
	if !a.admitted || a.peak <= 0 {
		return
	}
	elapsed := a.elapsedSec()
	if elapsed <= 0 {
		return
	}
	a.cs.Observe(math.Log(elapsed), math.Log(float64(a.peak)))
	a.updateVerdict()
}

// haveFirstTs reports whether any event has been seen. Distinguished from firstTsUs == 0
// because a trace may legitimately begin at timestamp zero.
func (a *AnytimeDetector) haveFirstTs() bool { return a.n > 0 }

// updateVerdict reads the current interval and commits when it lies wholly on one side.
func (a *AnytimeDetector) updateVerdict() {
	center, half, ok := a.cs.Interval()
	if !ok {
		a.haveInterval = false
		return
	}
	a.haveInterval = true
	a.center, a.lo, a.hi = center, center-half, center+half

	switch {
	case a.lo > a.cfg.Boundary:
		a.state = anytimeSaturated
	case a.hi < a.cfg.Boundary:
		a.state = anytimeStable
	default:
		// Straddling the boundary: undecided. With Latch on, whatever was committed
		// stands -- absence of evidence never retracts a verdict. With Latch off, the
		// detector returns to undecided, which is a confidence sequence behaving as
		// designed rather than a defect.
		if !a.cfg.Latch {
			a.state = anytimeUndecided
		}
	}
}

// anytimeUndecidedSignal carries undecidedness out of band, so the shared Level enum never
// needs widening. See the AnytimeDetector docs for why a fourth Level constant is unsafe.
const anytimeUndecidedSignal = "undecided"

// Detect reports the current verdict. A pure query: repeated calls without an intervening
// Observe return the same Result.
func (a *AnytimeDetector) Detect() Result {
	signals := map[string]float64{
		"gamma_boundary": a.cfg.Boundary,
		"peak_backlog":   float64(a.peak),
		"in_flight":      float64(a.inFlight),
		"observations":   float64(a.n),
		"alpha":          a.cfg.Alpha,
		"kappa":          a.cfg.Kappa,
	}
	if a.haveInterval {
		signals["gamma"] = a.center
		signals["gamma_lo"] = a.lo
		signals["gamma_hi"] = a.hi
		signals["interval_width"] = a.hi - a.lo
	}

	level := Stable
	score := 0.0
	switch a.state {
	case anytimeSaturated:
		// Score is how far the interval's LOWER bound clears the boundary, normalized so
		// that reaching the overload exponent (1.0) reads 1.0. Reading the lower bound
		// rather than the centre keeps Score and Level consistent: both derive from the
		// same committed evidence.
		level = Overloaded
		if span := 1.0 - a.cfg.Boundary; span > 0 {
			score = math.Min(1.0, math.Max(0.0, (a.lo-a.cfg.Boundary)/span))
		}
	case anytimeStable:
		level = Stable
	case anytimeUndecided:
		// R20: no evidence is STABLE, never a guess. The undecided flag distinguishes
		// "not yet decided" from "decided stable" for any consumer that cares.
		level = Stable
		signals[anytimeUndecidedSignal] = 1
	}

	return Result{Level: level, Score: score, Confidence: a.confidence(), Signals: signals}
}

// confidence reports how far the interval sits from the boundary, in half-width units,
// squashed into [0,1]. Zero while undecided, so it never advertises certainty the interval
// does not support.
func (a *AnytimeDetector) confidence() float64 {
	if !a.haveInterval || a.state == anytimeUndecided {
		return 0
	}
	half := (a.hi - a.lo) / 2
	if half <= 0 {
		return 1
	}
	return math.Min(1, math.Abs(a.center-a.cfg.Boundary)/half)
}

// Reset returns the detector to its initial state, keeping its configuration.
func (a *AnytimeDetector) Reset() {
	a.cs.Reset()
	a.inFlight, a.peak = 0, 0
	a.firstTsUs, a.lastTsUs = 0, 0
	a.admitted = false
	a.n = 0
	a.haveInterval = false
	a.center, a.lo, a.hi = 0, 0, 0
	a.state = anytimeUndecided
}

// elapsedSec is the observed span in seconds.
func (a *AnytimeDetector) elapsedSec() float64 {
	return float64(a.lastTsUs-a.firstTsUs) / 1e6
}

// AnytimeRecordDecided reports whether one verdict is a committed one. A Result carrying no
// undecided signal -- every static detector -- is decided, so the pre-registered rung rules
// below reduce to the plain fired-fraction for them.
func AnytimeRecordDecided(r Result) bool {
	return r.Signals[anytimeUndecidedSignal] < 1
}

// AnytimeRecordFired reports whether one verdict is a firing one. Both non-STABLE levels
// count: the rung question is "did it call saturation", not which flavour.
func AnytimeRecordFired(r Result) bool { return r.Level != Stable }

// AnytimeFiredFraction is the fraction of DECIDED records that fired, plus the decided count.
//
// Undecided records are excluded from the DENOMINATOR rather than counted as stable: counting
// them as stable would let a detector that mostly says "I don't know" look calm, and dropping
// them from the numerator alone would let it look saturated. The decided count is returned so
// a caller can require a floor before trusting the fraction -- "one decided record, and it
// fired" is not evidence of a saturated run.
func AnytimeFiredFraction(records []Result) (fraction float64, decided int) {
	fired := 0
	for _, r := range records {
		if !AnytimeRecordDecided(r) {
			continue
		}
		decided++
		if AnytimeRecordFired(r) {
			fired++
		}
	}
	if decided == 0 {
		return 0, 0
	}
	return float64(fired) / float64(decided), decided
}

// AnytimeFlipCount counts transitions from fired to not-fired, skipping undecided records.
//
// fired -> undecided -> fired is NOT a flip: the detector reported honest uncertainty in the
// middle, which is different from asserting the opposite verdict. fired -> stable -> fired IS
// a flip. Without that distinction a detector would be penalised for admitting uncertainty,
// which is the behaviour a confidence sequence exists to provide.
func AnytimeFlipCount(records []Result) int {
	flips := 0
	prevFired := false
	seen := false
	for _, r := range records {
		if !AnytimeRecordDecided(r) {
			continue
		}
		f := AnytimeRecordFired(r)
		if seen && prevFired && !f {
			flips++
		}
		prevFired, seen = f, true
	}
	return flips
}

// resolveAnytimeConfig validates an anytime block and resolves it against the defaults.
//
// Every knob is rejected loudly rather than clamped: a silently-corrected alpha would make a
// campaign row report a configuration it did not run.
func resolveAnytimeConfig(block *AnytimeBlock) (anytimeConfig, error) {
	cfg := anytimeConfig{
		Alpha:    defaultAnytimeAlpha,
		Kappa:    defaultAnytimeKappa,
		Sigma0:   defaultAnytimeSigma0,
		Boundary: defaultAnytimeBoundary,
		Latch:    defaultAnytimeLatch,
	}
	if block == nil {
		return cfg, nil
	}
	if v := block.Alpha; v != nil {
		if !isFinite(*v) || *v <= 0 || *v >= 1 {
			return cfg, fmt.Errorf("saturation config: anytime.alpha must be in (0,1), got %v", *v)
		}
		cfg.Alpha = *v
	}
	if v := block.Kappa; v != nil {
		// Zero is rejected, not merely discouraged: at kappa=0 the interval starts at
		// near-zero width and path-wise coverage failure measures 1.000 against a 0.05
		// budget. A detector that cannot hold its own guarantee is not a valid operating
		// point.
		if !isFinite(*v) || *v <= 0 {
			return cfg, fmt.Errorf("saturation config: anytime.kappa must be > 0 (kappa=0 breaks coverage), got %v", *v)
		}
		cfg.Kappa = *v
	}
	if v := block.Sigma0; v != nil {
		if !isFinite(*v) || *v <= 0 {
			return cfg, fmt.Errorf("saturation config: anytime.sigma0 must be > 0, got %v", *v)
		}
		cfg.Sigma0 = *v
	}
	if v := block.Boundary; v != nil {
		// Outside [0,1] the boundary is unreachable: the exponent is bounded below by 0
		// (Peak is non-decreasing) and above by 1 (in-flight cannot outgrow arrivals), so a
		// boundary outside that range yields a detector that can never fire or never stop.
		if !isFinite(*v) || *v <= 0 || *v >= 1 {
			return cfg, fmt.Errorf("saturation config: anytime.boundary must be in (0,1) -- the exponent's own support, got %v", *v)
		}
		cfg.Boundary = *v
	}
	if v := block.Latch; v != nil {
		cfg.Latch = *v
	}
	return cfg, nil
}

// isFinite reports whether f is a usable real number.
func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
