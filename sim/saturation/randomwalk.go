// sim/saturation/randomwalk.go
//
// A reflected-random-walk saturation detector whose STATISTIC is a declared
// factor, so an optimization campaign searches the family instead of a human
// picking one member.
//
// THE FRAMING. Backlog in a single-server queue is a random walk reflected at
// zero (Lindley: W <- max(0, W + service - interarrival)). The reflection is not
// incidental — it is what makes the regimes distinguishable, and it licenses
// statistics that need NO capacity estimate at all:
//
//	R_t = Peak_t / t   (running max of backlog over elapsed time)
//
//	  positive drift  =>  R_t -> (p_up - p_down), a positive CONSTANT
//	  zero drift      =>  R_t -> 0  as 1/sqrt(t)   (Peak grows as sqrt(t))
//	  negative drift  =>  R_t -> 0  as 1/t         (Peak is a finite r.v. M_inf)
//
// Verified by simulation over 10^2..10^5 steps: positive drift converged to
// 0.1016 against a predicted 0.10; zero drift decayed ~3.2x per decade
// (1/sqrt(t)); negative drift ~10x per decade (1/t). The DECAY RATE, not the
// limit, is the discriminator — at t=10^5 the zero-drift and negative-drift R_t
// differed by 23x while a linear slope fit on both was ~0.
//
// WHY THIS MATTERS. A slope-of-backlog detector is degenerate at exactly the
// interesting boundary: at rho ~= 1 backlog grows like sqrt(t), so a linear fit
// tends to ZERO and the detector reports STABLE at criticality — the worst
// failure direction. R_t separates that case without estimating drift.
//
// EXCURSIONS are a second, independent family. An excursion is a departure from
// zero and the return to it; peak M and duration T scale as T ~ M^2 under zero
// drift (diffusive) but T ~ M under negative drift (forced return). Verified:
// at zero drift dur/peak^2 was FLAT at ~2.2 across every peak bin from [1,2) to
// [32,64) while dur/peak doubled per bin; at negative drift dur/peak flattened
// and dur/peak^2 FELL, with excursions dying out by peak 16. Excursion COUNT is
// itself strongly discriminating: 8,150 at zero drift vs 599,260 at negative
// drift over 200k steps.
//
// None of these statistics needs a capacity estimate, so none is exposed to the
// identifiability trilemma that blocks the §2e work-residual
// (campaign/findings/ESTIMATOR-IMPOSSIBILITY.md).
package saturation

import "math"

// rwStatistic selects which reflected-walk statistic drives the verdict. This is
// the campaign's primary categorical factor.
type rwStatistic string

const (
	// statPeakOverElapsed is R_t = Peak_t / t.
	statPeakOverElapsed rwStatistic = "peak_over_elapsed"
	// statPeakDecayRate is Peak_t / sqrt(t): asymptotically CONSTANT at zero
	// drift, GROWING under positive drift, DECAYING under negative drift. Targets
	// the decay rate directly, which simulation showed is the real discriminator.
	statPeakDecayRate rwStatistic = "peak_decay_rate"
	// statExcursionRate is completed excursions per unit time (few => the walk
	// left zero and did not return => positive drift).
	statExcursionRate rwStatistic = "excursion_rate"
	// statExcursionScaling is the fitted exponent b in T ~ M^b: ~2
	// diffusive/critical, ~1 forced-return/healthy.
	statExcursionScaling rwStatistic = "excursion_scaling"
	// statIdleFraction is reflecting-boundary occupancy — the fraction of elapsed
	// time the walk sat AT zero, ~ (1 - rho).
	statIdleFraction rwStatistic = "idle_fraction"
	// statPeakRatioStability is R_t's STABILITY across two horizons, and it is the
	// statistic the random-walk result actually implies.
	//
	// All three regimes make R_t = Peak_t/t large EARLY and then decay or flatten:
	// positive drift converges to the constant (p_up - p_down); zero drift decays
	// as 1/sqrt(t); negative drift as 1/t. So the LEVEL of R_t conflates "saturated"
	// with "early in the run" -- at t=100 the three regimes span 0.031..0.139 and
	// OVERLAP. Only the DECAY RATE separates them, and a rate needs two time points.
	//
	// This statistic is R_t(now) / R_t(half-horizon ago): ~1 means R_t is holding
	// (positive drift, peak growing with t => saturated); << 1 means R_t is decaying
	// (peak has stopped growing => healthy). Measured on the real apparatus at fixed
	// rate, comparing n=300 vs n=1200 runs: 0.3x nominal decayed 3.4x (0.594 ->
	// 0.174) while 2.0x nominal held flat (11.56 -> 10.72, ratio 0.93).
	statPeakRatioStability rwStatistic = "peak_ratio_stability"
)

