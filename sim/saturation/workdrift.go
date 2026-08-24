// sim/saturation/workdrift.go
//
// The work-conservation residual detectors — SWD (Spec Work Drift) and OWD
// (Observed Work Drift) — specified in
// ../papers/saturation/detection_strategies.md §2e.
//
// WHY THESE EXIST. The three pre-existing detectors are level/symptom based
// (threshold reads mean E2E; composite blends a rate deficit with a latency
// trend; backlog-drift fits a slope to a reconstructed in-flight count). §2a of
// the source document states the recurring lesson: strategies that estimate the
// DRIFT beat strategies that measure a symptom of it. These two estimate the
// drift directly, as a work-conservation residual.
//
// THE STATISTIC. Per request i, with prefill tokens I_i and decode tokens O_i,
// the work it brings is w_i = kappa*I_i + O_i, expressed in decode-token
// equivalents (kappa is the prefill:decode cost ratio). Against a drain rate
// r_dec (decode-token equivalents per microsecond) over an inter-arrival gap
// dt, the residual follows a Lindley recursion:
//
//	R <- max(0, R + w_i - r_dec*dt)
//
// The max(0,·) is the reflecting boundary: R is the backlog of unserved work,
// which cannot go negative. Positive drift (arriving work exceeding drain
// capacity) makes R climb without bound; negative drift pins it near zero. This
// is exactly the reflected-random-walk / Lindley structure, so R's growth is the
// drift sign rather than a symptom of it.
//
// WHAT MAKES IT ADAPTIVE (§2e.4). r_dec and kappa are NOT pinned — they are
// re-estimated online from BUSY WINDOWS via a ridge fit. A window in which at
// least one request was always in flight is a window the engine spent entirely
// working, so tokens_served vs window_length is a direct rate measurement:
//
//	I_w*a + O_w*b = T_w      where a = 1/r_pre, b = 1/r_dec
//
// Many such windows with differing prefill:decode mixes give an overdetermined
// system in two unknowns. Ridge regularization (toward the seeded prior) keeps
// it finite when the mixes are collinear, which is the common case for a
// fixed-shape workload. Because a later window returns a lower rate when
// capacity collapses, the detector sees THRASH for free — a pinned-rate detector
// structurally cannot (this is the T3 mechanism, §2e.6).
//
// CAUSALITY (§2e.5). O_i is unknown at arrival. This implementation is
// COMPLETION-INDEXED: the recursion advances on completion, using the now-known
// O_i and the arrival gap recorded earlier. That costs one request-latency of
// delay and needs no forecast — and it is what keeps the detector inside BLIS
// invariant INV-9 (the control plane must not read Request.OutputTokens before
// the execution engine produces them). An arrival-indexed variant that peeked at
// the true O_i would violate the oracle-knowledge boundary and overstate its own
// accuracy.
//
// SWD vs OWD. They share this entire loop and differ only in the threshold h:
// SWD computes h from the workload spec's burst envelope (so only genuinely
// super-capacity load can climb past it); OWD LEARNS h from a running quantile
// of its own sub-threshold history, costing a warm-up but needing no spec.
// Only OWD is production-deployable, since production traffic has no spec.
//
// DETERMINISM (INV-6). No clock reads, no randomness, no map iteration over
// float accumulation: every fold is in event order, so identical input yields a
// byte-identical signal map.
package saturation

import (
	"math"
	"sort"
)

// workDriftKind distinguishes the two threshold policies.
type workDriftKind int

const (
	kindSWD workDriftKind = iota // threshold computed from the spec envelope
	kindOWD                      // threshold learned from a running quantile
)

// Tunable defaults. These are the FPR-calibration knobs (metamorphic_tests.md
// §3.4 requires every compared detector to expose one), not magic constants.
const (
	defaultWorkDriftKappa0       = 0.02      // prefill:decode cost ratio prior
	defaultWorkDriftWindowUs     = 1_000_000 // 1s busy-window width
	defaultWorkDriftNumWindows   = 20        // ring size for the ridge fit
	defaultWorkDriftConsecutiveK = 3         // consecutive breaches before firing
	defaultWorkDriftRidgeLambda  = 1e-3      // ridge regularization strength
	defaultWorkDriftEWMAAlpha    = 0.3       // smoothing on r_dec / kappa updates
	defaultWorkDriftQuantile     = 0.95      // OWD learned-threshold quantile
	defaultWorkDriftRDec0        = 5000.0    // seeded drain rate, tok-equiv/sec
	defaultWorkDriftThreshold    = 5000.0    // default h
	workDriftBacklogK            = 3.0       // BACKLOGGED -> OVERLOADED multiplier
)

