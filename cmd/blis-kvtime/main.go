// cmd/blis-kvtime — experiment runner for the KV-time entitlement scheduling experiment.
//
// This binary runs a single-instance BLIS simulation with any of 7 schedulers,
// measures per-tenant memory-time consumption, TTFT percentiles, and emits JSON
// metrics for analysis.
//
// Usage:
//
//	blis-kvtime --scheduler wfq --seed 42 --duration 600 --warmup 30 \
//	            --workload /path/to/workload.yaml \
//	            --total-kv-blocks 500 \
//	            --output results.json
//
// Supported schedulers: fcfs | wfq | kvtime | decode-token | request-rr | kv-quota | hol-wait
//
// This file is part of the paper-memorytime-mirage experiment (iter-2).
// It does NOT modify any production BLIS files.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
	"github.com/inference-sim/inference-sim/sim/kvtime"
	"github.com/inference-sim/inference-sim/sim/latency"
	"github.com/inference-sim/inference-sim/sim/workload"
)

// ─── Result schema ───────────────────────────────────────────────────────────

// TTFTPercentiles holds TTFT latency percentiles for a tenant (microseconds).
type TTFTPercentiles struct {
	P50Us float64 `json:"p50_us"`
	P95Us float64 `json:"p95_us"`
	P99Us float64 `json:"p99_us"`
}

// TenantMetrics holds per-tenant experiment measurements.
type TenantMetrics struct {
	KVTimeTokenUs     float64          `json:"kv_time_token_us"`      // A_i(T): accumulated memory-time
	MemoryTimeShare   float64          `json:"memory_time_share"`     // s_i = A_i(T) / (K * T_active)
	CompletedRequests int64            `json:"completed_requests"`    // requests completed within horizon (post-warmup)
	VTCCounter        float64          `json:"vtc_counter,omitempty"` // Token-WFQ virtual counter at end
	TTFTPercentiles   *TTFTPercentiles `json:"ttft_percentiles,omitempty"`

	// ─ Coverage / survivorship-bias metrics (added for iter-3 σ sweep) ─
	// Counts requests by their state at simulation horizon, enabling coverage-gap
	// analysis. Closed-loop identity (post-warmup): submitted = completed +
	// in_flight_at_horizon + unserved_at_horizon. Workloads where kv-quota
	// silently rejects admissions show non-zero `unserved_at_horizon` for the
	// affected tenant; KVtime should show ~0.
	SubmittedRequests int64 `json:"submitted_requests"`           // count of requests submitted (post-warmup)
	UnservedAtHorizon int64 `json:"unserved_at_horizon"`          // requests in WaitQ at horizon (never had a first token)
	InFlightAtHorizon int64 `json:"in_flight_at_horizon"`         // requests in RunningBatch at horizon (had first token, didn't complete)
	CompletionRate    float64 `json:"completion_rate"`            // completed / max(1, submitted)

	// ─ Prompt-block distribution (mechanism-level for kv-quota threshold) ─
	// kv-quota rejects admission when (current resident + new req blocks) > ω·K.
	// FracPromptBlocksOverOmegaK is the fraction of submitted prompts whose own
	// block count alone exceeds ω·K; this fraction predicts (mechanism-level)
	// the regime where kv-quota's coverage gap should appear.
	PromptBlocksP50              float64 `json:"prompt_blocks_p50"`
	PromptBlocksP95              float64 `json:"prompt_blocks_p95"`
	PromptBlocksP99              float64 `json:"prompt_blocks_p99"`
	PromptBlocksMax              int64   `json:"prompt_blocks_max"`
	FracPromptBlocksOverOmegaK   float64 `json:"frac_prompt_blocks_over_omega_K"`

	// ─ Censored TTFT (lower bound on unserved requests) ─
	// For each unserved-at-horizon request, lower-bound TTFT = sim_horizon - ArrivalTime.
	// True TTFT is at least this; possibly infinite if request would never have served.
	// Reported as the "maximally generous to kv-quota" robustness column.
	// Combined distribution: served-set TTFTs + per-unserved lower-bound values.
	CensoredTTFTLowerBound *TTFTPercentiles `json:"censored_ttft_lower_bound,omitempty"`

	// ─ thm:service / thm:vector-burst empirical bound test (Step 1A) ─
	// Tests paper §1019(iv,v): a backlogged conformant tenant's under-service
	// is bounded by aggregate competing slack; an over-consuming tenant's
	// excess is bounded by its own bucket slack. Both quantities in token·µs.
	//
	// EntitledTokenUs: ω_i · K · blockSize · T_active — what the tenant was
	// entitled to over the active window, ignoring competition.
	// UnderServiceTokenUs: max(0, EntitledTokenUs − KVTimeTokenUs). Zero
	// iff the tenant met or exceeded its entitlement.
	// AggregateCompetingSlackTokenUs: Σ_{j≠i}(β_j + H_j) · K · blockSize · 1e6
	// + K · blockSize · τ_idle. The sum over OTHER tenants of their bucket
	// depths plus the cache-idle slack term. Emitted only for KVtime/KV-quota
	// (where bucket params are defined).
	// ServiceBoundHolds: UnderServiceTokenUs ≤ AggregateCompetingSlackTokenUs.
	// Refutation event = false here for any conformant tenant.
	// OverConsumptionTokenUs: max(0, KVTimeTokenUs − EntitledTokenUs). Zero
	// iff the tenant stayed within entitlement on average.
	// OwnBucketSlackTokenUs: (β_i + H_i) · K · blockSize · 1e6 — the tenant's
	// own slack ceiling for the thm:vector-burst test (over-consumption side).
	// VectorBurstBoundHolds: OverConsumptionTokenUs ≤ OwnBucketSlackTokenUs.
	EntitledTokenUs               float64 `json:"entitled_token_us,omitempty"`
	UnderServiceTokenUs           float64 `json:"under_service_token_us,omitempty"`
	AggregateCompetingSlackTokenUs float64 `json:"aggregate_competing_slack_token_us,omitempty"`
	ServiceBoundHolds             *bool   `json:"service_bound_holds,omitempty"`
	OverConsumptionTokenUs        float64 `json:"over_consumption_token_us,omitempty"`
	OwnBucketSlackTokenUs         float64 `json:"own_bucket_slack_token_us,omitempty"`
	VectorBurstBoundHolds         *bool   `json:"vector_burst_bound_holds,omitempty"`
}

// ArrivalCurveStats summarises arrival-curve bound violations for KV-time runs.
type ArrivalCurveStats struct {
	WindowSizeS      float64 `json:"window_size_s"`       // sliding window size in seconds
	TotalWindows     int64   `json:"total_windows"`       // number of (tenant, window) pairs tested
	ViolatingWindows int64   `json:"violating_windows"`   // windows exceeding epsilon_disc
	ViolationRate    float64 `json:"violation_rate"`      // violating / total
	MaxViolation     float64 `json:"max_violation"`       // largest violation (token·µs)
	EpsilonDisc      float64 `json:"epsilon_disc"`        // tolerance threshold (token·µs)
}