// rwBacklogSource selects WHICH quantity is treated as the reflected walk.
type rwBacklogSource string

const (
	srcInFlight    rwBacklogSource = "in_flight"    // arrivals - completions
	srcWorkBacklog rwBacklogSource = "work_backlog" // unserved kappa*I + O
)

type randomWalkConfig struct {
	Statistic rwStatistic
	Source    rwBacklogSource
	// Threshold is the FPR-calibration knob. Its units depend on Statistic, but
	// in every case a LARGER value fires less often.
	Threshold float64
	// WarmupUs discards an initial transient before the statistic is read: R_t is
	// largest early by construction (small t), so this knob is load-bearing.
	WarmupUs        int64
	MinObservations int
	ConsecutiveK    int // consecutive breaches before firing (the T4 lever)
	// Kappa is the prefill:decode cost ratio, used only by srcWorkBacklog. A
	// DECLARED constant, never fitted — see ESTIMATOR-IMPOSSIBILITY.md.
	Kappa    float64
	BacklogK float64 // multiplies Threshold for the OVERLOADED band
	// HorizonRatio is the spacing between the two TIME horizons that
	// peak_ratio_stability compares: the reference sample is retaken whenever
	// elapsed passes HorizonRatio x the recorded reference elapsed. 2.0 compares
	// "now" against "half the run ago"; a larger ratio compares against a more
	// distant past (more decay signal, slower to respond).
	//
	// It is a declared FACTOR rather than a constant because the theory is
	// asymptotic (t -> infinity) while every measurement is a finite run, and this
	// parameter lives exactly in that gap -- see
	// campaign/findings/PEAK-STATISTIC-DIAGNOSIS.md.
	HorizonRatio float64
}

type excursion struct {
	peak       float64
	durationUs int64
}

// RandomWalkDetector implements Detector over a reflected-walk statistic.
type RandomWalkDetector struct {
	cfg randomWalkConfig

	inFlight     int64
	workBacklog  float64
	peak         float64
	firstTsUs    int64
	lastTsUs     int64
	haveFirst    bool
	observations int64

	idleUs      int64
	isAtZero    bool
	curExcPeak  float64
	curExcStart int64
	inExcursion bool
	excursions  []excursion
	pendingWork map[string]float64

	consecutive int

	// peakAtHalfTime is Peak_t sampled at the midpoint of ELAPSED TIME, with the
	// elapsed value it was taken at. The comparison must be anchored to two TIME
	// horizons, not to two event-index positions: at high load arrivals cluster
	// early while completions trail, so the index midpoint is far from the time
	// midpoint and the ratio ends up measuring event ordering rather than decay.
	// Measured with an index midpoint: at 2.0x nominal the peak correctly grew
	// 1.21x, but elapsed grew 3.08x (vs 2.01x at 0.3x nominal), and the elapsed
	// term dominated -- inverting the verdict.
	peakAtHalf    float64
	elapsedAtHalf float64
	halfHorizonUs int64
}

// NewRandomWalkDetector builds the detector from a validated config.
func NewRandomWalkDetector(cfg randomWalkConfig) Detector { return newRandomWalk(cfg) }

func newRandomWalkForTest(cfg randomWalkConfig) *RandomWalkDetector { return newRandomWalk(cfg) }

func newRandomWalk(cfg randomWalkConfig) *RandomWalkDetector {
	if cfg.Statistic == "" {
		cfg.Statistic = statPeakOverElapsed
	}
	if cfg.Source == "" {
		cfg.Source = srcInFlight
	}
	if cfg.ConsecutiveK <= 0 {
		cfg.ConsecutiveK = 3
	}
	if cfg.MinObservations <= 0 {
		cfg.MinObservations = 20
	}
	if cfg.BacklogK <= 0 {
		cfg.BacklogK = 3.0
	}
	if cfg.Kappa < 0 {
		cfg.Kappa = 0
	}
	if cfg.HorizonRatio <= 1.0 {
		cfg.HorizonRatio = 2.0
	}
	return &RandomWalkDetector{cfg: cfg, pendingWork: make(map[string]float64), isAtZero: true}
}

