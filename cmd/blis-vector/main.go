// cmd/blis-vector — experiment runner for the KV-time vector residency campaign.
//
// This binary runs a single-instance BLIS simulation with a two-pool KV store
// (TwoPoolKVStore: independent P-pool and D-pool capacities) and any of five
// schedulers from the vector campaign:
//   - vector-kvtime:        per-axis, time-integrated, integrate-then-combine
//   - scalar-kvtime-vector: axis-collapsed single bucket over K_P+K_D
//   - static-drf:          instantaneous dominant share, no integral, no bucket
//   - sdrf-occupancy:      integrates instantaneous dominant share
//   - fcfs:                no meter, FIFO
//
// Usage:
//
//	blis-vector --scheduler vector-kvtime --seed 17 --duration 600 --warmup 30 \
//	            --workload V1-bursty.yaml \
//	            --kp-blocks 12288 --kd-blocks 12288 \
//	            --omega 0.45 --beta-seconds 10 --h-seconds 0 \
//	            --latency-backend trained-physics \
//	            --output results.json
//
// This file is part of patch 03-vector-untracked.
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

// TTFTPercentiles holds TTFT latency percentiles (microseconds).
type TTFTPercentiles struct {
	P50Us float64 `json:"p50_us"`
	P95Us float64 `json:"p95_us"`
	P99Us float64 `json:"p99_us"`
}

// PerWindowExcursionOutput holds per-window dominant-share excursion statistics
// for a single tenant, emitted as a top-level field in VectorRunMetrics.
type PerWindowExcursionOutput struct {
	P50         float64 `json:"p50"`
	P90         float64 `json:"p90"`
	P95         float64 `json:"p95"`
	P99         float64 `json:"p99"`
	Mean        float64 `json:"mean"`
	Max         float64 `json:"max"`
	WindowCount int     `json:"window_count"`
}

// VectorTenantMetrics holds per-tenant per-axis measurements.
type VectorTenantMetrics struct {
	// ─ per-axis residency (token·µs) ─
	KVTimeP float64 `json:"kv_time_p_token_us"` // A_i^P(T)
	KVTimeD float64 `json:"kv_time_d_token_us"` // A_i^D(T)
	KVTimeScalar float64 `json:"kv_time_scalar_token_us"` // A_i^P + A_i^D

	// ─ dominant share (max_r A_i^r / (C^r · T)) ─
	DominantShare float64 `json:"dominant_share"` // max_r A_i^r / (C^r·T_active)

	// ─ scalar memory-time share (legacy compatibility) ─
	MemoryTimeShare float64 `json:"memory_time_share"` // scalar A_i / ((K_P+K_D) · T)

	// ─ completion metrics ─
	CompletedRequests int64   `json:"completed_requests"`
	SubmittedRequests int64   `json:"submitted_requests"`
	UnservedAtHorizon int64   `json:"unserved_at_horizon"`
	InFlightAtHorizon int64   `json:"in_flight_at_horizon"`
	CompletionRate    float64 `json:"completion_rate"`

	// ─ TTFT ─
	TTFTPercentiles *TTFTPercentiles `json:"ttft_percentiles,omitempty"`

	// ─ handoff stall (vector-kvtime specific) ─
	HandoffStallEventCount int64 `json:"handoff_stall_event_count"`
}

// PoolUtilization holds per-pool utilization statistics.
type PoolUtilization struct {
	PPoolUsedBlocks     int64   `json:"p_pool_used_blocks"`
	DPoolUsedBlocks     int64   `json:"d_pool_used_blocks"`
	PPoolFreeBlocks     int64   `json:"p_pool_free_blocks"`
	DPoolFreeBlocks     int64   `json:"d_pool_free_blocks"`
	PPoolUtilization    float64 `json:"p_pool_utilization"`      // end-of-run used / total
	DPoolUtilization    float64 `json:"d_pool_utilization"`      // end-of-run used / total
	PPoolAvgUtilization float64 `json:"p_pool_avg_utilization"`  // post-warmup average
	DPoolAvgUtilization float64 `json:"d_pool_avg_utilization"`  // post-warmup average
}