// workDriftConfig is the detector's tunable surface.
type workDriftConfig struct {
	Kind         string
	Kappa0       float64 // prefill:decode cost prior
	RDec0        float64 // seeded drain rate, decode-token equivalents per SECOND
	Threshold    float64 // h; for OWD a floor until the quantile warms up
	WindowSizeUs int64
	NumWindows   int
	ConsecutiveK int
	FreezeKappa  bool // pin kappa (used to prove the kappa=0 -> VWD reduction)
	FreezeRDec   bool // pin r_dec (turns this into the VWD baseline)
	Quantile     float64
}

// busyWindow is one closed observation window used by the ridge fit.
type busyWindow struct {
	sumI, sumO int64
	durationUs int64
}

// pendingArrival records what a completion needs from its own arrival: the
// inter-arrival gap at the time it arrived. Holding it here (rather than
// advancing the recursion at arrival) is what makes the detector
// completion-indexed and INV-9 clean.
type pendingArrival struct {
	gapUs int64
}

// WorkDriftDetector implements Detector for both SWD and OWD.
type WorkDriftDetector struct {
	kind workDriftKind
	cfg  workDriftConfig

	// Lindley residual state.
	residual    float64
	rdec        float64 // decode-token equivalents per microsecond
	kappa       float64
	consecutive int

	// Completion-indexing bookkeeping.
	pending         map[string]pendingArrival
	lastArrivalUs   int64
	haveLastArrival bool

	// Busy-window accumulation for the online ridge fit.
	windows           []busyWindow
	curWinIdx         int64
	haveCurWin        bool
	curSumI           int64
	curSumO           int64
	curWinBusy        bool
	curWinCompletions int
	inFlight          int64

	// Diagnostics.
	rdecHistory []float64
	// OWD learned threshold: history of sub-threshold residuals.
	residualHistory []float64

	observed int64
}

// NewWorkDriftDetector builds an SWD or OWD detector from a validated config.
func NewWorkDriftDetector(cfg workDriftConfig) Detector {
	return newWorkDrift(cfg)
}

// newWorkDriftForTest is the test seam (canonical constructor, R4).
func newWorkDriftForTest(cfg workDriftConfig) *WorkDriftDetector {
	return newWorkDrift(cfg)
}

func newWorkDrift(cfg workDriftConfig) *WorkDriftDetector {
	if cfg.WindowSizeUs <= 0 {
		cfg.WindowSizeUs = defaultWorkDriftWindowUs
	}
	if cfg.NumWindows <= 0 {
		cfg.NumWindows = defaultWorkDriftNumWindows
	}
	if cfg.ConsecutiveK <= 0 {
		cfg.ConsecutiveK = defaultWorkDriftConsecutiveK
	}
	if cfg.Quantile <= 0 || cfg.Quantile >= 1 {
		cfg.Quantile = defaultWorkDriftQuantile
	}
	if cfg.RDec0 <= 0 {
		cfg.RDec0 = 5000.0
	}
	kind := kindOWD
	if cfg.Kind == "swd" {
		kind = kindSWD
	}
	return &WorkDriftDetector{
		kind:    kind,
		cfg:     cfg,
		rdec:    cfg.RDec0 / 1e6, // per-second -> per-microsecond
		kappa:   cfg.Kappa0,
		pending: make(map[string]pendingArrival),
	}
}

func (w *WorkDriftDetector) Name() string {
	if w.kind == kindSWD {
		return "swd"
	}
	return "owd"
}

// Observe folds one event into the residual and the busy-window accumulator.
//
// Arrivals only RECORD their inter-arrival gap (and raise in-flight); the
// recursion advances on COMPLETION, when O_i is known. See the causality note
// in the package comment.
func (w *WorkDriftDetector) Observe(event Event) {
	w.observed++
	switch event.Type {
	case Arrival:
		gap := int64(0)
		if w.haveLastArrival {
			gap = event.Timestamp - w.lastArrivalUs
			if gap < 0 {
				gap = 0 // out-of-order guard; never credit negative drain time
			}
		}
		w.lastArrivalUs = event.Timestamp
		w.haveLastArrival = true
		w.pending[event.RequestID] = pendingArrival{gapUs: gap}
		w.inFlight++
		w.rollWindow(event.Timestamp)
		if w.inFlight > 0 {
			w.curWinBusy = true
		}

	case Completion:
		w.rollWindow(event.Timestamp)
		p, ok := w.pending[event.RequestID]
		if !ok {
			// A completion with no recorded arrival carries no drain interval;
			// credit its work with zero elapsed time rather than guessing (R1:
			// no silent skip — the work still enters the residual).
			p = pendingArrival{gapUs: 0}
		} else {
			delete(w.pending, event.RequestID)
		}

		work := w.kappa*float64(event.InputTokens) + float64(event.OutputTokens)
		drained := w.rdec * float64(p.gapUs)
		w.residual = math.Max(0.0, w.residual+work-drained)

		// Busy-window accumulation: tokens served inside this window.
		w.curSumI += int64(event.InputTokens)
		w.curSumO += int64(event.OutputTokens)
		w.curWinCompletions++
		if w.inFlight > 0 {
			w.inFlight--
		}

		// Firing state is evaluated on the completion-indexed residual.
		if w.residual > w.threshold() {
			w.consecutive++
		} else {
			w.consecutive = 0
			// Only sub-threshold residuals inform OWD's learned quantile, so
			// the threshold cannot chase its own firings upward.
			w.residualHistory = append(w.residualHistory, w.residual)
			if len(w.residualHistory) > 512 {
				w.residualHistory = w.residualHistory[1:]
			}
		}
	}
}

