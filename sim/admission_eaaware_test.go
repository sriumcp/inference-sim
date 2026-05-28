package sim

import (
	"math"
	"testing"
)

// fakeTracker is a deterministic TenantExternalityTracker used by tests
// to control CumExternality/KVPressureSignal independently of any real
// simulator state. Per the test discipline (#229 of agentic-strategy-
// evolution: tests CLAUDE.md), this is a SEAM-INJECTED fake — not a
// mock that asserts call shapes.
type fakeTracker struct {
	cumExt   map[string]float64
	pressure float64
}

func (f *fakeTracker) CumExternality(tid string) float64 {
	if f.cumExt == nil {
		return 0
	}
	return f.cumExt[tid]
}

func (f *fakeTracker) KVPressureSignal() float64 { return f.pressure }

// TestEAAwareTokenBucket_Construction_ValidatesParams covers R3 — bad
// inputs panic at construction time, not deep in Admit.
func TestEAAwareTokenBucket_Construction_ValidatesParams(t *testing.T) {
	innerOK := NewTokenBucket(100, 10)
	tracker := &fakeTracker{}

	tests := []struct {
		name       string
		inner      *TokenBucket
		weight     float64
		blockSize  int64
		wantPanic  bool
		wantSubstr string
	}{
		{name: "happy path", inner: innerOK, weight: 0.005, blockSize: 16, wantPanic: false},
		{name: "zero weight is valid (passthrough)", inner: innerOK, weight: 0, blockSize: 16, wantPanic: false},
		{name: "nil inner panics", inner: nil, weight: 0.005, blockSize: 16, wantPanic: true, wantSubstr: "inner"},
		{name: "negative weight panics", inner: innerOK, weight: -0.001, blockSize: 16, wantPanic: true, wantSubstr: "weight"},
		{name: "NaN weight panics", inner: innerOK, weight: math.NaN(), blockSize: 16, wantPanic: true, wantSubstr: "weight"},
		{name: "Inf weight panics", inner: innerOK, weight: math.Inf(1), blockSize: 16, wantPanic: true, wantSubstr: "weight"},
		{name: "zero blockSize panics", inner: innerOK, weight: 0.005, blockSize: 0, wantPanic: true, wantSubstr: "blockSizeTokens"},
		{name: "negative blockSize panics", inner: innerOK, weight: 0.005, blockSize: -1, wantPanic: true, wantSubstr: "blockSizeTokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if tt.wantPanic && r == nil {
					t.Errorf("expected panic, got none")
				}
				if !tt.wantPanic && r != nil {
					t.Errorf("unexpected panic: %v", r)
				}
				if tt.wantPanic && tt.wantSubstr != "" {
					msg, ok := r.(string)
					if !ok {
						msg = ""
					}
					if msg != "" && !contains(msg, tt.wantSubstr) {
						t.Errorf("expected panic message to contain %q, got %q", tt.wantSubstr, msg)
					}
				}
			}()
			_ = NewEAAwareTokenBucket(tt.inner, tracker, tt.weight, tt.blockSize)
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEAAwareTokenBucket_NilTracker_DegradesToInner verifies the
// nil-safety contract: passing a nil tracker is permitted, and Admit
// behaves identically to the inner TokenBucket.
func TestEAAwareTokenBucket_NilTracker_DegradesToInner(t *testing.T) {
	inner := NewTokenBucket(1000, 0.001) // tiny refill, isolates first-admit cost
	ea := NewEAAwareTokenBucket(inner, nil /* tracker */, 0.005, 16)
	req := &Request{ID: "r0", InputTokens: make([]int, 100)}

	admitted, _ := ea.Admit(req, &RouterState{Clock: 0})
	if !admitted {
		t.Fatal("first admit with nil tracker should succeed (cost == base)")
	}

	// Computed cost (with nil tracker) must equal len(InputTokens).
	got := ea.computeCost(req)
	if got != 100 {
		t.Errorf("nil tracker → cost should equal len(InputTokens)=100, got %v", got)
	}
}

// TestEAAwareTokenBucket_ZeroPressure_DegradesToInner verifies that
// at zero KV pressure, the cost equals the base cost — i.e., adding
// the decorator at low load has no behavioral footprint.
func TestEAAwareTokenBucket_ZeroPressure_DegradesToInner(t *testing.T) {
	inner := NewTokenBucket(10000, 100)
	tracker := &fakeTracker{pressure: 0}
	ea := NewEAAwareTokenBucket(inner, tracker, 0.5, 16)
	req := &Request{ID: "r-aggressor", InputTokens: make([]int, 8192), TenantID: "agg"}

	got := ea.computeCost(req)
	if got != 8192 {
		t.Errorf("zero pressure → cost should equal len(InputTokens)=8192, got %v", got)
	}
}

// TestEAAwareTokenBucket_PressureWeightedCost verifies the formula
// cost = base × (1 + weight × pressure × kappa) numerically.
func TestEAAwareTokenBucket_PressureWeightedCost(t *testing.T) {
	const blockSize int64 = 16
	tests := []struct {
		name     string
		weight   float64
		pressure float64
		nTokens  int
		want     float64 // expected cost
	}{
		{
			// 8192-token aggressor under full pressure, weight=0.005:
			// kappa = ceil(8192/16) = 512
			// cost  = 8192 × (1 + 0.005 × 1.0 × 512) = 8192 × 3.56 = 29163.52
			name: "aggressor at full pressure", weight: 0.005, pressure: 1.0, nTokens: 8192,
			want: 8192 * (1 + 0.005*1.0*512),
		},
		{
			// 256-token cooperator under full pressure, weight=0.005:
			// kappa = 16, multiplier = 1 + 0.005 × 16 = 1.08
			// cost  = 256 × 1.08 = 276.48
			name: "cooperator at full pressure", weight: 0.005, pressure: 1.0, nTokens: 256,
			want: 256 * (1 + 0.005*1.0*16),
		},
		{
			// Half pressure: multiplier scales linearly.
			name: "aggressor at half pressure", weight: 0.005, pressure: 0.5, nTokens: 8192,
			want: 8192 * (1 + 0.005*0.5*512),
		},
		{
			// Weight=0: passthrough regardless of pressure.
			name: "weight=0 ignores pressure", weight: 0, pressure: 1.0, nTokens: 8192,
			want: 8192,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := NewTokenBucket(1e9, 1e6) // effectively unbounded
			tracker := &fakeTracker{pressure: tt.pressure}
			ea := NewEAAwareTokenBucket(inner, tracker, tt.weight, blockSize)
			req := &Request{ID: "r0", InputTokens: make([]int, tt.nTokens)}
			got := ea.computeCost(req)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("computeCost: want %v, got %v", tt.want, got)
			}
		})
	}
}

