package sim

import (
	"fmt"
	"math"
)

// EA-aware admission cost forms.
//
// All forms in this file satisfy the "EA-Family axioms" — the structural
// properties that define what it means for an admission cost function to
// be EA-aware:
//
//   A1: Two-signal input — cost depends on (κ, μ̂) plus the request's
//       own size N. No tenant-supplied class.
//
//   A2: Slackness preservation — when μ̂ = 0 (cache slack regime), cost
//       reduces to the base N (token-bucket parity). No overhead in
//       unloaded regimes. Mirrors the simulator's μ̂-gate behavior at
//       sim/simulator.go:1338-1340.
//
//   A3: Monotonicity — cost is non-decreasing in κ and in μ̂. Aggressors
//       never pay LESS than cooperators under matched pressure.
//
// We include EAFormQuadraticControl as a NEGATIVE-CONTROL form that
// VIOLATES A2 by design, to empirically demonstrate that the slackness
// axiom is load-bearing. Including a known-bad form is part of the
// scientific contribution: it gives us a dataset that demonstrates A2
// matters rather than asserting it does.

// EAFormName enumerates the admission cost forms supported by
// EAAwareTokenBucket. Each form combines (N, κ, μ̂) differently while
// preserving the EA-Family axioms (except EAFormQuadraticControl which
// intentionally violates A2 — see file-level docs).
type EAFormName string

const (
	// EAFormMultiplicative: cost = N · (1 + w · μ̂ · κ).
	// Multiplicatively couples the capacity channel with N. Produces
	// super-linear discrimination of large requests under pressure.
	// This is the originally-shipped form; default for backward compat.
	EAFormMultiplicative EAFormName = "multiplicative"

	// EAFormAdditive: cost = α_T · N + α_K · μ̂ · κ.
	// Paper-canonical: two channels with independent weights and clean
	// ablation. Setting α_K = 0 disables the capacity channel — used
	// for the "single-channel ablation" that empirically demonstrates
	// the paper's converse claim (omitting one channel ⇒ welfare loss).
	EAFormAdditive EAFormName = "additive"

	// EAFormPower: cost = N · (1 + w · μ̂ · κ)^β with β >= 1.
	// Multiplicative with tunable amplification. β = 1 → multiplicative;
	// β > 1 amplifies discrimination at the cost of larger gradient
	// near the bucket-rejection boundary (potentially less robust to
	// μ̂ estimator noise — testable property).
	EAFormPower EAFormName = "power"

	// EAFormQuadraticControl: cost = α_T · N + α_K · κ² (NO μ̂).
	// VIOLATES A2 by design — charges for occupancy regardless of
	// pressure. Imposes overhead in unloaded regimes. Included as a
	// negative control to demonstrate empirically that the μ̂ gate
	// matters. NOT a recommended deployment form.
	EAFormQuadraticControl EAFormName = "quadratic-control"

	// EAFormThreshold: cost = α_T · N + α_K · μ̂ · κ · 𝟙[μ̂ > θ].
	// Additive form with deadband: capacity channel charges only when
	// pressure exceeds threshold θ. Reduces noise-induced charges at
	// low pressure. Sharper transition than smooth additive.
	EAFormThreshold EAFormName = "threshold"

	// EAFormLog: cost = α_T · N + α_K · log(1 + μ̂ · κ · N).
	// Concave / diminishing-marginal-cost: large requests pay more in
	// absolute terms but less per unit κ. Throughput-preserving variant.
	EAFormLog EAFormName = "log"

	// EAFormConvex: cost = (1 − θ) · additive + θ · multiplicative.
	// Continuous one-parameter family interpolating between additive
	// (θ=0) and multiplicative (θ=1). Useful for sensitivity studies
	// across the additive↔multiplicative spectrum.
	EAFormConvex EAFormName = "convex"
)

// EAFormParams holds per-form configuration. Not all fields are used by
// all forms; each form's docstring lists the fields it consumes. Unused
// fields are ignored (no validation error) — the form's constructor only
// validates the fields it reads.
type EAFormParams struct {
	// AlphaT: token-channel weight. Used by additive, threshold, log,
	// quadratic-control, convex. Defaults to 1.0 (token-bucket parity)
	// when zero.
	AlphaT float64

	// AlphaK: capacity-channel weight. Used by additive, threshold, log,
	// quadratic-control, convex. Setting AlphaK = 0 in additive form
	// disables the capacity channel (single-channel ablation).
	AlphaK float64

	// Weight: multiplicative coefficient. Used by multiplicative,
	// power, convex.
	Weight float64

	// Power: exponent β for power-law form. Used by power. Must be > 0.
	Power float64

	// Threshold: pressure deadband θ. Used by threshold. Must be in
	// [0, 1] (pressure is bounded to that range).
	Threshold float64

	// ConvexMix: convex combination weight θ ∈ [0, 1]. Used by convex.
	// θ = 0 → pure additive; θ = 1 → pure multiplicative.
	ConvexMix float64
}

