package sim

import (
	"math"
	"testing"
)

// Test fixture: 8192-token aggressor and 256-token cooperator at
// blockSize=16 → kappa_aggressor = 512, kappa_cooperator = 16.
const (
	tNAgg     float64 = 8192
	tKappaAgg float64 = 512
	tNCoop    float64 = 256
	tKappaCoop float64 = 16
)

// ─── Axiom A2: slackness preservation ──────────────────────────────
//
// Every form in the family EXCEPT EAFormQuadraticControl must reduce
// to cost = N when pressure == 0. This is the load-bearing structural
// property — testing it for every form is the empirical demonstration
// that the family axiom holds for the chosen forms.

func TestEAForms_AxiomA2_SlacknessPreservation(t *testing.T) {
	cases := []struct {
		name   string
		form   EAFormName
		params EAFormParams
		// expectViolation: true iff the form DELIBERATELY violates A2
		// (only EAFormQuadraticControl).
		expectViolation bool
	}{
		{name: "additive",        form: EAFormAdditive,        params: EAFormParams{AlphaT: 1, AlphaK: 50}},
		{name: "multiplicative",  form: EAFormMultiplicative,  params: EAFormParams{Weight: 0.005}},
		{name: "power",           form: EAFormPower,           params: EAFormParams{Weight: 0.005, Power: 2}},
		{name: "threshold",       form: EAFormThreshold,       params: EAFormParams{AlphaT: 1, AlphaK: 50, Threshold: 0.1}},
		{name: "log",             form: EAFormLog,             params: EAFormParams{AlphaT: 1, AlphaK: 1}},
		{name: "convex",          form: EAFormConvex,          params: EAFormParams{AlphaT: 1, AlphaK: 50, Weight: 0.005, ConvexMix: 0.5}},
		{name: "quadratic-CONTROL (violates A2)", form: EAFormQuadraticControl, params: EAFormParams{AlphaT: 1, AlphaK: 1}, expectViolation: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cost := NewCostFn(c.form, c.params)
			// Aggressor at zero pressure
			gotAgg := cost(tNAgg, tKappaAgg, 0)
			gotCoop := cost(tNCoop, tKappaCoop, 0)
			expectAgg := tNAgg
			expectCoop := tNCoop
			if c.expectViolation {
				if gotAgg <= expectAgg {
					t.Errorf("expected A2 VIOLATION (cost > N at pressure=0), but got cost=%v <= N=%v", gotAgg, expectAgg)
				}
			} else {
				if math.Abs(gotAgg-expectAgg) > 1e-9 {
					t.Errorf("axiom A2 violated for aggressor: cost=%v, want %v", gotAgg, expectAgg)
				}
				if math.Abs(gotCoop-expectCoop) > 1e-9 {
					t.Errorf("axiom A2 violated for cooperator: cost=%v, want %v", gotCoop, expectCoop)
				}
			}
		})
	}
}

// ─── Axiom A3: monotonicity in κ and μ̂ ─────────────────────────────
//
// Cost must be non-decreasing in both κ (block consumption) and μ̂
// (pressure). Tests this for each form by sampling.

func TestEAForms_AxiomA3_MonotonicityInKappa(t *testing.T) {
	forms := []struct {
		name   string
		form   EAFormName
		params EAFormParams
	}{
		{"additive", EAFormAdditive, EAFormParams{AlphaT: 1, AlphaK: 50}},
		{"multiplicative", EAFormMultiplicative, EAFormParams{Weight: 0.005}},
		{"power", EAFormPower, EAFormParams{Weight: 0.005, Power: 2}},
		{"quadratic-control", EAFormQuadraticControl, EAFormParams{AlphaT: 1, AlphaK: 0.01}},
		{"threshold", EAFormThreshold, EAFormParams{AlphaT: 1, AlphaK: 50, Threshold: 0.1}},
		{"log", EAFormLog, EAFormParams{AlphaT: 1, AlphaK: 1}},
		{"convex", EAFormConvex, EAFormParams{AlphaT: 1, AlphaK: 50, Weight: 0.005, ConvexMix: 0.5}},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			cost := NewCostFn(f.form, f.params)
			// At fixed pressure=1.0, sweep kappa.
			prev := cost(1000, 0, 1.0)
			for k := 1.0; k <= 1024; k *= 2 {
				cur := cost(1000, k, 1.0)
				if cur < prev {
					t.Errorf("monotonicity in κ violated at κ=%v: cost=%v < prev=%v", k, cur, prev)
				}
				prev = cur
			}
		})
	}
}