// TestEAAwareTokenBucket_KappaCeilingDivision verifies R11 — kappa is
// computed via ceiling division so partial-block requests are charged
// for the full block they would consume.
func TestEAAwareTokenBucket_KappaCeilingDivision(t *testing.T) {
	const blockSize int64 = 16
	const weight = 1.0 // amplify so partial-block effects are visible
	// 17 tokens: would naturally consume 2 blocks (one full + one partial).
	// kappa should be 2 (ceil(17/16) = 2), not 1.
	inner := NewTokenBucket(1e9, 1e6)
	tracker := &fakeTracker{pressure: 1.0}
	ea := NewEAAwareTokenBucket(inner, tracker, weight, blockSize)

	req17 := &Request{ID: "r17", InputTokens: make([]int, 17)}
	got := ea.computeCost(req17)
	want := 17 * (1 + 1.0*1.0*2.0) // multiplier = 1 + 2 = 3
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("kappa for 17 tokens (block=16): want cost %v (kappa=2), got %v", want, got)
	}

	// Edge case: exactly one block (16 tokens). kappa=1.
	req16 := &Request{ID: "r16", InputTokens: make([]int, 16)}
	got16 := ea.computeCost(req16)
	want16 := 16 * (1 + 1.0*1.0*1.0) // multiplier = 1 + 1 = 2
	if math.Abs(got16-want16) > 1e-6 {
		t.Errorf("kappa for 16 tokens (block=16): want cost %v (kappa=1), got %v", want16, got16)
	}
}

// TestEAAwareTokenBucket_AdmitConsumesPressureWeightedTokens verifies
// the on-disk effect: at full pressure, an aggressor's admit drains
// the inner bucket by the EA-weighted cost (not the raw input-token
// count). This is the behavior that throttles aggressor injection.
func TestEAAwareTokenBucket_AdmitConsumesPressureWeightedTokens(t *testing.T) {
	// Bucket capacity 50000, refill negligible during the test window.
	inner := NewTokenBucket(50000, 0.001)
	tracker := &fakeTracker{pressure: 1.0}
	ea := NewEAAwareTokenBucket(inner, tracker, 0.005, 16)

	// Admit one aggressor (8192 tokens). Cost = 8192 × 3.56 = 29163.52.
	// Bucket should drop from 50000 to ~20836.48.
	agg := &Request{ID: "agg", InputTokens: make([]int, 8192), TenantID: "agg"}
	admitted, _ := ea.Admit(agg, &RouterState{Clock: 0})
	if !admitted {
		t.Fatal("first aggressor should be admitted (50000 capacity > 29163 cost)")
	}
	// Try a second aggressor: cost is again ~29163. Bucket has ~20836. Reject.
	admitted2, reason := ea.Admit(agg, &RouterState{Clock: 0})
	if admitted2 {
		t.Fatalf("second aggressor should be rejected (insufficient tokens after EA pricing): admitted=%v reason=%q", admitted2, reason)
	}
	if reason != "insufficient tokens" {
		t.Errorf("expected reason 'insufficient tokens', got %q", reason)
	}
}

// TestEAAwareTokenBucket_CooperatorsNotMaterallyPenalized verifies the
// design intent: at full pressure with default weight=0.005, cooperator
// (256-token, kappa=16) cost is only marginally above base. This is the
// guarantee that EA-aware admission doesn't punish well-behaved tenants
// during a jam.
func TestEAAwareTokenBucket_CooperatorsNotMaterallyPenalized(t *testing.T) {
	inner := NewTokenBucket(10000, 1.0)
	tracker := &fakeTracker{pressure: 1.0}
	ea := NewEAAwareTokenBucket(inner, tracker, 0.005, 16)
	coop := &Request{ID: "coop", InputTokens: make([]int, 256), TenantID: "coop"}

	cost := ea.computeCost(coop)
	// Expected: 256 × (1 + 0.005 × 1 × 16) = 276.48
	if cost > 280 {
		t.Errorf("cooperator cost should be marginally above base (≤280), got %v", cost)
	}
	if cost <= 256 {
		t.Errorf("cooperator cost should reflect pressure multiplier (>256 at full pressure), got %v", cost)
	}
}

// TestEAAwareTokenBucket_InterfaceConformance verifies that the
// decorator implements AdmissionPolicy at compile time (the var
// declaration in admission.go), and that the type can be passed
// where AdmissionPolicy is expected.
func TestEAAwareTokenBucket_InterfaceConformance(t *testing.T) {
	inner := NewTokenBucket(100, 10)
	var policy AdmissionPolicy = NewEAAwareTokenBucket(inner, nil, 0, 16)
	if policy == nil {
		t.Fatal("EAAwareTokenBucket should be usable as AdmissionPolicy")
	}
}