// CostFn computes admission cost given the request's size (N input
// tokens), the request's kappa (KV blocks it would hold during prefill),
// and the current cluster pressure signal in [0, 1].
//
// Contract: when pressure == 0, the result must equal N (axiom A2),
// EXCEPT for EAFormQuadraticControl which intentionally violates A2.
type CostFn func(N float64, kappa float64, pressure float64) float64

// IsValidEAForm returns true if name is a recognized form.
func IsValidEAForm(name EAFormName) bool {
	switch name {
	case EAFormMultiplicative, EAFormAdditive, EAFormPower,
		EAFormQuadraticControl, EAFormThreshold, EAFormLog, EAFormConvex:
		return true
	}
	return false
}

// ValidEAFormNames returns sorted recognized form names. CLI uses this
// in its error message for unknown --ea-aware-form values.
func ValidEAFormNames() []string {
	return []string{
		string(EAFormAdditive),
		string(EAFormConvex),
		string(EAFormLog),
		string(EAFormMultiplicative),
		string(EAFormPower),
		string(EAFormQuadraticControl),
		string(EAFormThreshold),
	}
}

// NewCostFn constructs the cost function for the given form. Validates
// per R3: all weights must be finite and >= 0; Power must be > 0;
// Threshold must be in [0, 1]; ConvexMix must be in [0, 1].
//
// Panics on invalid params (programmer error). The CLI boundary
// validates user-supplied values before reaching this constructor.
func NewCostFn(form EAFormName, p EAFormParams) CostFn {
	if !IsValidEAForm(form) {
		panic(fmt.Sprintf("NewCostFn: unknown form %q; valid: %v", form, ValidEAFormNames()))
	}
	checkNonNegFinite("AlphaT", p.AlphaT)
	checkNonNegFinite("AlphaK", p.AlphaK)
	checkNonNegFinite("Weight", p.Weight)
	switch form {
	case EAFormAdditive:
		alphaT := p.AlphaT
		if alphaT == 0 {
			alphaT = 1.0
		}
		ak := p.AlphaK
		return func(N, kappa, pressure float64) float64 {
			return alphaT*N + ak*pressure*kappa
		}

	case EAFormMultiplicative:
		w := p.Weight
		return func(N, kappa, pressure float64) float64 {
			return N * (1.0 + w*pressure*kappa)
		}

	case EAFormPower:
		if p.Power <= 0 || math.IsNaN(p.Power) || math.IsInf(p.Power, 0) {
			panic(fmt.Sprintf("NewCostFn[power]: Power must be a finite value > 0, got %v", p.Power))
		}
		w := p.Weight
		beta := p.Power
		return func(N, kappa, pressure float64) float64 {
			base := 1.0 + w*pressure*kappa
			return N * math.Pow(base, beta)
		}

	case EAFormQuadraticControl:
		// Negative control: α_T·N + α_K·κ² (NO μ̂).
		// Charges for occupancy unconditionally; violates A2.
		alphaT := p.AlphaT
		if alphaT == 0 {
			alphaT = 1.0
		}
		ak := p.AlphaK
		return func(N, kappa, _ /*pressure unused*/ float64) float64 {
			return alphaT*N + ak*kappa*kappa
		}

	case EAFormThreshold:
		if p.Threshold < 0 || p.Threshold > 1 || math.IsNaN(p.Threshold) {
			panic(fmt.Sprintf("NewCostFn[threshold]: Threshold must be in [0,1], got %v", p.Threshold))
		}
		alphaT := p.AlphaT
		if alphaT == 0 {
			alphaT = 1.0
		}
		ak := p.AlphaK
		theta := p.Threshold
		return func(N, kappa, pressure float64) float64 {
			capCharge := 0.0
			if pressure > theta {
				capCharge = ak * pressure * kappa
			}
			return alphaT*N + capCharge
		}

	case EAFormLog:
		alphaT := p.AlphaT
		if alphaT == 0 {
			alphaT = 1.0
		}
		ak := p.AlphaK
		return func(N, kappa, pressure float64) float64 {
			// log1p(x) = log(1+x); avoids precision loss for small x.
			return alphaT*N + ak*math.Log1p(pressure*kappa*N)
		}

	case EAFormConvex:
		if p.ConvexMix < 0 || p.ConvexMix > 1 || math.IsNaN(p.ConvexMix) {
			panic(fmt.Sprintf("NewCostFn[convex]: ConvexMix must be in [0,1], got %v", p.ConvexMix))
		}
		alphaT := p.AlphaT
		if alphaT == 0 {
			alphaT = 1.0
		}
		ak := p.AlphaK
		w := p.Weight
		theta := p.ConvexMix
		return func(N, kappa, pressure float64) float64 {
			additive := alphaT*N + ak*pressure*kappa
			multiplicative := N * (1.0 + w*pressure*kappa)
			return (1.0-theta)*additive + theta*multiplicative
		}
	}
	// Unreachable — IsValidEAForm guard at top.
	panic("unreachable")
}

func checkNonNegFinite(name string, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		panic(fmt.Sprintf("NewCostFn: %s must be a finite value >= 0, got %v", name, v))
	}
}