// RunMetrics is the top-level output JSON.
type RunMetrics struct {
	Scheduler              string                    `json:"scheduler"`
	Seed                   int64                     `json:"seed"`
	DurationS              float64                   `json:"duration_s"`
	WarmupS                float64                   `json:"warmup_s"`
	ActiveDurationUs       int64                     `json:"active_duration_us"` // horizon_us - warmup_us
	TotalKVBlocks          int64                     `json:"total_kv_blocks"`
	TotalKVCapacityTokenUs float64                   `json:"total_kv_capacity_token_us"`
	Tenants                map[string]*TenantMetrics `json:"tenants"`
	MemorytimeShareRatio   float64                   `json:"memorytime_share_ratio"` // max(s_i)/min(s_i)
	ConservationViolations int64                     `json:"conservation_violations"`
	TotalMeterTicks        int64                     `json:"total_meter_ticks"`
	BacklogNonEmptyTicks   int64                     `json:"backlog_nonempty_ticks"`
	TotalCompletedRequests int64                     `json:"total_completed_requests"`
	SimEndedUs             int64                     `json:"sim_ended_us"`
	ArrivalCurve           *ArrivalCurveStats        `json:"arrival_curve,omitempty"` // only for kvtime

	// ─ thm:service slack inputs (Step 1A) ─
	// TauIdleUs: post-warmup µs during which the cache had free capacity AND the
	// wait queue had work (paper §484, thm:service slack term K · τ_idle).
	// IdleFraction: tau_idle_us / active_duration_us — should be small at
	// near-saturation operating points (paper expects τ_idle = o(T)).
	// AnyServiceBoundViolated / AnyVectorBurstViolated: top-level pass/fail
	// flags aggregated across tenants for the empirical theorem tests; only
	// emitted for KVtime/KV-quota where bucket parameters are defined.
	TauIdleUs              int64                     `json:"tau_idle_us"`
	IdleFraction           float64                   `json:"idle_fraction"`
	AnyServiceBoundViolated *bool                    `json:"any_service_bound_violated,omitempty"`
	AnyVectorBurstViolated  *bool                    `json:"any_vector_burst_violated,omitempty"`
}

// ─── TTFT tracker ────────────────────────────────────────────────────────────

// ttftTracker records per-request TTFT (time-to-first-token) in microseconds.
// TTFT = Request.FirstTokenTime - Request.ArrivalTime.
// The simulator sets Request.FirstTokenTime and Request.TTFTSet when the first
// output token is generated. With D=1, TTFT is also the total latency (1 decode step).
type ttftTracker struct {
	// ttfts stores per-tenant slice of TTFT values in µs (after warmup).
	ttfts map[string][]float64
	// warmupUs is the warmup horizon; requests completing before this are excluded.
	warmupUs int64
}

func newTTFTTracker(warmupUs int64) *ttftTracker {
	return &ttftTracker{
		ttfts:    make(map[string][]float64),
		warmupUs: warmupUs,
	}
}

// RecordCompletion computes TTFT from Request.FirstTokenTime and Request.ArrivalTime.
// Uses the BLIS-native TTFTSet / FirstTokenTime fields for exact measurement.
// Requests completing before warmupUs are ignored.
func (t *ttftTracker) RecordCompletion(req *sim.Request, completionUs int64) {
	if completionUs < t.warmupUs {
		return // warmup period, exclude from metrics
	}
	// BA-5 fix: req.FirstTokenTime stores the TTFT duration directly
	// (simulator computes: now + stepAdvance + outputProcessingTime - ArrivalTime).
	// The old guard compared FirstTokenTime vs ArrivalTime, which always excluded
	// requests arriving after t=0 because TTFT << ArrivalTime.
	if !req.TTFTSet || req.FirstTokenTime <= 0 {
		// No first token recorded (e.g. D=0 or timed-out request).
		return
	}
	ttftUs := float64(req.FirstTokenTime) // FirstTokenTime IS the TTFT duration
	t.ttfts[req.TenantID] = append(t.ttfts[req.TenantID], ttftUs)
}

// percentilesFromMixed computes p50/p95/p99 over the union of two TTFT samples:
//   served:    raw TTFT for completed requests (in microseconds)
//   censoredLB: lower-bound TTFT for unserved-at-horizon requests
//              (= horizon − arrival, since true TTFT ≥ this).
//
// This is the "maximally generous to kv-quota" robustness column: we treat
// each unserved request as if it had the most charitable possible TTFT
// (it never got a first token, so true value is larger or infinite). When
// reported alongside the served-set TTFT, the gap between them quantifies
// the survivorship bias the served-set view hides.
func percentilesFromMixed(served []float64, censoredLB []float64) *TTFTPercentiles {
	if len(served) == 0 && len(censoredLB) == 0 {
		return nil
	}
	mixed := make([]float64, 0, len(served)+len(censoredLB))
	mixed = append(mixed, served...)
	mixed = append(mixed, censoredLB...)
	sort.Float64s(mixed)
	pct := func(p float64) float64 {
		if len(mixed) == 0 {
			return 0
		}
		idx := int(p * float64(len(mixed)-1))
		return mixed[idx]
	}
	return &TTFTPercentiles{
		P50Us: pct(0.50),
		P95Us: pct(0.95),
		P99Us: pct(0.99),
	}
}