func TestEAForms_AxiomA3_MonotonicityInPressure(t *testing.T) {
	forms := []struct {
		name   string
		form   EAFormName
		params EAFormParams
	}{
		{"additive", EAFormAdditive, EAFormParams{AlphaT: 1, AlphaK: 50}},
		{"multiplicative", EAFormMultiplicative, EAFormParams{Weight: 0.005}},
		{"power", EAFormPower, EAFormParams{Weight: 0.005, Power: 2}},
		// quadratic-control intentionally ignores pressure — not monotone in p
		{"threshold", EAFormThreshold, EAFormParams{AlphaT: 1, AlphaK: 50, Threshold: 0.0}},
		{"log", EAFormLog, EAFormParams{AlphaT: 1, AlphaK: 1}},
		{"convex", EAFormConvex, EAFormParams{AlphaT: 1, AlphaK: 50, Weight: 0.005, ConvexMix: 0.5}},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			cost := NewCostFn(f.form, f.params)
			prev := cost(tNAgg, tKappaAgg, 0)
			for p := 0.0; p <= 1.0+1e-9; p += 0.1 {
				cur := cost(tNAgg, tKappaAgg, p)
				if cur < prev-1e-9 {
					t.Errorf("monotonicity in μ̂ violated at p=%v: cost=%v < prev=%v", p, cur, prev)
				}
				prev = cur
			}
		})
	}
}

// ─── Per-form numerical correctness ────────────────────────────────

func TestEAFormAdditive_Formula(t *testing.T) {
	cost := NewCostFn(EAFormAdditive, EAFormParams{AlphaT: 1, AlphaK: 4})
	// Aggressor at full pressure: 1×8192 + 4×1×512 = 10240
	if got := cost(tNAgg, tKappaAgg, 1.0); math.Abs(got-10240) > 1e-9 {
		t.Errorf("additive aggressor: want 10240, got %v", got)
	}
	// Cooperator at full pressure: 1×256 + 4×1×16 = 320
	if got := cost(tNCoop, tKappaCoop, 1.0); math.Abs(got-320) > 1e-9 {
		t.Errorf("additive cooperator: want 320, got %v", got)
	}
}

func TestEAFormAdditive_SingleChannelAblation(t *testing.T) {
	// AlphaK = 0 disables the capacity channel — admission becomes pure
	// token-bucket. This is the empirical setup for the paper's converse
	// claim ("removing the capacity channel ⇒ welfare loss").
	cost := NewCostFn(EAFormAdditive, EAFormParams{AlphaT: 1, AlphaK: 0})
	if got := cost(tNAgg, tKappaAgg, 1.0); math.Abs(got-tNAgg) > 1e-9 {
		t.Errorf("AlphaK=0 should disable capacity channel; got %v, want %v", got, tNAgg)
	}
}

func TestEAFormPower_Beta1EqualsMultiplicative(t *testing.T) {
	// β = 1 is exactly the multiplicative form. Cross-form numerical equivalence.
	powCost := NewCostFn(EAFormPower, EAFormParams{Weight: 0.005, Power: 1})
	mulCost := NewCostFn(EAFormMultiplicative, EAFormParams{Weight: 0.005})
	for _, p := range []float64{0, 0.5, 1.0} {
		for _, k := range []float64{16, 64, 512} {
			a := powCost(tNAgg, k, p)
			b := mulCost(tNAgg, k, p)
			if math.Abs(a-b) > 1e-6 {
				t.Errorf("power(β=1) != multiplicative at p=%v, k=%v: pow=%v, mul=%v", p, k, a, b)
			}
		}
	}
}

func TestEAFormConvex_Endpoints(t *testing.T) {
	// θ = 0 → pure additive; θ = 1 → pure multiplicative.
	additive := NewCostFn(EAFormAdditive, EAFormParams{AlphaT: 1, AlphaK: 50})
	multiplicative := NewCostFn(EAFormMultiplicative, EAFormParams{Weight: 0.005})
	convexAt0 := NewCostFn(EAFormConvex, EAFormParams{AlphaT: 1, AlphaK: 50, Weight: 0.005, ConvexMix: 0.0})
	convexAt1 := NewCostFn(EAFormConvex, EAFormParams{AlphaT: 1, AlphaK: 50, Weight: 0.005, ConvexMix: 1.0})
	for _, p := range []float64{0, 0.5, 1.0} {
		if math.Abs(convexAt0(tNAgg, tKappaAgg, p)-additive(tNAgg, tKappaAgg, p)) > 1e-6 {
			t.Errorf("convex(θ=0) != additive at p=%v", p)
		}
		if math.Abs(convexAt1(tNAgg, tKappaAgg, p)-multiplicative(tNAgg, tKappaAgg, p)) > 1e-6 {
			t.Errorf("convex(θ=1) != multiplicative at p=%v", p)
		}
	}
}