func (r *RandomWalkDetector) Name() string { return "randomwalk" }

func (r *RandomWalkDetector) level() float64 {
	if r.cfg.Source == srcWorkBacklog {
		return r.workBacklog
	}
	return float64(r.inFlight)
}

// Observe folds one event into the walk, maintaining the running peak, the
// reflecting-boundary occupancy, and the completed-excursion list.
func (r *RandomWalkDetector) Observe(event Event) {
	ts := event.Timestamp
	if !r.haveFirst {
		r.firstTsUs, r.lastTsUs = ts, ts
		r.haveFirst = true
	}
	// Accrue time-at-zero for the interval that just elapsed.
	if ts > r.lastTsUs && r.isAtZero {
		r.idleUs += ts - r.lastTsUs
	}
	if ts > r.lastTsUs {
		r.lastTsUs = ts
	}
	r.observations++

	switch event.Type {
	case Arrival:
		r.inFlight++
		w := r.cfg.Kappa*float64(event.InputTokens) + float64(event.OutputTokens)
		r.pendingWork[event.RequestID] = w
		r.workBacklog += w
	case Completion:
		if r.inFlight > 0 {
			r.inFlight--
		}
		if w, ok := r.pendingWork[event.RequestID]; ok {
			r.workBacklog -= w
			delete(r.pendingWork, event.RequestID)
			if r.workBacklog < 0 {
				r.workBacklog = 0
			}
		}
	default:
		return
	}

	lvl := r.level()
	if lvl > r.peak {
		r.peak = lvl
	}

	// Snapshot Peak at the midpoint of elapsed TIME. The horizon doubles as the run
	// proceeds: whenever elapsed passes 2x the recorded half-horizon, the current
	// sample becomes the new "half" reference. That keeps one sample at
	// approximately half the elapsed time with O(1) state and no history buffer.
	elapsedUs := r.elapsedUs()
	if elapsedUs > 0 {
		if r.halfHorizonUs == 0 || float64(elapsedUs) >= r.cfg.HorizonRatio*float64(r.halfHorizonUs) {
			r.peakAtHalf = r.peak
			r.elapsedAtHalf = float64(elapsedUs) / 1e6
			r.halfHorizonUs = elapsedUs
		}
	}

	// Excursion + reflecting-boundary state machine.
	if lvl > 0 {
		if !r.inExcursion {
			r.inExcursion, r.curExcStart, r.curExcPeak = true, ts, lvl
		} else if lvl > r.curExcPeak {
			r.curExcPeak = lvl
		}
		r.isAtZero = false
	} else {
		if r.inExcursion {
			r.excursions = append(r.excursions, excursion{peak: r.curExcPeak, durationUs: ts - r.curExcStart})
			if len(r.excursions) > 4096 {
				r.excursions = r.excursions[1:]
			}
			r.inExcursion, r.curExcPeak = false, 0
		}
		r.isAtZero = true
	}
}

// elapsedUs is time since the first observation, net of the declared warm-up.
func (r *RandomWalkDetector) elapsedUs() int64 {
	if !r.haveFirst {
		return 0
	}
	if e := r.lastTsUs - r.firstTsUs - r.cfg.WarmupUs; e > 0 {
		return e
	}
	return 0
}