// VectorRunMetrics is the top-level output JSON.
type VectorRunMetrics struct {
	Scheduler              string                          `json:"scheduler"`
	Seed                   int64                           `json:"seed"`
	DurationS              float64                         `json:"duration_s"`
	WarmupS                float64                         `json:"warmup_s"`
	ActiveDurationUs       int64                           `json:"active_duration_us"`
	KPBlocks               int64                           `json:"kp_blocks"`
	KDBlocks               int64                           `json:"kd_blocks"`
	TotalKVBlocks          int64                           `json:"total_kv_blocks"`
	Tenants                map[string]*VectorTenantMetrics `json:"tenants"`

	// ─ dominant share disparity ─
	RhoDom float64 `json:"rho_dom"` // max_i dominant_share_i / min_i dominant_share_i

	// ─ apparatus checks ─
	ConservationViolations int64 `json:"conservation_violations"`
	TotalMeterTicks        int64 `json:"total_meter_ticks"`
	BacklogNonEmptyTicks   int64 `json:"backlog_nonempty_ticks"`
	StalledHandoffCount    int   `json:"stalled_handoff_count_at_end"`
	TotalHandoffStalls     int64 `json:"total_handoff_stall_events"`

	// ─ pool utilization at end ─
	PoolUtilization *PoolUtilization `json:"pool_utilization,omitempty"`

	// ─ legacy scalar fairness ratio ─
	MemorytimeShareRatio   float64 `json:"memorytime_share_ratio"`

	TotalCompletedRequests int64   `json:"total_completed_requests"`
	SimEndedUs             int64   `json:"sim_ended_us"`
	TauIdleUs              int64   `json:"tau_idle_us"`
	IdleFraction           float64 `json:"idle_fraction"`

	// ─ V3 integral order gap ─
	// integral_order_gap = (integral max_r share_i^r - max_r integral share_i^r) / max_r integral share_i^r
	// Only non-nil for sdrf-occupancy which computes integrated dominant share.
	IntegralOrderGap map[string]float64 `json:"integral_order_gap,omitempty"`

	// ─ per-window dominant-share excursion (all schedulers; only non-nil when --window-size-s > 0) ─
	// per_window_dominant_share_excursion[tenant] = excursion percentiles over post-warmup sealed windows.
	// excursion_i(w) = max(0, dominant_share_i(w) - omega_i).
	PerWindowDomShareExcursion map[string]*PerWindowExcursionOutput `json:"per_window_dominant_share_excursion,omitempty"`
}

// ─── TTFT tracker ────────────────────────────────────────────────────────────

type ttftTracker struct {
	ttfts    map[string][]float64
	warmupUs int64
}

func newTTFTTracker(warmupUs int64) *ttftTracker {
	return &ttftTracker{ttfts: make(map[string][]float64), warmupUs: warmupUs}
}

func (t *ttftTracker) RecordCompletion(req *sim.Request, completionUs int64) {
	if completionUs < t.warmupUs {
		return
	}
	if !req.TTFTSet || req.FirstTokenTime <= 0 {
		return
	}
	t.ttfts[req.TenantID] = append(t.ttfts[req.TenantID], float64(req.FirstTokenTime))
}

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

// ─── Submission tracker ───────────────────────────────────────────────────────

type submissionTracker struct {
	byTenant  map[string]int64
	blockSize int64
}

func newSubmissionTracker(blockSize int64) *submissionTracker {
	return &submissionTracker{byTenant: make(map[string]int64), blockSize: blockSize}
}

func (s *submissionTracker) Record(req *sim.Request) {
	if req == nil {
		return
	}
	s.byTenant[req.TenantID]++
}

// ─── Completion tracker ───────────────────────────────────────────────────────

type completionTracker struct {
	inner         func(*sim.Request, int64) []*sim.Request
	byTenant      map[string]int64
	byTenantTotal map[string]int64
	totalDone     int64
	ttft          *ttftTracker
	warmupUs      int64
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
		t.byTenantTotal[req.TenantID]++
		if clock >= t.warmupUs {
			t.byTenant[req.TenantID]++
			t.totalDone++
		}
		if t.ttft != nil {
			t.ttft.RecordCompletion(req, clock)
		}
	}
	if t.inner != nil {
		return t.inner(req, clock)
	}
	return nil
}