// Percentiles computes p50, p95, p99 from the recorded TTFT samples for a tenant.
func (t *ttftTracker) Percentiles(tenantID string) *TTFTPercentiles {
	vals := t.ttfts[tenantID]
	if len(vals) == 0 {
		return nil
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	return &TTFTPercentiles{
		P50Us: percentile(sorted, 50),
		P95Us: percentile(sorted, 95),
		P99Us: percentile(sorted, 99),
	}
}

// percentile computes the p-th percentile of a sorted slice using linear interpolation.
func percentile(sorted []float64, p float64) float64 {
	n := float64(len(sorted))
	if n == 0 {
		return 0
	}
	rank := (p / 100.0) * (n - 1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// ─── Per-tenant submission tracker ───────────────────────────────────────────
//
// Counts every request that enters the simulator's WaitQ via either the initial
// arrival injection (gw.Requests) or the session manager's OnComplete-driven
// follow-up generation. Records the per-request input-block count for
// downstream prompt-distribution analysis (mechanism-level prediction of where
// kv-quota's instantaneous cap rejects admissions).
//
// Submissions during the warmup window are EXCLUDED so completion-rate and
// coverage-gap statistics align with the steady-state evaluation window.

type submissionTracker struct {
	// byTenant counts ALL submissions (no warmup filter).
	// Closed-loop identity at horizon: byTenant[t] = completed_total[t] +
	// unserved_at_horizon[t] + in_flight_at_horizon[t]. Filtering submissions
	// by warmup would break this identity (initial-batch requests would not
	// be counted as submitted but their completions land post-warmup).
	byTenant       map[string]int64
	blocksByTenant map[string][]int64 // per-tenant list of input-block counts (full sim)
	blockSize      int64
}

func newSubmissionTracker(blockSize int64) *submissionTracker {
	return &submissionTracker{
		byTenant:       make(map[string]int64),
		blocksByTenant: make(map[string][]int64),
		blockSize:      blockSize,
	}
}

// Record counts a submitted request and stores its input-block count.
// Called from both initial-injection and OnDone-returned paths.
func (s *submissionTracker) Record(req *sim.Request) {
	if req == nil {
		return
	}
	s.byTenant[req.TenantID]++
	// Block count = ceil(prompt_length / block_size). InputTokens is the
	// per-prompt token-id slice; its length is the prompt length.
	inLen := int64(len(req.InputTokens))
	blocks := (inLen + s.blockSize - 1) / s.blockSize
	s.blocksByTenant[req.TenantID] = append(s.blocksByTenant[req.TenantID], blocks)
}

// ─── Per-tenant completion counter ───────────────────────────────────────────

// completionTracker wraps sim.OnRequestDone to count completions per tenant.
type completionTracker struct {
	inner          func(*sim.Request, int64) []*sim.Request
	byTenant       map[string]int64 // post-warmup completions (existing semantic; used by memory-time analysis)
	byTenantTotal  map[string]int64 // ALL completions (full sim); needed for the closed-loop identity
	totalDone      int64

	ttft        *ttftTracker
	requestRR   *kvtime.RequestRRScheduler // nil unless scheduler=request-rr
	warmupUs    int64
}

func newCompletionTracker(inner func(*sim.Request, int64) []*sim.Request, ttft *ttftTracker, warmupUs int64) *completionTracker {
	return &completionTracker{
		inner:         inner,
		byTenant:      make(map[string]int64),
		byTenantTotal: make(map[string]int64),
		ttft:          ttft,
		warmupUs:      warmupUs,
	}
}

func (t *completionTracker) OnDone(req *sim.Request, clock int64) []*sim.Request {
	if req.State == sim.StateCompleted {
		// Always count for the closed-loop identity (submitted = completed_total + unserved + in_flight).
		t.byTenantTotal[req.TenantID]++
		if clock >= t.warmupUs {
			t.byTenant[req.TenantID]++
			t.totalDone++
		}
		// Record TTFT for post-warmup completions.
		if t.ttft != nil {
			t.ttft.RecordCompletion(req, clock)
		}
		// Notify RequestRR of completion.
		if t.requestRR != nil {
			t.requestRR.RecordCompletion(req.TenantID)
		}
	}
	if t.inner != nil {
		return t.inner(req, clock)
	}
	return nil
}

// ─── Metered WFQ/baseline wrapper ────────────────────────────────────────────

// meteredScheduler wraps any InstanceScheduler to also tick the Meter on each
// OrderQueue call.  Used for WFQ, FCFS, decode-token, request-rr, kv-quota,
// and hol-wait baselines (all of which need meter ticks for memory-time measurement
// but don't manage the meter themselves).
//
// Interface forwarding: `inner` is a NAMED field, so Go does not promote the
// inner scheduler's methods (e.g. KVQuotaScheduler.AllowAdmission) onto the
// outer wrapper. Without explicit forwarders, the simulator's type assertions
// at simulator.go:702/712 would silently fail and KV-quota's hard cap would
// not be enforced. The cached `innerAdmit` and `innerPreempt` fields hold the
// once-only result of those type assertions; the AllowAdmission and
// ChooseVictims methods below dispatch through them.
type meteredScheduler struct {
	inner          sim.InstanceScheduler
	innerAdmit     sim.AdmissionAwareScheduler   // nil if inner doesn't implement
	innerPreempt   sim.PreemptionAwareScheduler  // nil if inner doesn't implement
	innerValidator sim.EnqueueValidatorScheduler // nil if inner doesn't implement
	meter          *kvtime.Meter
	kvCache        *kv.KVCacheState
	reqToTenant    map[string]string
	simRef         **sim.Simulator
}

func newMeteredScheduler(inner sim.InstanceScheduler, meter *kvtime.Meter, kvCache *kv.KVCacheState, simRef **sim.Simulator) *meteredScheduler {
	m := &meteredScheduler{
		inner:       inner,
		meter:       meter,
		kvCache:     kvCache,
		reqToTenant: make(map[string]string, 128),
		simRef:      simRef,
	}
	// Cache the interface assertions once at construction. The inner type is
	// fixed for the lifetime of the simulation, so doing this on every
	// AllowAdmission / ChooseVictims call would be wasteful — the wait queue
	// can be long under contention.
	if a, ok := inner.(sim.AdmissionAwareScheduler); ok {
		m.innerAdmit = a
	}
	if p, ok := inner.(sim.PreemptionAwareScheduler); ok {
		m.innerPreempt = p
	}
	if v, ok := inner.(sim.EnqueueValidatorScheduler); ok {
		m.innerValidator = v
	}
	return m
}

func (m *meteredScheduler) OrderQueue(reqs []*sim.Request, clock int64) {
	// Accumulate reqToTenant from wait queue.
	for _, r := range reqs {
		if r.TenantID != "" {
			m.reqToTenant[r.ID] = r.TenantID
		}
	}
	// Include running batch for complete attribution.
	if *m.simRef != nil && (*m.simRef).RunningBatch != nil {
		for _, r := range (*m.simRef).RunningBatch.Requests {
			if r.TenantID != "" {
				m.reqToTenant[r.ID] = r.TenantID
			}
		}
	}
	// Tick meter first.
	m.meter.Tick(m.kvCache, m.reqToTenant, clock)
	// Delegate to inner scheduler for ordering.
	m.inner.OrderQueue(reqs, clock)
}

// AllowAdmission forwards to the inner scheduler if it implements
// AdmissionAwareScheduler; otherwise admits unconditionally (no veto). This
// is what makes inner schedulers like KVQuotaScheduler reach the simulator's
// type-assertion site at simulator.go:702 — without this, the named-field
// wrapper would silently drop the interface (Go promotes methods only from
// embedded fields, not named ones).
//
// Returning true for non-admission-aware inners is behaviorally identical
// to the case where the wrapper does not satisfy the interface at all
// (FormBatch's AdmitFunc would have been nil; the dequeue loop's nil-check
// would skip the callback).
func (m *meteredScheduler) AllowAdmission(req *sim.Request, clock int64) bool {
	if m.innerAdmit != nil {
		return m.innerAdmit.AllowAdmission(req, clock)
	}
	return true
}

// ChooseVictims forwards to the inner scheduler if it implements
// PreemptionAwareScheduler; otherwise returns nil (no eviction proposed).
// Returning nil is behaviorally identical to the case where the wrapper
// does not satisfy the interface at all (FormBatch breaks out of the
// dequeue loop on a nil PreemptFunc).
func (m *meteredScheduler) ChooseVictims(candidate *sim.Request, running []*sim.Request, clock int64) []int {
	if m.innerPreempt != nil {
		return m.innerPreempt.ChooseVictims(candidate, running, clock)
	}
	return nil
}

// IsServeable forwards to the inner scheduler if it implements
// EnqueueValidatorScheduler; otherwise reports the request as serveable
// (no scheduler-level structural rejection). This makes the wrapper
// satisfy sim.EnqueueValidatorScheduler for every wrapped scheduler so
// the type assertion in EnqueueRequest reaches the inner correctly —
// without this forwarder, KVQuotaScheduler.IsServeable would be silently
// dropped by the named-field wrapper (same Go interface-promotion issue
// that motivated the AllowAdmission and ChooseVictims forwarders above).
func (m *meteredScheduler) IsServeable(req *sim.Request) (bool, string) {
	if m.innerValidator != nil {
		return m.innerValidator.IsServeable(req)
	}
	return true, ""
}

// ─── WFQ wrapper (needs SetSimulator) ────────────────────────────────────────

// meteredWFQ wraps WFQScheduler so it also ticks the Meter.
// The WFQScheduler needs SetSimulator which the generic meteredScheduler can't do.
type meteredWFQ struct {
	wfq         *kvtime.WFQScheduler
	meter       *kvtime.Meter
	kvCache     *kv.KVCacheState
	reqToTenant map[string]string
	simRef      **sim.Simulator
}

func newMeteredWFQ(wfq *kvtime.WFQScheduler, meter *kvtime.Meter, kvCache *kv.KVCacheState, simRef **sim.Simulator) *meteredWFQ {
	return &meteredWFQ{
		wfq:         wfq,
		meter:       meter,
		kvCache:     kvCache,
		reqToTenant: make(map[string]string, 128),
		simRef:      simRef,
	}
}

func (m *meteredWFQ) OrderQueue(reqs []*sim.Request, clock int64) {
	for _, r := range reqs {
		if r.TenantID != "" {
			m.reqToTenant[r.ID] = r.TenantID
		}
	}
	if *m.simRef != nil && (*m.simRef).RunningBatch != nil {
		for _, r := range (*m.simRef).RunningBatch.Requests {
			if r.TenantID != "" {
				m.reqToTenant[r.ID] = r.TenantID
			}
		}
	}
	m.meter.Tick(m.kvCache, m.reqToTenant, clock)
	m.wfq.OrderQueue(reqs, clock)
}

// ─── Decode-token wrapper (needs SetSimulator) ───────────────────────────────

type meteredDecodeToken struct {
	inner       *kvtime.DecodeTokenScheduler
	meter       *kvtime.Meter
	kvCache     *kv.KVCacheState
	reqToTenant map[string]string
	simRef      **sim.Simulator
}

func newMeteredDecodeToken(inner *kvtime.DecodeTokenScheduler, meter *kvtime.Meter, kvCache *kv.KVCacheState, simRef **sim.Simulator) *meteredDecodeToken {
	return &meteredDecodeToken{
		inner:       inner,
		meter:       meter,
		kvCache:     kvCache,
		reqToTenant: make(map[string]string, 128),
		simRef:      simRef,
	}
}

func (m *meteredDecodeToken) OrderQueue(reqs []*sim.Request, clock int64) {
	for _, r := range reqs {
		if r.TenantID != "" {
			m.reqToTenant[r.ID] = r.TenantID
		}
	}
	if *m.simRef != nil && (*m.simRef).RunningBatch != nil {
		for _, r := range (*m.simRef).RunningBatch.Requests {
			if r.TenantID != "" {
				m.reqToTenant[r.ID] = r.TenantID
			}
		}
	}
	m.meter.Tick(m.kvCache, m.reqToTenant, clock)
	m.inner.OrderQueue(reqs, clock)
}

// ─── Backlog + τ_idle tracker ────────────────────────────────────────────────
//
// Single ProgressHook that records two quantities per snapshot:
//
//   nonEmptyCount: number of snapshots where any instance had QueueDepth > 0
//                  (existing v1 metric, retained).
//
//   tauIdleUs:     accumulated post-warmup µs during which the cache had free
//                  capacity AND the wait queue had work — the "wasted-cache-
//                  while-work-waits" intervals that thm:service uses as the
//                  slack term K · τ_idle (paper §484, line 1040). Required for
//                  the backlogged-conformant-tenant under-service bound.
//                  Operationalized as point-in-time sampling at the snapshot
//                  interval (1ms in this campaign) — overcounts slightly, which
//                  is the right side of the bound to err on (more slack =
//                  bound is more permissive = harder to falsify).
type backlogTracker struct {
	nonEmptyCount      int64
	tauIdleUs          int64
	warmupUs           int64
	snapshotIntervalUs int64
	prevClockUs        int64 // clock at previous snapshot; 0 = first call

	// τ_idle threshold: cache occupancy below idleThresholdBlocks counts as "idle
	// in the sense of paper §484 (the simulator failed to maintain η·K under work)."
	// idleThresholdBlocks = ⌊η · K⌋. Set at construction.
	idleThresholdBlocks int64
}

func newBacklogTracker(warmupUs, snapshotIntervalUs, idleThresholdBlocks int64) *backlogTracker {
	return &backlogTracker{
		warmupUs:            warmupUs,
		snapshotIntervalUs:  snapshotIntervalUs,
		idleThresholdBlocks: idleThresholdBlocks,
	}
}

func (b *backlogTracker) OnProgress(snap sim.ProgressSnapshot) {
	queueNonEmpty := false
	cacheBelowThreshold := false
	for _, inst := range snap.InstanceSnapshots {
		if inst.QueueDepth > 0 {
			queueNonEmpty = true
		}
		// "Below η·K" — cache used < idleThresholdBlocks. Equivalent to:
		// (KVTotalBlocks - KVFreeBlocks) < idleThresholdBlocks
		// = KVFreeBlocks > KVTotalBlocks - idleThresholdBlocks.
		used := inst.KVTotalBlocks - inst.KVFreeBlocks
		if used < b.idleThresholdBlocks {
			cacheBelowThreshold = true
		}
	}
	if queueNonEmpty {
		b.nonEmptyCount++
	}
	// τ_idle accounting: only post-warmup, only when conditions hold.
	// Use the actual interval since the last snapshot (snap.Clock - prevClock)
	// when available; fall back to the configured interval on the first call.
	if snap.Clock >= b.warmupUs && queueNonEmpty && cacheBelowThreshold {
		// Compute the time interval since the previous snapshot, clamped to the
		// post-warmup window. This avoids attributing pre-warmup time to τ_idle
		// when a step jumps across the warmup boundary. The estimator is a
		// conservative upper bound: when conditions changed within an interval
		// (snapshot fires once per step, intervals can exceed 1ms), the entire
		// interval is attributed to τ_idle. Over-attribution is the right side
		// to err on for the paper's bound (slack = K · τ_idle).
		startUs := b.prevClockUs
		if startUs < b.warmupUs {
			startUs = b.warmupUs
		}
		var dt int64
		if snap.Clock > startUs {
			dt = snap.Clock - startUs
		} else {
			dt = b.snapshotIntervalUs
		}
		b.tauIdleUs += dt
	}
	b.prevClockUs = snap.Clock
}

// ─── KV-time scheduler wrapper with per-tick arrival-curve logging (BA-6 fix) ──

// kvtimeWithAcLog wraps GreedyKVScheduler to call acLogger.record() at every
// OrderQueue invocation.  BA-6 fix: the original code only called record() once
// at simulation end, leaving total_windows=0 in arrival-curve stats.
//
// The wrapper also forwards AdmissionAwareScheduler so the simulator's
// type-assertion wires the admission veto correctly.
type kvtimeWithAcLog struct {
	inner    *kvtime.GreedyKVScheduler
	meter    *kvtime.Meter
	acLogger *arrivalCurveLogger
}

// OrderQueue delegates to the inner scheduler and records the KV-time snapshot.
func (w *kvtimeWithAcLog) OrderQueue(reqs []*sim.Request, clock int64) {
	w.inner.OrderQueue(reqs, clock)
	// BA-6 fix: record per-tick snapshot so sliding-window analysis has data.
	w.acLogger.record(clock, w.meter.TenantKVTime())
}

// AllowAdmission forwards to the inner scheduler's admission veto.
// This ensures the type-assertion in simulator.go:702 continues to work.
func (w *kvtimeWithAcLog) AllowAdmission(req *sim.Request, clock int64) bool {
	return w.inner.AllowAdmission(req, clock)
}

// ChooseVictims forwards to the inner scheduler's paper §6 line 644
// density-ordered eviction. Required so the simulator's type assertion at
// simulator.go:712 finds PreemptionAwareScheduler on the outer wrapper.
func (w *kvtimeWithAcLog) ChooseVictims(candidate *sim.Request, running []*sim.Request, clock int64) []int {
	return w.inner.ChooseVictims(candidate, running, clock)
}

// ─── Arrival-curve violation analysis ────────────────────────────────────────

// ArrivalPoint records a per-tenant KV-time snapshot at a given simulation time.
type ArrivalPoint struct {
	TimeUs   int64
	TenantID string
	CumKVUs  float64 // cumulative A_i(t) in token·µs
}

// arrivalCurveLogger records per-tick KV-time snapshots for arrival-curve analysis.
type arrivalCurveLogger struct {
	points []ArrivalPoint
}

func (a *arrivalCurveLogger) record(timeUs int64, tenantKVTime map[string]float64) {
	for tenant, kv := range tenantKVTime {
		a.points = append(a.points, ArrivalPoint{
			TimeUs:   timeUs,
			TenantID: tenant,
			CumKVUs:  kv,
		})
	}
}

// computeArrivalCurveStats checks the arrival-curve bound:
//
//	A_i(t2) - A_i(t1) <= omega_i * K * (t2 - t1) + B_i^max + H_i + epsilon_disc
//
// for all 30-second sliding windows.
func computeArrivalCurveStats(
	points []ArrivalPoint,
	omegaI float64,
	totalKVBlocksInt int64,
	blockSizeTokens int64,
	betaSeconds float64,
	warmupUs int64,
	windowUs int64,
) *ArrivalCurveStats {
	K := float64(totalKVBlocksInt) * float64(blockSizeTokens) // tokens
	// epsilon_disc ≈ one block's worth of tokens × max tick interval (conservative)
	// max tick interval ≈ 200ms = 200000µs; one block = 16 tokens
	// epsilon_disc = 16 * 200000 = 3,200,000 token·µs
	epsilonDisc := float64(blockSizeTokens) * 200000.0
	bMax := betaSeconds * K * 1e6 // B_i^max in token·µs
	hUs := 0.0                    // H_i=0 for this experiment

	// Build per-tenant time series (post-warmup only).
	type ts struct {
		times []int64
		kvs   []float64
	}
	tenantTS := make(map[string]*ts)
	for _, p := range points {
		if p.TimeUs < warmupUs {
			continue
		}
		t, ok := tenantTS[p.TenantID]
		if !ok {
			t = &ts{}
			tenantTS[p.TenantID] = t
		}
		t.times = append(t.times, p.TimeUs)
		t.kvs = append(t.kvs, p.CumKVUs)
	}

	var totalWindows, violating int64
	var maxViol float64

	for _, t := range tenantTS {
		n := len(t.times)
		if n < 2 {
			continue
		}
		// For each t1, find the latest t2 such that t2 - t1 <= windowUs.
		j := 0
		for i := 0; i < n; i++ {
			// Advance j to the furthest point within the window.
			for j < n-1 && t.times[j+1]-t.times[i] <= windowUs {
				j++
			}
			if j <= i {
				continue
			}
			totalWindows++
			t1, t2 := t.times[i], t.times[j]
			kv1, kv2 := t.kvs[i], t.kvs[j]
			deltaT := float64(t2 - t1)
			bound := omegaI*K*deltaT + bMax + hUs + epsilonDisc
			actual := kv2 - kv1
			if actual > bound {
				violating++
				viol := actual - bound
				if viol > maxViol {
					maxViol = viol
				}
			}
		}
	}

	var violRate float64
	if totalWindows > 0 {
		violRate = float64(violating) / float64(totalWindows)
	}

	return &ArrivalCurveStats{
		WindowSizeS:      float64(windowUs) / 1e6,
		TotalWindows:     totalWindows,
		ViolatingWindows: violating,
		ViolationRate:    violRate,
		MaxViolation:     maxViol,
		EpsilonDisc:      epsilonDisc,
	}
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	schedulerFlag   := flag.String("scheduler", "wfq", "scheduler: fcfs | wfq | kvtime | decode-token | request-rr | kv-quota | hol-wait")
	seedFlag        := flag.Int64("seed", 42, "RNG seed")
	durationFlag    := flag.Float64("duration", 600.0, "simulation duration in seconds")
	warmupFlag      := flag.Float64("warmup", 30.0, "warmup period in seconds (excluded from metrics)")
	workloadFlag    := flag.String("workload", "", "path to workload YAML spec")
	outputFlag      := flag.String("output", "", "path for JSON metrics output (stdout if empty)")
	totalKVBlocksF  := flag.Int64("total-kv-blocks", 500, "total KV cache blocks (default 500 for iter-2 operative regime)")
	omegaFlag       := flag.Float64("omega", 0.45, "KV-time bucket entitlement fraction (0 < omega < 1) — applies to all tenants unless per-tenant flags override")
	omegaAFlag      := flag.Float64("omega-a", -1.0, "KV-time entitlement for tenantA (overrides --omega when >= 0)")
	omegaBFlag      := flag.Float64("omega-b", -1.0, "KV-time entitlement for tenantB (overrides --omega when >= 0)")
	betaSecondsFlag := flag.Float64("beta-seconds", 1.0, "KV-time bucket depth in seconds (larger = deeper buffer)")
	hSecondsFlag    := flag.Float64("h-seconds", 0.0, "KV-time bucket overdraft floor in seconds (0 = no overdraft)")
	latencyBackend  := flag.String("latency-backend", "roofline", "latency model: roofline (analytical) | trained-physics (fit from profiled microbenchmarks; paper §1017)")
	flag.Parse()

	if *workloadFlag == "" {
		fmt.Fprintf(os.Stderr, "error: --workload is required\n")
		os.Exit(1)
	}

	horizonUs        := int64(*durationFlag * 1e6)
	warmupUs         := int64(*warmupFlag * 1e6)
	activeDurationUs := horizonUs - warmupUs
	totalKVBlocks    := *totalKVBlocksF
	const blockSizeTokens = 16

	// ── Hardware config: H100 SXM5 ──
	hwCfg := sim.HardwareCalib{
		TFlopsPeak: 989.5,
		TFlopsFP8:  1979.0,
		BwPeakTBs:  3.35,
		MfuPrefill: 0.45,
		MfuDecode:  0.30,
		MemoryGiB:  80.0,
	}

	// ── Model config: LLaMA 3.1 8B (BF16) ──
	modelCfg := sim.ModelConfig{
		NumLayers:       32,
		HiddenDim:       4096,
		NumHeads:        32,
		NumKVHeads:      8,
		VocabSize:       128256,
		BytesPerParam:   2.0, // bfloat16
		IntermediateDim: 14336,
	}

	// ── KV cache config ──
	kvCfg := sim.NewKVCacheConfig(totalKVBlocks, blockSizeTokens, 0, 0, 0, 0)

	// ── KV store ──
	kvStoreIface := kv.NewKVStore(kvCfg)
	kvCache, ok := kvStoreIface.(*kv.KVCacheState)
	if !ok {
		fmt.Fprintf(os.Stderr, "error: expected *kv.KVCacheState, got %T\n", kvStoreIface)
		os.Exit(1)
	}

	// ── Latency model ──
	// Two backends:
	//   "roofline":         analytical FLOPs/bandwidth model. Used by v1 of this
	//                       campaign. All coefficients zero (pure roofline; no
	//                       fitted corrections).
	//   "trained-physics":  physics-informed roofline + fitted corrections from
	//                       profiled microbenchmarks (BLIS defaults.yaml,
	//                       iter-29 fit, loss 34.57%). This is what paper §1017
	//                       commits to ("step-latency tables fit from profiled
	//                       microbenchmarks ... so throughput degrades
	//                       realistically with batch size and context length").
	//                       Use this for paper figures.
	var latencyCoeffs sim.LatencyCoeffs
	switch *latencyBackend {
	case "roofline":
		// NewLatencyCoeffs(betaCoeffs, alphaCoeffs) — betas first per sim/config.go.
		latencyCoeffs = sim.NewLatencyCoeffs(
			[]float64{0.0, 0.0, 0.0}, // betas
			[]float64{0.0, 0.0, 0.0}, // alphas
		)
	case "trained-physics":
		// Llama-3.1-8B-Instruct / H100 / TP=1 / vLLM v0.11.0
		// Source: inference-sim/defaults.yaml (alpha_coeffs, beta_coeffs).
		// Keep in sync if defaults.yaml is regenerated.
		// Argument order: NewLatencyCoeffs(betaCoeffs, alphaCoeffs).
		latencyCoeffs = sim.NewLatencyCoeffs(
			// β₁..β₁₀: prefill/decode compute corrections + per-layer/per-step/per-MoE overheads
			[]float64{0.152128, 0.0, 1.36252915, 0.752037, 32.09546717, 4.41684444, 126.024825, 481.8613888, 0.0, 1.94710771},
			// α₁..α₃: pre-scheduling, post-decode, output-token-streaming overheads
			[]float64{15563.199579, 777.3455, 45.907545},
		)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --latency-backend %q (valid: roofline, trained-physics)\n", *latencyBackend)
		os.Exit(1)
	}
	hwModelCfg := sim.NewModelHardwareConfig(modelCfg, hwCfg,
		"meta-llama/llama-3.1-8b-instruct", "H100", 1, *latencyBackend, 16384)
	latencyModel, err := latency.NewLatencyModel(latencyCoeffs, hwModelCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating latency model: %v\n", err)
		os.Exit(1)
	}

	// ── Load workload spec ──
	wlData, err := os.ReadFile(*workloadFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading workload: %v\n", err)
		os.Exit(1)
	}
	var wlSpec workload.WorkloadSpec
	if err := yaml.Unmarshal(wlData, &wlSpec); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing workload YAML: %v\n", err)
		os.Exit(1)
	}
	wlSpec.Seed = *seedFlag

	// ── Generate workload ──
	gw, err := workload.GenerateWorkload(&wlSpec, horizonUs, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating workload: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[blis-kvtime] scheduler=%s seed=%d total_kv_blocks=%d initial_requests=%d sessions=%d\n",
		*schedulerFlag, *seedFlag, totalKVBlocks, len(gw.Requests), len(gw.Sessions))

	// ── Session manager ──
	sessionMgr := workload.NewSessionManager(gw.Sessions)

	// ── KV-time meter ──
	meter := kvtime.NewMeter(blockSizeTokens)

	// ── TTFT tracker ──
	ttft := newTTFTTracker(warmupUs)

	// ── Arrival-curve logger (only for kvtime) ──
	var acLogger *arrivalCurveLogger

	// ── Create scheduler ──
	// Two-phase init: scheduler → simulator → wire back.
	var schedulerInst sim.InstanceScheduler
	var simPtr *sim.Simulator
	var wfqSched *kvtime.WFQScheduler
	var gkvSched *kvtime.GreedyKVScheduler
	var decodeTokenSched *kvtime.DecodeTokenScheduler
	var requestRRSched *kvtime.RequestRRScheduler
	var kvQuotaSched *kvtime.KVQuotaScheduler

	// KV-time bucket params (used for kvtime and kv-quota).
	// Per-tenant omega: use --omega-a / --omega-b when set (>= 0), else fall back to --omega.
	omegaI      := *omegaFlag
	omegaA      := omegaI
	omegaB      := omegaI
	if *omegaAFlag >= 0 {
		omegaA = *omegaAFlag
	}
	if *omegaBFlag >= 0 {
		omegaB = *omegaBFlag
	}
	betaSeconds := *betaSecondsFlag
	hSeconds    := *hSecondsFlag
	const eta   = 0.9

	switch *schedulerFlag {
	case "fcfs":
		fcfs := &kvtime.FCFSScheduler{}
		schedulerInst = newMeteredScheduler(fcfs, meter, kvCache, &simPtr)

	case "wfq":
		wfqSched = kvtime.NewWFQScheduler()
		schedulerInst = newMeteredWFQ(wfqSched, meter, kvCache, &simPtr)

	case "kvtime":
		bucketCfgs := map[string]kvtime.TenantBucketConfig{
			"tenantA": {OmegaI: omegaA, BetaSeconds: betaSeconds, HSeconds: hSeconds},
			"tenantB": {OmegaI: omegaB, BetaSeconds: betaSeconds, HSeconds: hSeconds},
		}
		buckets := kvtime.NewBucketManager(totalKVBlocks, blockSizeTokens, bucketCfgs)
		gkvSched = kvtime.NewGreedyKVScheduler(kvCache, meter, buckets)
		acLogger = &arrivalCurveLogger{}
		// BA-6 fix: wrap gkvSched so acLogger is called at every OrderQueue tick.
		schedulerInst = &kvtimeWithAcLog{inner: gkvSched, meter: meter, acLogger: acLogger}

	case "decode-token":
		decodeTokenSched = kvtime.NewDecodeTokenScheduler()
		schedulerInst = newMeteredDecodeToken(decodeTokenSched, meter, kvCache, &simPtr)

	case "request-rr":
		requestRRSched = kvtime.NewRequestRRScheduler()
		schedulerInst = newMeteredScheduler(requestRRSched, meter, kvCache, &simPtr)

	case "kv-quota":
		tenantOmega := map[string]float64{
			"tenantA": omegaA,
			"tenantB": omegaB,
		}
		kvQuotaSched = kvtime.NewKVQuotaScheduler(kvCache, tenantOmega, totalKVBlocks)
		schedulerInst = newMeteredScheduler(kvQuotaSched, meter, kvCache, &simPtr)

	case "hol-wait":
		holWait := kvtime.NewHOLWaitScheduler()
		schedulerInst = newMeteredScheduler(holWait, meter, kvCache, &simPtr)

	default:
		fmt.Fprintf(os.Stderr, "error: unknown scheduler %q; valid: fcfs|wfq|kvtime|decode-token|request-rr|kv-quota|hol-wait\n", *schedulerFlag)
		os.Exit(1)
	}

	// ── SimConfig ──
	simCfg := sim.SimConfig{
		Horizon: horizonUs,
		Seed:    *seedFlag,
		KVCacheConfig: kvCfg,
		BatchConfig: sim.NewBatchConfig(
			256,   // max running reqs
			32768, // max scheduled tokens (campaign locked = 32768; comfortable for full A-prefill + decodes)
			0,     // long prefill threshold (disabled)
		),
		LatencyCoeffs:       latencyCoeffs,
		ModelHardwareConfig: hwModelCfg,
		PolicyConfig:        sim.NewPolicyConfig("fcfs", "fcfs"),
	}

	// ── Create simulator ──
	simulator, err := sim.NewSimulatorWithScheduler(simCfg, kvStoreIface, latencyModel, schedulerInst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating simulator: %v\n", err)
		os.Exit(1)
	}
	simPtr = simulator

	// ── Wire scheduler back-references ──
	if wfqSched != nil {
		wfqSched.SetSimulator(simulator)
	}
	if gkvSched != nil {
		gkvSched.SetSimulator(simulator)
	}
	if decodeTokenSched != nil {
		decodeTokenSched.SetSimulator(simulator)
	}
	if kvQuotaSched != nil {
		kvQuotaSched.SetSimulator(simulator)
	}

	// ── Submission tracker ──
	// Counts per-tenant submissions (initial injections + session-mgr replenishments)
	// and records each request's input-block count for downstream coverage analysis.
	submissionsT := newSubmissionTracker(blockSizeTokens)

	// ── Completion tracker ──
	// Wraps sessionMgr.OnComplete so we record each follow-up request as a new
	// submission before it enters the simulator's WaitQ.
	tracker := newCompletionTracker(func(req *sim.Request, clock int64) []*sim.Request {
		newReqs := sessionMgr.OnComplete(req, clock)
		for _, r := range newReqs {
			submissionsT.Record(r)
		}
		return newReqs
	}, ttft, warmupUs)
	if requestRRSched != nil {
		tracker.requestRR = requestRRSched
	}
	simulator.OnRequestDone = tracker.OnDone

	// ── Backlog + τ_idle tracker ──
	// τ_idle threshold = η · K. Paper §484 defines τ_idle as time during which
	// the work-conserving placement fails to maintain ηC under eligible demand;
	// operationalized here as "cache_used < η·K AND wait queue non-empty".
	// η is hardcoded at 0.9 to match the campaign's eta=0.9 (Σω_j ≤ η constraint).
	const snapshotIntervalUs = int64(1000) // 1 ms
	idleThresholdBlocks := int64(eta * float64(totalKVBlocks))
	backlog := newBacklogTracker(warmupUs, snapshotIntervalUs, idleThresholdBlocks)
	simulator.SetProgressHook(backlog, snapshotIntervalUs)

	// ── Inject initial requests + record as submissions ──
	for _, req := range gw.Requests {
		submissionsT.Record(req)
		simulator.InjectArrival(req)
	}

	// ── Run simulation ──
	fmt.Fprintf(os.Stderr, "[blis-kvtime] running simulation (horizon=%dµs, warmup=%dµs)...\n", horizonUs, warmupUs)
	simulator.Run()
	simulator.Finalize()
	fmt.Fprintf(os.Stderr, "[blis-kvtime] simulation complete: clock=%dµs completed=%d\n",
		simulator.Clock, tracker.totalDone)

	// ── Arrival-curve log flush (kvtime only) ──
	if acLogger != nil && gkvSched != nil {
		// Record final KV-time snapshot.
		kvTimes := meter.TenantKVTime()
		acLogger.record(simulator.Clock, kvTimes)
	}

	// ── Compute per-tenant KV-time shares ──
	totalCapTokenUs := float64(totalKVBlocks) * float64(blockSizeTokens) * float64(activeDurationUs)
	kvTimes := meter.TenantKVTime()

	// ── Compute coverage / survivorship-bias metrics ──
	// At horizon: walk WaitQ (unserved — never had first token) and RunningBatch
	// (in-flight — had first token, didn't complete) to count per tenant.
	unservedAtHorizon := make(map[string]int64)
	unservedTTFTLowerBoundUs := make(map[string][]float64)
	for _, r := range simulator.WaitQ.Items() {
		if r == nil {
			continue
		}
		unservedAtHorizon[r.TenantID]++
		// Lower-bound TTFT for unserved: time from arrival until horizon (true TTFT ≥ this).
		lb := float64(simulator.Clock - r.ArrivalTime)
		if lb < 0 {
			lb = 0
		}
		unservedTTFTLowerBoundUs[r.TenantID] = append(unservedTTFTLowerBoundUs[r.TenantID], lb)
	}
	inFlightAtHorizon := make(map[string]int64)
	if simulator.RunningBatch != nil {
		for _, r := range simulator.RunningBatch.Requests {
			if r == nil {
				continue
			}
			inFlightAtHorizon[r.TenantID]++
		}
	}

	// Compute prompt-block stats per tenant (mechanism-level: predicts where
	// kv-quota's instantaneous cap rejects single-prompt admissions).
	omegaK := omegaI * float64(totalKVBlocks) // per-tenant block cap (assumes ω_A = ω_B currently)
	promptStats := make(map[string]struct {
		p50, p95, p99 float64
		max           int64
		fracOver      float64
	})
	for tenant, blocks := range submissionsT.blocksByTenant {
		if len(blocks) == 0 {
			continue
		}
		sortedB := make([]int64, len(blocks))
		copy(sortedB, blocks)
		sort.Slice(sortedB, func(i, j int) bool { return sortedB[i] < sortedB[j] })
		pct := func(p float64) float64 {
			if len(sortedB) == 0 {
				return 0
			}
			idx := int(p * float64(len(sortedB)-1))
			return float64(sortedB[idx])
		}
		var maxB int64
		var overCount int64
		for _, b := range sortedB {
			if b > maxB {
				maxB = b
			}
			if float64(b) > omegaK {
				overCount++
			}
		}
		promptStats[tenant] = struct {
			p50, p95, p99 float64
			max           int64
			fracOver      float64
		}{
			p50: pct(0.50), p95: pct(0.95), p99: pct(0.99),
			max:      maxB,
			fracOver: float64(overCount) / float64(len(sortedB)),
		}
	}

	tenantResults := make(map[string]*TenantMetrics)
	// Build the union of tenants seen in kvTimes, submissions, and completions
	// so we don't drop any (e.g., tenant that submitted but didn't complete).
	allTenants := make(map[string]struct{})
	for t := range kvTimes {
		allTenants[t] = struct{}{}
	}
	for t := range submissionsT.byTenant {
		allTenants[t] = struct{}{}
	}
	for t := range tracker.byTenantTotal {
		allTenants[t] = struct{}{}
	}

	for tenant := range allTenants {
		submitted := submissionsT.byTenant[tenant]
		completedTotal := tracker.byTenantTotal[tenant]
		completedPostWarmup := tracker.byTenant[tenant]
		// Closed-loop identity: submitted = completed_total + unserved + in_flight
		// (Coverage rate uses completed_TOTAL because it answers "of all submitted
		//  requests, what fraction got served at all?")
		var compRate float64
		if submitted > 0 {
			compRate = float64(completedTotal) / float64(submitted)
		}
		ps := promptStats[tenant]

		mt := &TenantMetrics{
			KVTimeTokenUs:              kvTimes[tenant],
			MemoryTimeShare:            kvTimes[tenant] / totalCapTokenUs,
			CompletedRequests:          completedPostWarmup, // post-warmup (existing semantic)
			TTFTPercentiles:            ttft.Percentiles(tenant),
			SubmittedRequests:          submitted,
			UnservedAtHorizon:          unservedAtHorizon[tenant],
			InFlightAtHorizon:          inFlightAtHorizon[tenant],
			CompletionRate:             compRate, // = completed_total / submitted
			PromptBlocksP50:            ps.p50,
			PromptBlocksP95:            ps.p95,
			PromptBlocksP99:            ps.p99,
			PromptBlocksMax:            ps.max,
			FracPromptBlocksOverOmegaK: ps.fracOver,
			CensoredTTFTLowerBound:     percentilesFromMixed(ttft.ttfts[tenant], unservedTTFTLowerBoundUs[tenant]),
		}
		_ = completedTotal // value already used in compRate; silence linters
		tenantResults[tenant] = mt
	}

	// Attach VTC counters for WFQ condition.
	if wfqSched != nil {
		for tenant, counter := range wfqSched.TenantCounters() {
			if tm, ok := tenantResults[tenant]; ok {
				tm.VTCCounter = counter
			}
		}
	}

	// ── Compute fairness ratio ──
	var maxShare, minShare float64
	minShare = math.MaxFloat64
	for _, tm := range tenantResults {
		if tm.MemoryTimeShare > maxShare {
			maxShare = tm.MemoryTimeShare
		}
		if tm.MemoryTimeShare < minShare {
			minShare = tm.MemoryTimeShare
		}
	}
	var ratio float64
	if minShare > 0 {
		ratio = maxShare / minShare
	}

	// ── Arrival-curve stats (kvtime only) ──
	var acStats *ArrivalCurveStats
	if acLogger != nil {
		// Record per-tick snapshots — we piggy-back on the kvtime logger populated
		// by recording the final state only (the bucket already handles per-tick reconciliation).
		// For a fuller picture, we record the final cumulative snapshot above.
		// The arrival-curve analysis uses the cumulative A_i(T) from the meter
		// as a single-window check: A_i(T) - A_i(0) over the full active window.
		windowUs := int64(30 * 1e6) // 30-second window
		K := float64(totalKVBlocks) * float64(blockSizeTokens)
		bMax := betaSeconds * K * 1e6
		epsilonDisc := float64(blockSizeTokens) * 200000.0

		// Single-window check: A_i(T) - 0 <= omegaI * K * T_active + bMax + H + epsilonDisc
		T := float64(activeDurationUs)
		var violating, total int64
		var maxViol float64
		for _, t := range kvTimes {
			total++
			bound := omegaI*K*T + bMax + 0.0 + epsilonDisc
			if t > bound {
				violating++
				v := t - bound
				if v > maxViol {
					maxViol = v
				}
			}
		}
		// Also do sliding-window analysis using logged points if available.
		if len(acLogger.points) > 0 {
			acStats = computeArrivalCurveStats(
				acLogger.points,
				omegaI,
				totalKVBlocks,
				blockSizeTokens,
				betaSeconds,
				warmupUs,
				windowUs,
			)
		} else {
			var violRate float64
			if total > 0 {
				violRate = float64(violating) / float64(total)
			}
			acStats = &ArrivalCurveStats{
				WindowSizeS:      float64(activeDurationUs) / 1e6,
				TotalWindows:     total,
				ViolatingWindows: violating,
				ViolationRate:    violRate,
				MaxViolation:     maxViol,
				EpsilonDisc:      epsilonDisc,
			}
		}
		_ = bMax // used in stats above
	}

	// ── thm:service / thm:vector-burst empirical bound test (Step 1A) ──
	//
	// Two-sided bound from paper §1019(iv,v):
	//   (over) over_consumption_i  ≤ own_bucket_slack_i      (thm:vector-burst)
	//   (under) under_service_i    ≤ aggregate_competing_slack_i   (thm:service)
	//
	// Units throughout: token·µs. K is in blocks; multiply by blockSize for tokens;
	// β and H are in seconds; multiply by 1e6 for µs. Bucket params β=betaSeconds,
	// H=hSeconds are global (per-tenant equal in this campaign); for asymmetric
	// per-tenant β/H this code generalises straightforwardly.
	//
	// Only emitted when the active scheduler is residency-aware (kvtime, kv-quota);
	// the projection-meter schedulers (VTC/FCFS/etc.) lack a bucket structure so
	// these bounds are not theoretically applicable to them.
	K := float64(totalKVBlocks) * float64(blockSizeTokens) // K in tokens
	// τ_idle estimator: backlog tracker accumulates inter-snapshot intervals
	// during which the cache had free capacity AND the wait queue had work.
	// Because snapshots fire once per simulator step (not at exact 1ms ticks),
	// inter-snapshot intervals can exceed the snapshot interval; conditions may
	// have varied within an interval. The estimator over-attributes in those
	// transitions — yielding a CONSERVATIVE UPPER BOUND on τ_idle. This is the
	// right side to err on for the paper's bound (K · τ_idle is a slack term;
	// over-estimating slack makes the bound MORE permissive, not less). Cap at
	// active_duration_us so the reported scalar `idle_fraction` is well-defined
	// in [0, 1]; the underlying token-µs slack uses the capped value.
	tauIdleUs := backlog.tauIdleUs
	if tauIdleUs > activeDurationUs {
		tauIdleUs = activeDurationUs
	}
	idleFraction := 0.0
	if activeDurationUs > 0 {
		idleFraction = float64(tauIdleUs) / float64(activeDurationUs)
	}
	residencyAware := *schedulerFlag == "kvtime" || *schedulerFlag == "kv-quota"
	var anyServiceViolated, anyVectorBurstViolated *bool
	if residencyAware {
		// Per-tenant bucket params: ω = omegaA / omegaB; β/H are global flags.
		// (This campaign uses equal β, H across tenants; the framework supports
		// per-tenant β_j, H_j by extending betaForTenant() below.)
		betaForTenant := func(_ string) float64 { return betaSeconds }
		hForTenant := func(_ string) float64 { return hSeconds }
		omegaForTenant := func(t string) float64 {
			switch t {
			case "tenantA":
				return omegaA
			case "tenantB":
				return omegaB
			}
			return omegaI
		}

		// Aggregate competing slack contribution: Σ_j (β_j + H_j) · K — does NOT
		// depend on tenant i; computed once.
		var sumOtherSlackPerJ float64
		for tenant := range tenantResults {
			sumOtherSlackPerJ += (betaForTenant(tenant) + hForTenant(tenant)) * K * 1e6
		}
		// Cache-idle slack term: K · τ_idle (in token·µs).
		tauIdleSlack := K * float64(tauIdleUs)

		serviceViolated := false
		vectorBurstViolated := false
		for tenant, mt := range tenantResults {
			beta_i := betaForTenant(tenant)
			h_i := hForTenant(tenant)
			ownSlack := (beta_i + h_i) * K * 1e6 // token·µs

			entitled := omegaForTenant(tenant) * K * float64(activeDurationUs)
			realized := mt.KVTimeTokenUs

			under := entitled - realized
			if under < 0 {
				under = 0
			}
			over := realized - entitled
			if over < 0 {
				over = 0
			}

			// Aggregate competing slack: Σ_{j≠i}(β_j + H_j) · K + K · τ_idle.
			compSlack := sumOtherSlackPerJ - ownSlack + tauIdleSlack

			serviceHolds := under <= compSlack
			vectorBurstHolds := over <= ownSlack

			mt.EntitledTokenUs = entitled
			mt.UnderServiceTokenUs = under
			mt.AggregateCompetingSlackTokenUs = compSlack
			mt.ServiceBoundHolds = &serviceHolds
			mt.OverConsumptionTokenUs = over
			mt.OwnBucketSlackTokenUs = ownSlack
			mt.VectorBurstBoundHolds = &vectorBurstHolds

			if !serviceHolds {
				serviceViolated = true
			}
			if !vectorBurstHolds {
				vectorBurstViolated = true
			}
		}
		anyServiceViolated = &serviceViolated
		anyVectorBurstViolated = &vectorBurstViolated
	}

	// ── Assemble output ──
	result := &RunMetrics{
		Scheduler:              *schedulerFlag,
		Seed:                   *seedFlag,
		DurationS:              *durationFlag,
		WarmupS:                *warmupFlag,
		ActiveDurationUs:       activeDurationUs,
		TotalKVBlocks:          totalKVBlocks,
		TotalKVCapacityTokenUs: totalCapTokenUs,
		Tenants:                tenantResults,
		MemorytimeShareRatio:   ratio,
		ConservationViolations: meter.ConservationViolations(),
		TotalMeterTicks:        meter.TotalTicks(),
		BacklogNonEmptyTicks:   backlog.nonEmptyCount,
		TotalCompletedRequests: tracker.totalDone,
		SimEndedUs:             simulator.Clock,
		ArrivalCurve:           acStats,
		TauIdleUs:              tauIdleUs,
		IdleFraction:           idleFraction,
		AnyServiceBoundViolated:  anyServiceViolated,
		AnyVectorBurstViolated:   anyVectorBurstViolated,
	}

	// ── Write output ──
	jsonBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling output: %v\n", err)
		os.Exit(1)
	}

	if *outputFlag == "" {
		os.Stdout.Write(jsonBytes)
		os.Stdout.WriteString("\n")
	} else {
		if err := os.WriteFile(*outputFlag, append(jsonBytes, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "[blis-kvtime] metrics written to %s\n", *outputFlag)
	}
}