// rollWindow closes any windows the timestamp has advanced past, pushing a busy
// one into the ring and refitting.
func (w *WorkDriftDetector) rollWindow(tsUs int64) {
	idx := tsUs / w.cfg.WindowSizeUs
	if !w.haveCurWin {
		w.curWinIdx = idx
		w.haveCurWin = true
		return
	}
	if idx <= w.curWinIdx {
		return
	}
	// Close the current window.
	//
	// §2e.6's T3 mitigation: a window with in-flight > 0 but ZERO completions is
	// still busy — charge its duration with no work credited, which pushes r_dec
	// DOWN. That is the correct direction, and skipping such windows is exactly
	// the mis-gating that would reproduce a pinned-rate detector's blindness to
	// thrash.
	if w.curWinBusy {
		w.windows = append(w.windows, busyWindow{
			sumI: w.curSumI, sumO: w.curSumO, durationUs: w.cfg.WindowSizeUs,
		})
		if len(w.windows) > w.cfg.NumWindows {
			w.windows = w.windows[1:]
		}
		w.refit()
	}
	w.curSumI, w.curSumO, w.curWinCompletions = 0, 0, 0
	w.curWinIdx = idx
	// The next window starts busy iff work is still resident.
	w.curWinBusy = w.inFlight > 0
}

// refit re-estimates (kappa, r_dec) by ridge regression over the busy-window
// ring (§2e.4), then EWMA-smooths the result. Ridge (rather than plain OLS) is
// what keeps this finite when every window carries the same prefill:decode mix,
// which makes the two columns collinear.
func (w *WorkDriftDetector) refit() {
	if w.cfg.FreezeRDec && w.cfg.FreezeKappa {
		return
	}
	if len(w.windows) < 3 {
		return
	}
	// Normal equations for [a b] minimizing ||X[a b]^T - T||^2 + lambda*||.||^2,
	// with the ridge pulling toward the seeded prior rather than toward zero.
	var sII, sIO, sOO, sIT, sOT float64
	for _, win := range w.windows {
		fi := float64(win.sumI)
		fo := float64(win.sumO)
		ft := float64(win.durationUs)
		sII += fi * fi
		sIO += fi * fo
		sOO += fo * fo
		sIT += fi * ft
		sOT += fo * ft
	}
	lam := defaultWorkDriftRidgeLambda * (sII + sOO + 1.0)
	// Prior: b0 = 1/rdec0, a0 = kappa0 * b0.
	b0 := 1.0 / (w.cfg.RDec0 / 1e6)
	a0 := w.cfg.Kappa0 * b0

	m11 := sII + lam
	m12 := sIO
	m22 := sOO + lam
	r1 := sIT + lam*a0
	r2 := sOT + lam*b0

	det := m11*m22 - m12*m12
	if det <= 0 || math.IsNaN(det) || math.IsInf(det, 0) {
		return // keep the previous estimate; never emit NaN (R11)
	}
	a := (r1*m22 - m12*r2) / det
	b := (m11*r2 - m12*r1) / det

	// b = 1/r_dec must be positive and finite for the estimate to mean anything.
	if b <= 0 || math.IsNaN(b) || math.IsInf(b, 0) {
		return
	}
	newRDec := 1.0 / b
	if newRDec <= 0 || math.IsNaN(newRDec) || math.IsInf(newRDec, 0) {
		return
	}
	if !w.cfg.FreezeRDec {
		w.rdec = ewma(w.rdec, newRDec, defaultWorkDriftEWMAAlpha)
		w.rdecHistory = append(w.rdecHistory, w.rdec)
		if len(w.rdecHistory) > 256 {
			w.rdecHistory = w.rdecHistory[1:]
		}
	}
	if !w.cfg.FreezeKappa {
		newKappa := a / b
		if !math.IsNaN(newKappa) && !math.IsInf(newKappa, 0) && newKappa >= 0 {
			w.kappa = ewma(w.kappa, newKappa, defaultWorkDriftEWMAAlpha)
		}
	}
}

