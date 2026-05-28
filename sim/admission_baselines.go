// iter-3 baseline admission policies for comparison-breadth study.
//
// Three additional admission baselines evaluated against the EA-aware
// admission family for the SIGMETRICS submission's rigorous-comparisons
// section:
//
//   - AIMDAdmission: Additive-Increase, Multiplicative-Decrease admission
//     rate. Reactive — increases rate linearly under low pressure,
//     decreases multiplicatively when KV pressure crosses a threshold.
//     Classic congestion-avoidance pattern (TCP Reno, etc.) applied to
//     LLM-serving admission.
//
//   - AWTAdmission: Alternative-Window-Time admission. Per-tenant
//     sliding-window rate limiter; admits if the tenant's request
//     count within the trailing window is below a threshold. Does NOT
//     read EA signals — it's a fairness shaper on raw request rate.
//
//   - OracleAdmission: Perfect future-knowledge baseline. Admits all
//     cooperators (tenant_id starts with "coop"), rejects all aggressors
//     (tenant_id == "aggressor"). Conceptually the upper bound: any
//     practical admission policy must approach but cannot beat oracle.
//
// All three integrate via the standard AdmissionPolicy interface and
// participate in the existing decorator stack (TenantBudget,
// FlowControl, etc.).

package sim

import (
	"fmt"
	"math"
	"strings"
)

var _ AdmissionPolicy = (*AIMDAdmission)(nil)
var _ AdmissionPolicy = (*AWTAdmission)(nil)
var _ AdmissionPolicy = (*OracleAdmission)(nil)

// ---------------------------------------------------------------------------
// AIMD: Additive-Increase Multiplicative-Decrease
// ---------------------------------------------------------------------------

// AIMDAdmission gates requests by a probabilistic admission rate that
// increases linearly when KV pressure is low and decreases
// multiplicatively when KV pressure crosses a threshold.
//
// State: currentRate, last update tick. The rate evolves by:
//
//	if KVPressure(state) >= threshold: rate *= decreaseFactor   (e.g. 0.5)
//	else:                              rate += increaseStep     (e.g. 0.05/tick)
//
// Each request is admitted with probability `rate`, deterministically
// derived from a hash of (request ID, currentRate) — *not* a wall-clock
// random source — so admission is reproducible across reruns.
//
// AIMD does NOT read tenant identity or EA signals. It is a pure
// reactive-pressure controller; performance under aggressor workloads
// depends on whether the threshold-crossing happens fast enough.
type AIMDAdmission struct {
	rate            float64 // current admission probability ∈ [rateFloor, 1.0]
	rateFloor       float64 // minimum rate (prevents lock-out)
	increaseStep    float64 // additive increase per microsecond
	decreaseFactor  float64 // multiplicative decrease on overload
	pressureThresh  float64 // KV utilization threshold that triggers MD
	lastUpdateTick  int64
}

// NewAIMDAdmission creates an AIMDAdmission with validated parameters.
//
// Defaults (when constructed with zero values via factory):
//   - rateFloor:       0.05 (5% min admission rate; prevents starvation lock-out)
//   - increaseStep:    1e-6 (per-µs additive growth; reaches 1.0 in ~1s of slack)
//   - decreaseFactor:  0.5  (halve rate on each pressure event)
//   - pressureThresh:  0.9  (matches EA-aware KVPressureSignal threshold)
//
// Panics on invalid params (R3).
func NewAIMDAdmission(rateFloor, increaseStep, decreaseFactor, pressureThresh float64) *AIMDAdmission {
	if rateFloor <= 0 || rateFloor > 1 || math.IsNaN(rateFloor) || math.IsInf(rateFloor, 0) {
		panic(fmt.Sprintf("NewAIMDAdmission: rateFloor must be in (0, 1], got %v", rateFloor))
	}
	if increaseStep <= 0 || math.IsNaN(increaseStep) || math.IsInf(increaseStep, 0) {
		panic(fmt.Sprintf("NewAIMDAdmission: increaseStep must be > 0, got %v", increaseStep))
	}
	if decreaseFactor <= 0 || decreaseFactor >= 1 || math.IsNaN(decreaseFactor) {
		panic(fmt.Sprintf("NewAIMDAdmission: decreaseFactor must be in (0, 1), got %v", decreaseFactor))
	}
	if pressureThresh <= 0 || pressureThresh > 1 || math.IsNaN(pressureThresh) {
		panic(fmt.Sprintf("NewAIMDAdmission: pressureThresh must be in (0, 1], got %v", pressureThresh))
	}
	return &AIMDAdmission{
		rate:           1.0, // start fully open
		rateFloor:      rateFloor,
		increaseStep:   increaseStep,
		decreaseFactor: decreaseFactor,
		pressureThresh: pressureThresh,
	}
}

