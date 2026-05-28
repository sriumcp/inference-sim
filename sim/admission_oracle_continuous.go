// iter-4 Theme 3: continuous-rate oracle baseline.
//
// The binary OracleAdmission (iter-3) rejects 100% of aggressors and
// admits 100% of cooperators. iter-3 found EA-aware beats this binary
// oracle by 29% on cooperator TTFT P95 — because admitting *some*
// aggressor traffic at low pressure keeps the KV cache warm.
//
// ContinuousRateOracleAdmission addresses the "fancier oracle" caveat:
// it admits aggressors at a rate determined by current KV pressure.
// At low pressure: admit (warm-up traffic, like EA-aware does).
// At high pressure: throttle (protect cooperators).
//
// Like the binary oracle, it uses tenant identity (not SLO class) — so
// it remains gameability-immune. Unlike the binary oracle, it tries to
// match EA-aware's pressure-proportional admission discipline. The
// research question: does a hand-tuned continuous-rate oracle beat
// EA-aware, or does EA-aware match it?

package sim

import (
	"fmt"
	"math"
	"strings"
)

var _ AdmissionPolicy = (*ContinuousRateOracleAdmission)(nil)

// ContinuousRateOracleAdmission applies smooth pressure-proportional
// admission to known aggressors. Cooperators are always admitted.
//
// Aggressor admission probability:
//
//	if kvUtil <= LowSetpoint:  rate = 1.0   (admit warm-up traffic)
//	if kvUtil >= HighSetpoint: rate = 0.0   (protect cooperators)
//	else:                      linear interpolation
//
// Defaults: LowSetpoint=0.5, HighSetpoint=0.85. These bracket the
// EA-aware shadow-price activation threshold (0.9). The result is a
// continuous-throttle that smooths the binary oracle's all-or-nothing
// rejection.
//
// Like AIMD, admission decisions are deterministic via FNV hash of
// request ID — reproducible across reruns.
type ContinuousRateOracleAdmission struct {
	CooperatorPrefix  string  // tenant ID prefix for cooperators (default "coop")
	AggressorTenantID string  // exact tenant ID for the aggressor (default "aggressor")
	LowSetpoint       float64 // KV util ≤ this: admit fully
	HighSetpoint      float64 // KV util ≥ this: reject fully
}

// NewContinuousRateOracleAdmission constructs a continuous-rate oracle
// with validated parameters. Empty strings substitute defaults
// matching the campaign workload schema. Panics on invalid setpoints.
func NewContinuousRateOracleAdmission(cooperatorPrefix, aggressorID string,
	lowSetpoint, highSetpoint float64) *ContinuousRateOracleAdmission {
	if cooperatorPrefix == "" {
		cooperatorPrefix = "coop"
	}
	if aggressorID == "" {
		aggressorID = "aggressor"
	}
	if lowSetpoint < 0 || lowSetpoint >= 1 || math.IsNaN(lowSetpoint) {
		panic(fmt.Sprintf("NewContinuousRateOracleAdmission: lowSetpoint must be in [0, 1), got %v", lowSetpoint))
	}
	if highSetpoint <= lowSetpoint || highSetpoint > 1 || math.IsNaN(highSetpoint) {
		panic(fmt.Sprintf("NewContinuousRateOracleAdmission: highSetpoint must be in (lowSetpoint, 1], got %v", highSetpoint))
	}
	return &ContinuousRateOracleAdmission{
		CooperatorPrefix:  cooperatorPrefix,
		AggressorTenantID: aggressorID,
		LowSetpoint:       lowSetpoint,
		HighSetpoint:      highSetpoint,
	}
}

// Admit implements AdmissionPolicy. Cooperators always pass. Aggressors
// are admitted with pressure-proportional probability; the gating is
// deterministic per request ID (FNV hash).
func (o *ContinuousRateOracleAdmission) Admit(req *Request, state *RouterState) (bool, string) {
	tid := req.TenantID
	// Cooperator: always admit (oracle's cooperator-protection contract).
	if strings.HasPrefix(tid, o.CooperatorPrefix) {
		return true, ""
	}
	// Unknown tenant: admit (don't punish what you can't classify).
	if tid != o.AggressorTenantID {
		return true, ""
	}

	// Aggressor: pressure-proportional admission.
	kvUtil := kvUtilFromState(state)
	var rate float64
	switch {
	case kvUtil <= o.LowSetpoint:
		rate = 1.0
	case kvUtil >= o.HighSetpoint:
		rate = 0.0
	default:
		// Linear interpolation between low and high setpoints.
		rate = 1.0 - (kvUtil-o.LowSetpoint)/(o.HighSetpoint-o.LowSetpoint)
	}

	if admissionHashUnit(req.ID) < rate {
		return true, ""
	}
	return false, fmt.Sprintf("oracle-cont: kvUtil=%.3f rate=%.3f reject", kvUtil, rate)
}