func ewma(old, new, alpha float64) float64 {
	return (1-alpha)*old + alpha*new
}

// threshold returns h. SWD uses the configured (spec-computed) envelope; OWD
// learns it from a running quantile of its own sub-threshold history, falling
// back to the configured floor until enough history exists (the warm-up §2e.5
// predicts will cost OWD some sharpness relative to SWD).
func (w *WorkDriftDetector) threshold() float64 {
	if w.kind == kindSWD {
		return w.cfg.Threshold
	}
	const minHistory = 32
	if len(w.residualHistory) < minHistory {
		return w.cfg.Threshold
	}
	sorted := make([]float64, len(w.residualHistory))
	copy(sorted, w.residualHistory)
	sort.Float64s(sorted)
	idx := int(w.cfg.Quantile * float64(len(sorted)-1))
	q := sorted[idx]
	// The learned threshold must never fall below the configured floor, or a
	// long quiet stretch would drive h to ~0 and manufacture false alarms.
	return math.Max(q, w.cfg.Threshold)
}

// Detect reports the current verdict and the six signals §2e.5 specifies.
func (w *WorkDriftDetector) Detect() Result {
	signals := make(map[string]float64)
	h := w.threshold()

	signals["residual"] = w.residual
	signals["threshold"] = h
	signals["r_dec_hat"] = w.rdec * 1e6 // back to per-second for readability
	signals["kappa_hat"] = w.kappa
	signals["in_flight"] = float64(w.inFlight)

	// drift_est: the residual's normalized excess over h — the estimated drift
	// sign. Positive means arriving work is outrunning the drain.
	driftEst := 0.0
	if h > 0 {
		driftEst = (w.residual - h) / h
	}
	signals["drift_est"] = driftEst

	// r_dec_trend: DIAGNOSTIC ONLY (§2e.5) — it deliberately gets no threshold
	// of its own, because a second ORed firing path would need its own
	// false-alarm budget and break §1a's fixed-operating-point discipline.
	// Falling r_dec under flat work means thrash; flat r_dec with rising work
	// means overload.
	signals["r_dec_trend"] = slopeOf(w.rdecHistory)

	// drift_ci: a crude spread of the recent r_dec estimates, used only to mark
	// the verdict INDETERMINATE when it straddles zero (§2e.6 gray-zone
	// mitigation).
	signals["drift_ci"] = spreadOf(w.rdecHistory)

	level := Stable
	switch {
	case w.consecutive >= w.cfg.ConsecutiveK && w.residual > workDriftBacklogK*h:
		level = Overloaded
	case w.consecutive >= w.cfg.ConsecutiveK:
		level = Backlogged
	}

	score := 0.0
	if h > 0 {
		score = math.Min(1.0, math.Max(0.0, w.residual)/(workDriftBacklogK*h))
	}
	confidence := math.Min(1.0, float64(w.observed)/40.0)

	return Result{Level: level, Score: score, Confidence: confidence, Signals: signals}
}

// slopeOf is the OLS slope of a series against its index (diagnostic).
func slopeOf(xs []float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	for i, v := range xs {
		x := float64(i)
		sx += x
		sy += v
		sxy += x * v
		sxx += x * x
	}
	fn := float64(n)
	den := fn*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (fn*sxy - sx*sy) / den
}

// spreadOf is a max-min spread, used as a cheap CI proxy.
func spreadOf(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	lo, hi := xs[0], xs[0]
	for _, v := range xs {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	return hi - lo
}

// Reset returns the detector to its initial state (Detector contract).
func (w *WorkDriftDetector) Reset() {
	w.residual = 0
	w.rdec = w.cfg.RDec0 / 1e6
	w.kappa = w.cfg.Kappa0
	w.consecutive = 0
	w.pending = make(map[string]pendingArrival)
	w.lastArrivalUs = 0
	w.haveLastArrival = false
	w.windows = nil
	w.curWinIdx = 0
	w.haveCurWin = false
	w.curSumI, w.curSumO, w.curWinCompletions = 0, 0, 0
	w.curWinBusy = false
	w.inFlight = 0
	w.rdecHistory = nil
	w.residualHistory = nil
	w.observed = 0
}