// kvUtilFromState derives max KV utilization across instances. Mirrors
// what TenantExternalityTracker.KVPressureSignal sees, but is computed
// here from RouterState because AIMD is intentionally tracker-free.
func kvUtilFromState(state *RouterState) float64 {
	maxUtil := 0.0
	for _, s := range state.Snapshots {
		if s.KVUtilization > maxUtil {
			maxUtil = s.KVUtilization
		}
	}
	return maxUtil
}

// admissionHashUnit maps a request ID to a deterministic [0, 1) float.
// FNV-style fold of the string bytes — cheap, stable, reproducible.
func admissionHashUnit(id string) float64 {
	const offset, prime = uint32(2166136261), uint32(16777619)
	h := offset
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= prime
	}
	return float64(h%1_000_000) / 1_000_000.0
}

// Admit implements AdmissionPolicy. Updates rate based on KV pressure
// since the last admit call, then accepts with probability `rate`
// (deterministic-hash gating).
//
// On the first call (lastUpdateTick still at its zero value), elapsed
// is `clock - 0 = clock`. If pressure is below threshold, this drives
// a one-time additive increase that's harmlessly clamped to rate=1.0
// (the initial rate). If pressure is above threshold, rate halves
// regardless of elapsed.
func (a *AIMDAdmission) Admit(req *Request, state *RouterState) (bool, string) {
	clock := state.Clock
	elapsed := clock - a.lastUpdateTick
	if elapsed < 0 {
		elapsed = 0 // guard against backwards clock (shouldn't happen)
	}
	a.lastUpdateTick = clock

	if kvUtilFromState(state) >= a.pressureThresh {
		// Multiplicative decrease — once per Admit call, magnitude
		// independent of elapsed (a single "pressure event" halves the rate).
		a.rate *= a.decreaseFactor
		if a.rate < a.rateFloor {
			a.rate = a.rateFloor
		}
	} else if elapsed > 0 {
		// Additive increase, scaled by elapsed time.
		a.rate += a.increaseStep * float64(elapsed)
		if a.rate > 1.0 {
			a.rate = 1.0
		}
	}

	if admissionHashUnit(req.ID) < a.rate {
		return true, ""
	}
	return false, fmt.Sprintf("aimd: rate=%.3f reject", a.rate)
}

// CurrentRate exposes the controller's internal admission probability
// for inspection (tests, instrumentation).
func (a *AIMDAdmission) CurrentRate() float64 { return a.rate }

// ---------------------------------------------------------------------------
// AWT: Alternative-Window-Time
// ---------------------------------------------------------------------------

// AWTAdmission limits each tenant to a fixed number of admissions
// within a trailing time window. Sliding-window rate limiter, agnostic
// to KV pressure or EA signals.
//
// Per-tenant state: a ring buffer of admission timestamps (in ticks).
// On each Admit call, expired entries (older than window) are dropped;
// admit if remaining count < perTenantBudget.
//
// AWT exposes the limit of "admission control without externality
// awareness" — it can rate-limit fairly across tenants but cannot
// distinguish a tenant whose requests cause large KV harm from one
// whose requests are harmless.
type AWTAdmission struct {
	windowTicks    int64               // trailing window size in microseconds
	perTenantQuota int                 // max admissions per tenant within window
	perTenant      map[string][]int64  // tenant ID → admission timestamps
}