// ─── Backlog tracker ──────────────────────────────────────────────────────────

type backlogTracker struct {
	warmupUs            int64
	snapshotIntervalUs  int64
	idleThresholdBlocks int64
	nonEmptyTicks       int64
	tauIdleUs           int64
	prevClock           int64

	// pool utilization tracking (average over post-warmup ticks)
	twoPool        *kv.TwoPoolKVStore
	sumPUsedBlocks float64
	sumDUsedBlocks float64
	utilTicks      int64
}

func newBacklogTracker(warmupUs, snapshotIntervalUs, idleThresholdBlocks int64) *backlogTracker {
	return &backlogTracker{
		warmupUs:            warmupUs,
		snapshotIntervalUs:  snapshotIntervalUs,
		idleThresholdBlocks: idleThresholdBlocks,
	}
}

func (b *backlogTracker) OnProgress(snap sim.ProgressSnapshot) {
	clock := snap.Clock
	if clock < b.warmupUs {
		b.prevClock = clock
		return
	}
	if len(snap.InstanceSnapshots) > 0 {
		inst := snap.InstanceSnapshots[0]
		qDepth := inst.QueueDepth
		used := inst.KVTotalBlocks - inst.KVFreeBlocks
		if qDepth > 0 {
			b.nonEmptyTicks++
		}
		if qDepth > 0 && used < b.idleThresholdBlocks {
			interval := clock - b.prevClock
			if b.prevClock > 0 && interval > 0 {
				b.tauIdleUs += interval
			}
		}
	}
	// Track average pool utilization (post-warmup)
	if b.twoPool != nil {
		b.sumPUsedBlocks += float64(b.twoPool.PPoolUsedBlocks())
		b.sumDUsedBlocks += float64(b.twoPool.DPoolUsedBlocks())
		b.utilTicks++
	}
	b.prevClock = clock
}

// AvgPPoolUtil returns average P-pool utilization over post-warmup ticks.
func (b *backlogTracker) AvgPPoolUtil(kpBlocks int64) float64 {
	if b.utilTicks == 0 || kpBlocks == 0 {
		return 0
	}
	return (b.sumPUsedBlocks / float64(b.utilTicks)) / float64(kpBlocks)
}