// statistic computes the configured statistic. LARGER always means "more
// saturated", so one comparison direction serves every family member.
func (r *RandomWalkDetector) statistic() float64 {
	elapsedSec := float64(r.elapsedUs()) / 1e6
	switch r.cfg.Statistic {
	case statPeakOverElapsed:
		if elapsedSec <= 0 {
			return 0
		}
		return r.peak / elapsedSec
	case statPeakDecayRate:
		if elapsedSec <= 0 {
			return 0
		}
		return r.peak / math.Sqrt(elapsedSec)
	case statExcursionRate:
		// Inverted: FEW excursions per second => walk stayed away from zero.
		if elapsedSec <= 0 {
			return 0
		}
		return 1.0 / (float64(len(r.excursions))/elapsedSec + 1e-9)
	case statExcursionScaling:
		return excursionExponent(r.excursions)
	case statIdleFraction:
		// Inverted: a walk that never sits at zero is not draining.
		total := float64(r.lastTsUs - r.firstTsUs)
		if total <= 0 {
			return 0
		}
		return 1.0 - float64(r.idleUs)/total

	case statPeakRatioStability:
		// Peak GROWTH per unit of TIME growth, both measured between the recorded
		// half-time horizon and now:
		//
		//	(Peak_now / Peak_half) / (t_now / t_half)
		//
		// Positive drift keeps Peak growing roughly linearly in t, so the ratio
		// approaches 1; a drained system's Peak stops growing while t keeps
		// running, driving it toward 0. Larger = more saturated, matching the
		// family's shared direction.
		if r.peakAtHalf <= 0 || r.elapsedAtHalf <= 0 {
			return 0
		}
		now := float64(r.elapsedUs()) / 1e6
		if now <= r.elapsedAtHalf {
			return 0
		}
		peakGrowth := r.peak / r.peakAtHalf
		timeGrowth := now / r.elapsedAtHalf
		if timeGrowth <= 0 {
			return 0
		}
		ratio := peakGrowth / timeGrowth
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
			return 0
		}
		return ratio
	}
	return 0
}

// excursionExponent fits log(T) = a + b*log(M) over completed excursions with
// peak >= 2 (peak-1 excursions carry no scaling information). Returns 0 when
// there is too little spread in M to identify a slope (R11: guarded denominator).
func excursionExponent(exc []excursion) float64 {
	var xs, ys []float64
	for _, e := range exc {
		if e.peak < 2 || e.durationUs <= 0 {
			continue
		}
		xs = append(xs, math.Log(e.peak))
		ys = append(ys, math.Log(float64(e.durationUs)))
	}
	if len(xs) < 5 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxy += xs[i] * ys[i]
		sxx += xs[i] * xs[i]
	}
	n := float64(len(xs))
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	b := (n*sxy - sx*sy) / den
	if math.IsNaN(b) || math.IsInf(b, 0) {
		return 0
	}
	return b
}

// Detect bands the configured statistic against the calibrated threshold.
func (r *RandomWalkDetector) Detect() Result {
	signals := make(map[string]float64)
	stat := r.statistic()
	h := r.cfg.Threshold

	signals["statistic"] = stat
	signals["threshold"] = h
	signals["peak"] = r.peak
	signals["walk_level"] = r.level()
	signals["elapsed_sec"] = float64(r.elapsedUs()) / 1e6
	signals["excursions"] = float64(len(r.excursions))
	signals["excursion_exponent"] = excursionExponent(r.excursions)
	total := float64(r.lastTsUs - r.firstTsUs)
	idleFrac := 0.0
	if total > 0 {
		idleFrac = float64(r.idleUs) / total
	}
	signals["idle_fraction"] = idleFrac
	signals["observations"] = float64(r.observations)

	if r.observations < int64(r.cfg.MinObservations) || r.elapsedUs() <= 0 {
		return Result{Level: Stable, Score: 0, Confidence: 0, Signals: signals}
	}

	if h > 0 && stat > h {
		r.consecutive++
	} else {
		r.consecutive = 0
	}

	level := Stable
	switch {
	case r.consecutive >= r.cfg.ConsecutiveK && h > 0 && stat > r.cfg.BacklogK*h:
		level = Overloaded
	case r.consecutive >= r.cfg.ConsecutiveK:
		level = Backlogged
	}

	score := 0.0
	if h > 0 {
		score = math.Min(1.0, math.Max(0.0, stat)/(r.cfg.BacklogK*h))
	}
	return Result{
		Level: level, Score: score,
		Confidence: math.Min(1.0, float64(r.observations)/40.0),
		Signals:    signals,
	}
}

// Reset returns the detector to its initial state (Detector contract).
func (r *RandomWalkDetector) Reset() {
	r.inFlight, r.workBacklog, r.peak = 0, 0, 0
	r.firstTsUs, r.lastTsUs, r.haveFirst = 0, 0, false
	r.observations, r.idleUs = 0, 0
	r.isAtZero = true
	r.curExcPeak, r.curExcStart, r.inExcursion = 0, 0, false
	r.excursions = nil
	r.pendingWork = make(map[string]float64)
	r.consecutive = 0
	r.peakAtHalf, r.elapsedAtHalf, r.halfHorizonUs = 0, 0, 0
}