// NewAWTAdmission constructs an AWTAdmission. Panics on invalid params.
//
// Typical defaults: windowTicks=10_000_000 (10s), perTenantQuota=20.
// Calibrated so that a steady cooperator (issuing ~1 req/s) is never
// throttled, but a burst aggressor (issuing 50 reqs/s) is gated.
func NewAWTAdmission(windowTicks int64, perTenantQuota int) *AWTAdmission {
	if windowTicks <= 0 {
		panic(fmt.Sprintf("NewAWTAdmission: windowTicks must be > 0, got %d", windowTicks))
	}
	if perTenantQuota <= 0 {
		panic(fmt.Sprintf("NewAWTAdmission: perTenantQuota must be > 0, got %d", perTenantQuota))
	}
	return &AWTAdmission{
		windowTicks:    windowTicks,
		perTenantQuota: perTenantQuota,
		perTenant:      make(map[string][]int64),
	}
}

// Admit implements AdmissionPolicy. Enforces per-tenant
// rate limit over the trailing window.
func (a *AWTAdmission) Admit(req *Request, state *RouterState) (bool, string) {
	tid := req.TenantID
	if tid == "" {
		// No tenant ID: admit (AWT cannot enforce without a key).
		return true, ""
	}
	clock := state.Clock
	cutoff := clock - a.windowTicks

	// Drop expired admissions.
	stamps := a.perTenant[tid]
	keep := stamps[:0]
	for _, t := range stamps {
		if t >= cutoff {
			keep = append(keep, t)
		}
	}
	a.perTenant[tid] = keep

	if len(keep) >= a.perTenantQuota {
		return false, fmt.Sprintf("awt: tenant=%s window-quota=%d/%d reached",
			tid, len(keep), a.perTenantQuota)
	}
	a.perTenant[tid] = append(keep, clock)
	return true, ""
}

// ---------------------------------------------------------------------------
// Oracle: perfect future-knowledge baseline
// ---------------------------------------------------------------------------

// OracleAdmission is the upper-bound baseline. It uses tenant identity
// directly: admits all cooperators (tenant_id has CooperatorPrefix),
// rejects all aggressors (tenant_id == AggressorTenantID).
//
// Conceptually: "what's the best any admission policy could do, given
// perfect future knowledge of which requests are cooperators and which
// are aggressors?" Practical policies (EA-aware, AIMD, AWT, tier-shed)
// must approach but cannot beat oracle on the headline metric.
//
// The paper's claim "EA-aware closes the gap to oracle" is empirical:
// if EA-aware ≈ oracle on the dishonest workload, EA-aware is
// near-optimal among realistic admission policies.
//
// Oracle is *not* a deployable mechanism — it requires knowledge no
// real serving system has. It is purely an analytical baseline.
type OracleAdmission struct {
	CooperatorPrefix  string // tenant ID prefix for cooperators (default "coop")
	AggressorTenantID string // exact tenant ID for the aggressor (default "aggressor")
}

// NewOracleAdmission constructs an OracleAdmission. Empty arguments
// substitute sensible defaults matching the campaign workload schema.
func NewOracleAdmission(cooperatorPrefix, aggressorID string) *OracleAdmission {
	if cooperatorPrefix == "" {
		cooperatorPrefix = "coop"
	}
	if aggressorID == "" {
		aggressorID = "aggressor"
	}
	return &OracleAdmission{
		CooperatorPrefix:  cooperatorPrefix,
		AggressorTenantID: aggressorID,
	}
}

// Admit implements AdmissionPolicy. Admit cooperators; reject the
// known-aggressor tenant. Tenants matching neither rule are admitted
// (the oracle is conservative for unknown tenants — it doesn't punish
// what it can't classify).
func (o *OracleAdmission) Admit(req *Request, _ *RouterState) (bool, string) {
	tid := req.TenantID
	if tid == o.AggressorTenantID {
		return false, fmt.Sprintf("oracle: tenant=%s is known aggressor", tid)
	}
	if strings.HasPrefix(tid, o.CooperatorPrefix) {
		return true, ""
	}
	// Unknown tenant: admit (don't punish the unrecognized).
	return true, ""
}