// AvgDPoolUtil returns average D-pool utilization over post-warmup ticks.
func (b *backlogTracker) AvgDPoolUtil(kdBlocks int64) float64 {
	if b.utilTicks == 0 || kdBlocks == 0 {
		return 0
	}
	return (b.sumDUsedBlocks / float64(b.utilTicks)) / float64(kdBlocks)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	schedulerFlag  := flag.String("scheduler", "vector-kvtime", "scheduler: vector-kvtime | scalar-kvtime-vector | static-drf | sdrf-occupancy | fcfs")
	seedFlag       := flag.Int64("seed", 17, "RNG seed")
	durationFlag   := flag.Float64("duration", 600.0, "simulation duration in seconds")
	warmupFlag     := flag.Float64("warmup", 30.0, "warmup period in seconds")
	workloadFlag   := flag.String("workload", "", "path to workload YAML spec")
	outputFlag     := flag.String("output", "", "path for JSON output (stdout if empty)")
	kpBlocksFlag   := flag.Int64("kp-blocks", 12288, "P-pool KV cache blocks (prefill pool)")
	kdBlocksFlag   := flag.Int64("kd-blocks", 12288, "D-pool KV cache blocks (decode pool)")
	omegaFlag      := flag.Float64("omega", 0.45, "per-tenant per-axis entitlement share")
	betaSecondsFlag := flag.Float64("beta-seconds", 10.0, "bucket depth in seconds")
	hSecondsFlag   := flag.Float64("h-seconds", 0.0, "bucket overdraft floor in seconds")
	handoffDelayFlag := flag.Int64("handoff-delay-ms", 0, "P→D handoff delay in ms (0=instantaneous)")
	latencyBackend := flag.String("latency-backend", "trained-physics", "latency model: roofline | trained-physics")
	windowSizeFlag := flag.Float64("window-size-s", 0.0, "per-window dominant-share excursion window length in seconds (0=disabled)")
	flag.Parse()

	if *workloadFlag == "" {
		fmt.Fprintf(os.Stderr, "error: --workload is required\n")
		os.Exit(1)
	}
	_ = *handoffDelayFlag // headline is instantaneous (0); reserved for robustness sweep

	horizonUs        := int64(*durationFlag * 1e6)
	warmupUs         := int64(*warmupFlag * 1e6)
	activeDurationUs := horizonUs - warmupUs
	kpBlocks         := *kpBlocksFlag
	kdBlocks         := *kdBlocksFlag
	totalKVBlocks    := kpBlocks + kdBlocks
	const blockSizeTokens = int64(16)
	const eta          = 0.9

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
		BytesPerParam:   2.0,
		IntermediateDim: 14336,
	}

	// ── Latency model ──
	var latencyCoeffs sim.LatencyCoeffs
	switch *latencyBackend {
	case "roofline":
		latencyCoeffs = sim.NewLatencyCoeffs(
			[]float64{0.0, 0.0, 0.0},
			[]float64{0.0, 0.0, 0.0},
		)
	case "trained-physics":
		// Llama-3.1-8B-Instruct / H100 / TP=1 / vLLM v0.11.0
		latencyCoeffs = sim.NewLatencyCoeffs(
			[]float64{0.152128, 0.0, 1.36252915, 0.752037, 32.09546717, 4.41684444, 126.024825, 481.8613888, 0.0, 1.94710771},
			[]float64{15563.199579, 777.3455, 45.907545},
		)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --latency-backend %q\n", *latencyBackend)
		os.Exit(1)
	}
	hwModelCfg := sim.NewModelHardwareConfig(modelCfg, hwCfg,
		"meta-llama/llama-3.1-8b-instruct", "H100", 1, *latencyBackend, 16384)
	latencyModel, err := latency.NewLatencyModel(latencyCoeffs, hwModelCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating latency model: %v\n", err)
		os.Exit(1)
	}

	// ── Load workload ──
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

	gw, err := workload.GenerateWorkload(&wlSpec, horizonUs, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating workload: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[blis-vector] scheduler=%s seed=%d kp=%d kd=%d requests=%d\n",
		*schedulerFlag, *seedFlag, kpBlocks, kdBlocks, len(gw.Requests))

	// ── Session manager ──
	sessionMgr := workload.NewSessionManager(gw.Sessions)

	// ── TwoPoolKVStore ──
	twoPool := kv.NewTwoPoolKVStore(kpBlocks, kdBlocks, blockSizeTokens)

	// ── VectorMeter ──
	vmeter := kvtime.NewVectorMeter(blockSizeTokens)

	// ── Configure per-window excursion tracking (if --window-size-s > 0) ──
	windowSizeUs := int64(*windowSizeFlag * 1e6)
	if windowSizeUs > 0 {
		vmeter.SetWindowParams(windowSizeUs, warmupUs, kpBlocks, kdBlocks, *omegaFlag)
	}

	// ── TTFT tracker ──
	ttft := newTTFTTracker(warmupUs)

	// ── Per-axis bucket configs (same ω/β/H for both tenants, both axes) ──
	axisCfgs := map[string]kvtime.AxisBucketConfig{
		"tenantA": {OmegaI: *omegaFlag, BetaSeconds: *betaSecondsFlag, HSeconds: *hSecondsFlag},
		"tenantB": {OmegaI: *omegaFlag, BetaSeconds: *betaSecondsFlag, HSeconds: *hSecondsFlag},
	}
	// Scalar bucket configs for scalar-kvtime-vector (over K_P + K_D).
	scalarCfgs := map[string]kvtime.TenantBucketConfig{
		"tenantA": {OmegaI: *omegaFlag, BetaSeconds: *betaSecondsFlag, HSeconds: *hSecondsFlag},
		"tenantB": {OmegaI: *omegaFlag, BetaSeconds: *betaSecondsFlag, HSeconds: *hSecondsFlag},
	}

	// ── Create scheduler ──
	var schedulerInst sim.InstanceScheduler
	var simPtr *sim.Simulator

	var vectorSched *kvtime.VectorKVTimeScheduler
	var scalarVectorSched *kvtime.ScalarKVTimeVectorScheduler
	var staticDRFSched *kvtime.StaticDRFScheduler
	var sdrfOccSched *kvtime.SDRFOccupancyScheduler
	var fcfsVectorSched *kvtime.FCFSVectorScheduler

	switch *schedulerFlag {
	case "vector-kvtime":
		perAxisBuckets := kvtime.NewPerAxisBucketManager(kpBlocks, kdBlocks, blockSizeTokens, axisCfgs)
		vectorSched = kvtime.NewVectorKVTimeScheduler(twoPool, vmeter, perAxisBuckets)
		schedulerInst = vectorSched

	case "scalar-kvtime-vector":
		scalarBuckets := kvtime.NewBucketManager(totalKVBlocks, blockSizeTokens, scalarCfgs)
		scalarVectorSched = kvtime.NewScalarKVTimeVectorScheduler(twoPool, vmeter, scalarBuckets)
		schedulerInst = scalarVectorSched

	case "static-drf":
		staticDRFSched = kvtime.NewStaticDRFScheduler(twoPool, vmeter)
		schedulerInst = staticDRFSched

	case "sdrf-occupancy":
		sdrfOccSched = kvtime.NewSDRFOccupancyScheduler(twoPool, vmeter)
		schedulerInst = sdrfOccSched

	case "fcfs":
		fcfsVectorSched = kvtime.NewFCFSVectorScheduler(twoPool, vmeter)
		schedulerInst = fcfsVectorSched

	default:
		fmt.Fprintf(os.Stderr, "error: unknown scheduler %q; valid: vector-kvtime|scalar-kvtime-vector|static-drf|sdrf-occupancy|fcfs\n",
			*schedulerFlag)
		os.Exit(1)
	}

	// ── KV cache config (uses combined K_P + K_D for the sim config; actual
	//    allocation is mediated by TwoPoolKVStore overriding the standard cache) ──
	kvCfg := sim.NewKVCacheConfig(totalKVBlocks, blockSizeTokens, 0, 0, 0, 0)

	// ── SimConfig ──
	simCfg := sim.SimConfig{
		Horizon: horizonUs,
		Seed:    *seedFlag,
		KVCacheConfig: kvCfg,
		BatchConfig: sim.NewBatchConfig(256, 32768, 0),
		LatencyCoeffs:       latencyCoeffs,
		ModelHardwareConfig: hwModelCfg,
		PolicyConfig:        sim.NewPolicyConfig("fcfs", "fcfs"),
	}

	// ── Create simulator (inject TwoPoolKVStore as the KVStore) ──
	simulator, err := sim.NewSimulatorWithScheduler(simCfg, twoPool, latencyModel, schedulerInst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating simulator: %v\n", err)
		os.Exit(1)
	}
	simPtr = simulator

	// ── Wire back-references ──
	if vectorSched != nil {
		vectorSched.SetSimulator(simPtr)
	}
	if scalarVectorSched != nil {
		scalarVectorSched.SetSimulator(simPtr)
	}
	if staticDRFSched != nil {
		staticDRFSched.SetSimulator(simPtr)
	}
	if sdrfOccSched != nil {
		sdrfOccSched.SetSimulator(simPtr)
	}
	if fcfsVectorSched != nil {
		fcfsVectorSched.SetSimulator(simPtr)
	}

	// ── Submission / completion trackers ──
	submissionsT := newSubmissionTracker(blockSizeTokens)
	tracker := newCompletionTracker(func(req *sim.Request, clock int64) []*sim.Request {
		newReqs := sessionMgr.OnComplete(req, clock)
		for _, r := range newReqs {
			submissionsT.Record(r)
		}
		return newReqs
	}, ttft, warmupUs)
	simulator.OnRequestDone = tracker.OnDone

	// ── Backlog + τ_idle tracker ──
	const snapshotIntervalUs = int64(1000)
	idleThresholdBlocks := int64(eta * float64(totalKVBlocks))
	backlog := newBacklogTracker(warmupUs, snapshotIntervalUs, idleThresholdBlocks)
	backlog.twoPool = twoPool // wire pool reference for average utilization tracking
	simulator.SetProgressHook(backlog, snapshotIntervalUs)

	// ── Inject requests ──
	for _, req := range gw.Requests {
		submissionsT.Record(req)
		simulator.InjectArrival(req)
	}

	// ── Run simulation ──
	fmt.Fprintf(os.Stderr, "[blis-vector] running (horizon=%dµs warmup=%dµs)...\n", horizonUs, warmupUs)
	simulator.Run()
	simulator.Finalize()
	fmt.Fprintf(os.Stderr, "[blis-vector] done: clock=%dµs completed=%d\n",
		simulator.Clock, tracker.totalDone)

	// ── Compute metrics ──
	kvTimesP := vmeter.TenantKVTimeP()
	kvTimesD := vmeter.TenantKVTimeD()
	kvTimesScalar := vmeter.TenantKVTimeScalar()
	domShares := vmeter.DominantShare(kpBlocks, kdBlocks, activeDurationUs)
	rhoDom := vmeter.RhoDom(kpBlocks, kdBlocks, activeDurationUs)
	totalCapTokenUs := float64(totalKVBlocks) * float64(blockSizeTokens) * float64(activeDurationUs)

	// ── Coverage / survivorship-bias metrics ──
	unservedAtHorizon := make(map[string]int64)
	for _, r := range simulator.WaitQ.Items() {
		if r != nil {
			unservedAtHorizon[r.TenantID]++
		}
	}
	inFlightAtHorizon := make(map[string]int64)
	if simulator.RunningBatch != nil {
		for _, r := range simulator.RunningBatch.Requests {
			if r != nil {
				inFlightAtHorizon[r.TenantID]++
			}
		}
	}

	// ── Build union of all tenants ──
	allTenants := make(map[string]struct{})
	for t := range kvTimesScalar {
		allTenants[t] = struct{}{}
	}
	for t := range submissionsT.byTenant {
		allTenants[t] = struct{}{}
	}
	for t := range tracker.byTenantTotal {
		allTenants[t] = struct{}{}
	}

	tenantResults := make(map[string]*VectorTenantMetrics)
	for tenant := range allTenants {
		submitted := submissionsT.byTenant[tenant]
		completedTotal := tracker.byTenantTotal[tenant]
		completedPostWarmup := tracker.byTenant[tenant]
		var compRate float64
		if submitted > 0 {
			compRate = float64(completedTotal) / float64(submitted)
		}

		tm := &VectorTenantMetrics{
			KVTimeP:            kvTimesP[tenant],
			KVTimeD:            kvTimesD[tenant],
			KVTimeScalar:       kvTimesScalar[tenant],
			DominantShare:      domShares[tenant],
			MemoryTimeShare:    kvTimesScalar[tenant] / totalCapTokenUs,
			CompletedRequests:  completedPostWarmup,
			SubmittedRequests:  submitted,
			UnservedAtHorizon:  unservedAtHorizon[tenant],
			InFlightAtHorizon:  inFlightAtHorizon[tenant],
			CompletionRate:     compRate,
			TTFTPercentiles:    ttft.Percentiles(tenant),
			HandoffStallEventCount: twoPool.HandoffStallCount(tenant), // per request; aggregate via TotalHandoffStallEvents
		}
		_ = completedTotal
		tenantResults[tenant] = tm
	}

	// ── Legacy scalar ratio ──
	var maxScalarShare, minScalarShare float64
	minScalarShare = math.MaxFloat64
	for _, tm := range tenantResults {
		if tm.MemoryTimeShare > maxScalarShare {
			maxScalarShare = tm.MemoryTimeShare
		}
		if tm.MemoryTimeShare < minScalarShare {
			minScalarShare = tm.MemoryTimeShare
		}
	}
	var scalarRatio float64
	if minScalarShare > 0 {
		scalarRatio = maxScalarShare / minScalarShare
	}

	// ── τ_idle ──
	tauIdleUs := min(backlog.tauIdleUs, activeDurationUs)
	idleFraction := 0.0
	if activeDurationUs > 0 {
		idleFraction = float64(tauIdleUs) / float64(activeDurationUs)
	}

	// ── Pool utilization (end-of-run + post-warmup average) ──
	poolUtil := &PoolUtilization{
		PPoolUsedBlocks:     twoPool.PPoolUsedBlocks(),
		DPoolUsedBlocks:     twoPool.DPoolUsedBlocks(),
		PPoolFreeBlocks:     twoPool.PPoolFreeBlocks(),
		DPoolFreeBlocks:     twoPool.DPoolFreeBlocks(),
		PPoolUtilization:    float64(twoPool.PPoolUsedBlocks()) / float64(kpBlocks),
		DPoolUtilization:    float64(twoPool.DPoolUsedBlocks()) / float64(kdBlocks),
		PPoolAvgUtilization: backlog.AvgPPoolUtil(kpBlocks),
		DPoolAvgUtilization: backlog.AvgDPoolUtil(kdBlocks),
	}

	// ── Per-window dominant-share excursion (all schedulers; only when --window-size-s > 0) ──
	var perWindowExcursion map[string]*PerWindowExcursionOutput
	if windowSizeUs > 0 {
		stats := vmeter.WindowExcursionPercentiles()
		if len(stats) > 0 {
			perWindowExcursion = make(map[string]*PerWindowExcursionOutput, len(stats))
			for tenant, s := range stats {
				perWindowExcursion[tenant] = &PerWindowExcursionOutput{
					P50:         s.P50,
					P90:         s.P90,
					P95:         s.P95,
					P99:         s.P99,
					Mean:        s.Mean,
					Max:         s.Max,
					WindowCount: s.WindowCount,
				}
			}
		}
	}

	// ── V3 integral order gap (sdrf-occupancy only) ──
	var integralOrderGap map[string]float64
	if sdrfOccSched != nil {
		// integratedDomShare[tenant] = ∫ max_r share_i^r dt  (combine-then-integrate)
		// vector meter gives max_r integral share_i^r via DominantShare (integrate-then-combine)
		intDom := sdrfOccSched.IntegratedDomShare()
		integralOrderGap = make(map[string]float64, len(allTenants))
		for tenant := range allTenants {
			intFirst := intDom[tenant]                         // ∫ max_r share_i^r dt
			maxFirst := domShares[tenant] * float64(activeDurationUs) // max_r ∫ share_i^r dt
			if maxFirst > 0 {
				integralOrderGap[tenant] = (intFirst - maxFirst) / maxFirst
			}
		}
	}

	// ── Assemble output ──
	out := &VectorRunMetrics{
		Scheduler:              *schedulerFlag,
		Seed:                   *seedFlag,
		DurationS:              *durationFlag,
		WarmupS:                *warmupFlag,
		ActiveDurationUs:       activeDurationUs,
		KPBlocks:               kpBlocks,
		KDBlocks:               kdBlocks,
		TotalKVBlocks:          totalKVBlocks,
		Tenants:                tenantResults,
		RhoDom:                 rhoDom,
		ConservationViolations: vmeter.ConservationViolations(),
		TotalMeterTicks:        vmeter.TotalTicks(),
		BacklogNonEmptyTicks:   backlog.nonEmptyTicks,
		StalledHandoffCount:    twoPool.StalledHandoffCount(),
		TotalHandoffStalls:     twoPool.TotalHandoffStallEvents(),
		PoolUtilization:        poolUtil,
		MemorytimeShareRatio:   scalarRatio,
		TotalCompletedRequests: tracker.totalDone,
		SimEndedUs:             simulator.Clock,
		TauIdleUs:              tauIdleUs,
		IdleFraction:           idleFraction,
		IntegralOrderGap:       integralOrderGap,
		PerWindowDomShareExcursion: perWindowExcursion,
	}

	// ── Write output ──
	outJSON, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling output: %v\n", err)
		os.Exit(1)
	}
	if *outputFlag == "" {
		fmt.Println(string(outJSON))
	} else {
		if err := os.WriteFile(*outputFlag, outJSON, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	}
}