func TestEAFormThreshold_Deadband(t *testing.T) {
	// θ = 0.5: pressure ≤ 0.5 charges only the token channel.
	cost := NewCostFn(EAFormThreshold, EAFormParams{AlphaT: 1, AlphaK: 100, Threshold: 0.5})
	// At p=0.3, below threshold: cost = N only
	if got := cost(tNAgg, tKappaAgg, 0.3); math.Abs(got-tNAgg) > 1e-9 {
		t.Errorf("below threshold: want token-only=%v, got %v", tNAgg, got)
	}
	// At p=0.7, above threshold: cost = N + 100·0.7·512 = 8192 + 35840 = 44032
	if got := cost(tNAgg, tKappaAgg, 0.7); math.Abs(got-44032) > 1e-6 {
		t.Errorf("above threshold: want 44032, got %v", got)
	}
}

func TestEAFormLog_Concavity(t *testing.T) {
	// log form must satisfy: doubling N less than doubles the capacity
	// charge (concavity in N within the capacity term).
	cost := NewCostFn(EAFormLog, EAFormParams{AlphaT: 0, AlphaK: 1})
	c1 := cost(1000, 100, 1.0)
	c2 := cost(2000, 100, 1.0)
	if c2 >= 2.0*c1 {
		t.Errorf("log form must be concave in N: cost(2N)=%v >= 2·cost(N)=%v", c2, 2.0*c1)
	}
}

func TestEAFormQuadraticControl_ViolatesA2_DeliberatelyDocumented(t *testing.T) {
	// Negative-control form: cost > N at pressure=0. Documents that the
	// quadratic-in-κ form IS NOT an A2-preserving form. The test
	// codifies the violation rather than the conformance.
	cost := NewCostFn(EAFormQuadraticControl, EAFormParams{AlphaT: 1, AlphaK: 0.001})
	got := cost(tNAgg, tKappaAgg, 0)
	expectMore := tNAgg + 0.001*tKappaAgg*tKappaAgg
	if math.Abs(got-expectMore) > 1e-9 {
		t.Errorf("quadratic-control: want %v, got %v", expectMore, got)
	}
	// And its cost is independent of pressure (the failure mode the
	// negative control demonstrates).
	gotP1 := cost(tNAgg, tKappaAgg, 1.0)
	if math.Abs(gotP1-got) > 1e-9 {
		t.Errorf("quadratic-control: cost should not depend on pressure (intentional A2 violation)")
	}
}

// ─── Construction validation (R3) ─────────────────────────────────

func TestNewCostFn_ValidatesParams(t *testing.T) {
	negativeWeight := func() {
		_ = NewCostFn(EAFormMultiplicative, EAFormParams{Weight: -0.001})
	}
	if !panics(negativeWeight) {
		t.Error("expected panic on negative Weight")
	}
	nanAlphaK := func() {
		_ = NewCostFn(EAFormAdditive, EAFormParams{AlphaT: 1, AlphaK: math.NaN()})
	}
	if !panics(nanAlphaK) {
		t.Error("expected panic on NaN AlphaK")
	}
	zeroPower := func() {
		_ = NewCostFn(EAFormPower, EAFormParams{Weight: 0.005, Power: 0})
	}
	if !panics(zeroPower) {
		t.Error("expected panic on Power=0")
	}
	thresholdOutOfRange := func() {
		_ = NewCostFn(EAFormThreshold, EAFormParams{AlphaT: 1, AlphaK: 1, Threshold: 1.5})
	}
	if !panics(thresholdOutOfRange) {
		t.Error("expected panic on Threshold > 1")
	}
	convexMixOutOfRange := func() {
		_ = NewCostFn(EAFormConvex, EAFormParams{ConvexMix: 1.5})
	}
	if !panics(convexMixOutOfRange) {
		t.Error("expected panic on ConvexMix > 1")
	}
	unknownForm := func() {
		_ = NewCostFn(EAFormName("nonsense"), EAFormParams{})
	}
	if !panics(unknownForm) {
		t.Error("expected panic on unknown form")
	}
}

func TestIsValidEAForm(t *testing.T) {
	for _, name := range ValidEAFormNames() {
		if !IsValidEAForm(EAFormName(name)) {
			t.Errorf("IsValidEAForm(%q) returned false but is in ValidEAFormNames", name)
		}
	}
	if IsValidEAForm("nonsense") {
		t.Error("IsValidEAForm(nonsense) should return false")
	}
}

func panics(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return
}
