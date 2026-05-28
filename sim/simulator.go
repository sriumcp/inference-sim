// sim/simulator.go
package sim

import (
	"container/heap"
	"fmt"
	"math/rand"
	"sort"

	"github.com/sirupsen/logrus"

	"github.com/inference-sim/inference-sim/sim/internal/util"
)

const MaxTokenID = 128000 // Max token ID in request input/output

// eventEntry wraps an Event with a sequence ID for deterministic ordering.
// The EventQueue orders by (Timestamp, Priority, seqID), matching the
// cluster event queue's scheme. seqID breaks ties within same-type same-timestamp events.
type eventEntry struct {
	event Event
	seqID int64
}

// EventQueue implements heap.Interface and orders events by (timestamp, priority, seqID).
// This ensures deterministic same-tick event ordering (INV-6 improvement).
type EventQueue []eventEntry

func (eq EventQueue) Len() int { return len(eq) }

func (eq EventQueue) Less(i, j int) bool {
	if eq[i].event.Timestamp() != eq[j].event.Timestamp() {
		return eq[i].event.Timestamp() < eq[j].event.Timestamp()
	}
	if eq[i].event.Priority() != eq[j].event.Priority() {
		return eq[i].event.Priority() < eq[j].event.Priority()
	}
	return eq[i].seqID < eq[j].seqID
}

func (eq EventQueue) Swap(i, j int) { eq[i], eq[j] = eq[j], eq[i] }

func (eq *EventQueue) Push(x any) {
	*eq = append(*eq, x.(eventEntry))
}

func (eq *EventQueue) Pop() any {
	old := *eq
	n := len(old)
	item := old[n-1]
	*eq = old[:n-1]
	return item
}

// SimConfig holds all configuration for creating a Simulator.
// Sub-configs are embedded so fields are accessible via promotion
// (e.g., cfg.TotalKVBlocks resolves to cfg.KVCacheConfig.TotalKVBlocks).
type SimConfig struct {
	// Simulation control (no sub-config — no factory uses only these)
	Horizon int64
	Seed    int64

	// Module-scoped sub-configs (R16)
	KVCacheConfig
	BatchConfig
	LatencyCoeffs
	ModelHardwareConfig
	PolicyConfig
	WorkloadConfig

	// SLO priority overrides for preemption victim selection (--preemption-policy priority).
	// nil = use GAIE defaults (critical=4, standard=3, batch=-1, sheddable=-2, background=-3).
	// Shared with admission: same overrides flow from policy bundle slo_priorities.
	// Set programmatically in cmd/root.go and cmd/replay.go from parsed bundle/CLI overrides — no YAML tag needed.
	SLOPriorityOverrides map[string]int

	// Credit enforcement config (iter-1 + iter-2 seven-baseline comparison).
	// CreditEnforcementEnabled is backward-compatible with the --credit-enforcement flag.
	// EnforcementPolicy selects the active multi-tenant fairness policy:
	//   "none" | "externality-credit" | "token-rate" | "wfq" | "red" | "ea-wfq" | "kv-quota" | "drf" | "oracle"
	// When EnforcementPolicy == "externality-credit", CreditEnforcementEnabled is set automatically.
	CreditEnforcementEnabled bool
	EnforcementPolicy        string
}

// Simulator is the core object that holds simulation time, system state, and the event loop.
type Simulator struct {
	Clock   int64
	Horizon int64
	// eventQueue has all the simulator events, like arrival and step events
	eventQueue EventQueue
	// WaitQ aka request waiting queue before it is scheduled
	WaitQ   *WaitQueue
	KVCache KVStore
	// Running batch contains the set of requests that go into the model for execution per Step.
	// In vLLM, running is a list (not queue) of requests, hence we don't call it RunningQ here.
	// Requests are ordered by First-Come-First-Served in WaitQ, and the same order is maintained
	// while adding requests to RunningBatch
	RunningBatch *Batch
	Metrics *Metrics
	// max number of requests RunningBatch can hold
	maxRunningReqs int64
	// max total number of new tokens across all requests in RunningBatch
	maxScheduledTokens        int64
	longPrefillTokenThreshold int64
	stepEvent                 Event
	stepCount                 int
	// map of request IDs to total num computed tokens (including cached tokens)
	reqNumComputedTokens map[string]int64
	batchFormation       BatchFormation
	model                  string
	gpu                    string
	maxModelLen            int64 // max total sequence length (0 = unlimited)
	rng                    *PartitionedRNG // partitioned RNG for deterministic multi-subsystem simulation
	sloMap *SLOPriorityMap // vLLM-convention priority mapping for instance-level scheduling
	scheduler      InstanceScheduler
	latencyModel           LatencyModel
	seqCounter             int64 // monotonic counter for event queue seqID (deterministic ordering)
	// OnRequestDone is an optional callback invoked when a request reaches a terminal
	// state (completed, length-capped, or timed out). Returns follow-up requests to inject.
	// Set by the caller (cmd/root.go or ClusterSimulator). Nil = no callback.
	OnRequestDone func(req *Request, tick int64) []*Request

	progressHook                ProgressHook
	simClockProgressIntervalUs int64
	nextSnapshotClockUs        int64
	tenantTokenCount            map[string]int64 // per-tenant cumulative output token count (for VTC)

	// Enforcement policy config (stored from SimConfig for use in methods).
	cfg SimConfig

	// Credit enforcement state (externality-credit policy).
	creditBalance   map[string]float64 // per-tenant credit balance (µs)
	creditRate      map[string]float64 // per-tenant replenishment rate (credits/µs)
	creditThreshold float64            // gate threshold: block tenant when balance < threshold

	// Per-policy accumulator state (shared/reused across baseline policies).
	tenantCumExternality map[string]float64 // ea-wfq, oracle: cumulative externality virtual time
	tenantCumStepTime    map[string]float64 // drf: cumulative step-time fraction
	tenantCumKVBlocks    map[string]float64 // drf: cumulative KV-blocks fraction
	tenantCumTokens      map[string]int64   // token-rate, wfq: cumulative new tokens processed
	numActiveTenants     int                // number of distinct tenant IDs seen (for quota computation)
	perTenantTokenQuota  float64            // token-rate: per-tenant token-per-second allowance (lazy init)
	redDropped           int                // red: local counter (synced to Metrics.REDDropped)
}

// NewSimulator creates a Simulator from a SimConfig struct and pre-built dependencies.
// All workload generation now happens externally — callers inject requests via InjectArrival.
func NewSimulator(cfg SimConfig, kvStore KVStore, latencyModel LatencyModel) (*Simulator, error) {
	if kvStore == nil {
		return nil, fmt.Errorf("NewSimulator: kvStore must not be nil")
	}
	if latencyModel == nil {
		return nil, fmt.Errorf("NewSimulator: latencyModel must not be nil")
	}
	if cfg.MaxRunningReqs <= 0 {
		return nil, fmt.Errorf("NewSimulator: MaxRunningReqs must be > 0, got %d", cfg.MaxRunningReqs)
	}
	if cfg.MaxScheduledTokens <= 0 {
		return nil, fmt.Errorf("NewSimulator: MaxScheduledTokens must be > 0, got %d", cfg.MaxScheduledTokens)
	}
	if cfg.LongPrefillTokenThreshold < 0 {
		return nil, fmt.Errorf("NewSimulator: LongPrefillTokenThreshold must be >= 0, got %d", cfg.LongPrefillTokenThreshold)
	}
	if cfg.MaxModelLen < 0 {
		return nil, fmt.Errorf("NewSimulator: MaxModelLen must be >= 0, got %d", cfg.MaxModelLen)
	}
	if cfg.MaxModelLen > 0 {
		if cfg.BlockSizeTokens <= 0 {
			return nil, fmt.Errorf("NewSimulator: BlockSizeTokens must be > 0 when MaxModelLen is set, got %d", cfg.BlockSizeTokens)
		}
		// Ceiling division for MaxModelLen → block count (R11).
		blocksForMaxLen := cfg.MaxModelLen / cfg.BlockSizeTokens
		if cfg.MaxModelLen%cfg.BlockSizeTokens != 0 {
			blocksForMaxLen++
		}
		if blocksForMaxLen > cfg.TotalKVBlocks {
			return nil, fmt.Errorf("NewSimulator: KV cache too small for MaxModelLen: need %d blocks (ceil(%d/%d)) but TotalKVBlocks=%d",
				blocksForMaxLen, cfg.MaxModelLen, cfg.BlockSizeTokens, cfg.TotalKVBlocks)
		}
	}
	batchFormation := NewBatchFormation(cfg.PreemptionPolicy, cfg.SoftPreemptionWeight)

	// Normalize: --credit-enforcement is backward-compatible alias for --enforcement-policy externality-credit
	if cfg.CreditEnforcementEnabled && cfg.EnforcementPolicy == "" {
		cfg.EnforcementPolicy = "externality-credit"
	}
	if cfg.EnforcementPolicy == "externality-credit" {
		cfg.CreditEnforcementEnabled = true
	}

	s := &Simulator{
		Clock:                     0,
		Horizon:                   cfg.Horizon,
		eventQueue:                make(EventQueue, 0),
		WaitQ:                     &WaitQueue{},
		KVCache:                   kvStore,
		RunningBatch:              &Batch{},
		Metrics:                   NewMetrics(),
		maxRunningReqs:            cfg.MaxRunningReqs,
		maxScheduledTokens:        cfg.MaxScheduledTokens,
		longPrefillTokenThreshold: cfg.LongPrefillTokenThreshold,
		stepEvent:                 nil,
		stepCount:                 0,
		reqNumComputedTokens:      make(map[string]int64),
		tenantTokenCount:          make(map[string]int64),
		batchFormation:            batchFormation,
		model:                     cfg.Model,
		gpu:                       cfg.GPU,
		maxModelLen:               cfg.MaxModelLen,
		latencyModel:              latencyModel,
		sloMap:                    NewSLOPriorityMap(cfg.SLOPriorityOverrides),
		cfg:                       cfg,
		creditBalance:             make(map[string]float64),
		creditRate:                make(map[string]float64),
		creditThreshold:           0.0,
		tenantCumExternality:      make(map[string]float64),
		tenantCumStepTime:         make(map[string]float64),
		tenantCumKVBlocks:         make(map[string]float64),
		tenantCumTokens:           make(map[string]int64),
	}
	s.rng = NewPartitionedRNG(NewSimulationKey(cfg.Seed))
	s.scheduler = NewScheduler(cfg.Scheduler)

	return s, nil
}

// WorkloadRNG returns the RNG for workload generation.
// This maintains backward compatibility with the original single-RNG implementation.
func (sim *Simulator) WorkloadRNG() *rand.Rand {
	return sim.rng.ForSubsystem(SubsystemWorkload)
}

// Schedule pushes an event into the simulator's EventQueue with a monotonic seqID.
// Note, this has nothing to do with vLLM's scheduler.schedule().
func (sim *Simulator) Schedule(ev Event) {
	sim.seqCounter++
	heap.Push(&sim.eventQueue, eventEntry{event: ev, seqID: sim.seqCounter})
}

// HasPendingEvents returns true if the EventQueue is non-empty.
func (sim *Simulator) HasPendingEvents() bool {
	return len(sim.eventQueue) > 0
}

// PeekNextEventTime returns the timestamp of the earliest pending event.
// Caller MUST check HasPendingEvents() first. Panics on empty queue.
func (sim *Simulator) PeekNextEventTime() int64 {
	return sim.eventQueue[0].event.Timestamp()
}

// ProcessNextEvent pops the earliest event, advances Clock, executes it, and returns it.
// The returned Event lets callers react to what happened (e.g., detect QueuedEvent for
// pending-request tracking) without maintaining fragile before/after heuristics.
// Caller MUST check HasPendingEvents() first. Panics on empty queue.
// Does NOT check horizon — caller is responsible.
//
// Special case — lazy cancellation: if the popped event is a TimeoutEvent for a
// request that has already completed (State == StateCompleted), the event is
// returned immediately without advancing Clock or calling Execute(). This models
// real-world client behavior where a deadline timer is cancelled the moment a
// response arrives, preventing orphaned timeouts from inflating SimEndedTime.
func (sim *Simulator) ProcessNextEvent() Event {
	entry := heap.Pop(&sim.eventQueue).(eventEntry)
	ev := entry.event

	// Lazy cancellation: a TimeoutEvent whose request already completed is an
	// orphan — the real-world equivalent of a client cancelling its deadline
	// timer when the response arrives. Skip it before advancing the clock so
	// Finalize() captures SimEndedTime from the last real-work event, not from
	// an orphaned no-op timeout 300s in the future.
	if te, ok := ev.(*TimeoutEvent); ok && te.Request.State == StateCompleted {
		return ev
	}

	sim.Clock = ev.Timestamp()
	logrus.Debugf("[tick %07d] Executing %T", sim.Clock, ev)
	ev.Execute(sim)
	return ev
}

// Finalize records end-of-run state and sets SimEndedTime.
// Call once after the event loop ends. Called by both sim.Run() (single-instance)
// and ClusterSimulator.Run() (cluster mode via inst.Finalize()).
func (sim *Simulator) Finalize() {
	// Record conservation fields (BC-8, BC-9) — must happen in Finalize
	// because cluster mode drives events via ProcessNextEvent() directly
	// and never calls sim.Run().
	sim.Metrics.StillQueued = sim.WaitQ.Len()
	if sim.RunningBatch != nil {
		sim.Metrics.StillRunning = len(sim.RunningBatch.Requests)
	}
	sim.Metrics.SimEndedTime = min(sim.Clock, sim.Horizon)
	logrus.Infof("[tick %07d] Simulation ended", sim.Clock)
}

// InjectArrival schedules an ArrivalEvent for req and registers it in Metrics.Requests.
func (sim *Simulator) InjectArrival(req *Request) {
	if req.ArrivalTime > sim.Horizon {
		logrus.Warnf("InjectArrival: request %s has ArrivalTime %d > Horizon %d; "+
			"ArrivalEvent will not fire (INV-1 conservation may be affected)",
			req.ID, req.ArrivalTime, sim.Horizon)
	}
	sim.Schedule(&ArrivalEvent{time: req.ArrivalTime, Request: req})
	sim.Metrics.Requests[req.ID] = NewRequestMetrics(req, float64(req.ArrivalTime)/1e6)
}

// InjectArrivalAt schedules an ArrivalEvent at eventTime (not req.ArrivalTime).
// Metrics.Requests uses req.ArrivalTime for ArrivedAt to preserve original arrival time.
// Used by cluster-mode online routing where event time differs from original arrival.
func (sim *Simulator) InjectArrivalAt(req *Request, eventTime int64) {
	sim.Schedule(&ArrivalEvent{time: eventTime, Request: req})
	sim.Metrics.Requests[req.ID] = NewRequestMetrics(req, float64(req.ArrivalTime)/1e6)
}

func (sim *Simulator) Run() {
	for sim.HasPendingEvents() {
		sim.ProcessNextEvent()
		if sim.Clock > sim.Horizon {
			break
		}
		sim.maybeDeliverProgressSnapshot(false)
	}
	sim.maybeDeliverProgressSnapshot(true)
	sim.Finalize()
}

// SetProgressHook registers an optional hook that receives periodic state
// snapshots during simulation execution. Must be called before Run().
// When hook is nil (default), there is zero behavioral or performance impact.
// simClockIntervalUs controls the minimum simulation-clock interval (microseconds)
// between periodic snapshots. If simClockIntervalUs <= 0, only the final snapshot
// is delivered.
func (sim *Simulator) SetProgressHook(hook ProgressHook, simClockIntervalUs int64) {
	sim.progressHook = hook
	if simClockIntervalUs > 0 {
		sim.simClockProgressIntervalUs = simClockIntervalUs
		sim.nextSnapshotClockUs = simClockIntervalUs
	}
}

func (sim *Simulator) maybeDeliverProgressSnapshot(isFinal bool) {
	if sim.progressHook == nil {
		return
	}
	if !isFinal && (sim.simClockProgressIntervalUs <= 0 || sim.Clock < sim.nextSnapshotClockUs) {
		return
	}
	clock := sim.Clock
	if isFinal {
		clock = min(sim.Clock, sim.Horizon)
	}
	snap := ProgressSnapshot{
		Clock:             clock,
		TotalCompleted:    sim.Metrics.CompletedRequests,
		TotalTimedOut:     sim.Metrics.TimedOutRequests,
		TotalDropped:      sim.Metrics.DroppedUnservable,
		TotalInputTokens:  sim.Metrics.TotalInputTokens,
		TotalOutputTokens: sim.Metrics.TotalOutputTokens,
		TotalPreemptions:  sim.Metrics.PreemptionCount,
		InstanceSnapshots: []InstanceSnapshot{sim.buildInstanceSnapshot()},
		TotalInstances:    1,
		ActiveInstances:   1,
		IsFinal:           isFinal,
	}
	sim.progressHook.OnProgress(snap)
	if !isFinal {
		sim.nextSnapshotClockUs += sim.simClockProgressIntervalUs
	}
}

func (sim *Simulator) buildInstanceSnapshot() InstanceSnapshot {
	return InstanceSnapshot{
		ID:                "instance-0",
		Model:             sim.model,
		State:             InstanceStateActive,
		QueueDepth:        sim.QueueDepth(),
		BatchSize:         sim.BatchSize(),
		KVUtilization:     float64(sim.KVCache.UsedBlocks()) / float64(max(sim.KVCache.TotalCapacity(), 1)),
		KVFreeBlocks:      sim.KVCache.TotalCapacity() - sim.KVCache.UsedBlocks(),
		KVTotalBlocks:     sim.KVCache.TotalCapacity(),
		CacheHitRate:      sim.KVCache.CacheHitRate(),
		PreemptionCount:   sim.Metrics.PreemptionCount,
		CompletedRequests: sim.Metrics.CompletedRequests,
		TimedOutRequests:  sim.Metrics.TimedOutRequests,
	}
}

// QueueDepth returns the number of requests in the wait queue.
func (sim *Simulator) QueueDepth() int { return sim.WaitQ.Len() }

// DrainWaitQueue removes and returns all requests currently in the wait queue.
// Used by DrainRedirect policy to re-inject queued requests into the cluster router.
// After this call, WaitQ.Len() == 0.
func (sim *Simulator) DrainWaitQueue() []*Request {
	items := sim.WaitQ.Items()
	sim.WaitQ = &WaitQueue{}
	return items
}

// BatchSize returns the number of requests in the running batch, or 0 if nil.
func (sim *Simulator) BatchSize() int {
	if sim.RunningBatch == nil {
		return 0
	}
	return len(sim.RunningBatch.Requests)
}

// CumExternality implements TenantExternalityTracker. Returns the running
// externality counter for tenant tid; 0 for unknown tenants (the
// underlying map's zero default).
//
// Read-only. Safe to call from any policy layer (admission, preemption,
// batch formation) without taking locks — the simulator is single-threaded
// per the package's thread-safety contract.
func (sim *Simulator) CumExternality(tid string) float64 {
	return sim.tenantCumExternality[tid]
}

// KVPressureSignal implements TenantExternalityTracker. Returns a scalar
// in [0, 1] indicating cache-jam pressure right now. Mirrors the gate
// condition the simulator's μ̂ formula uses (kvUtil > 0.9 && WaitQ
// non-empty), normalized so admission/preemption layers can multiply
// against it directly. See sim/externality.go for the rationale.
func (sim *Simulator) KVPressureSignal() float64 {
	if sim.KVCache == nil {
		return 0
	}
	cap := sim.KVCache.TotalCapacity()
	if cap == 0 {
		return 0
	}
	kvUtil := float64(sim.KVCache.UsedBlocks()) / float64(cap)
	return kvPressureFromUtil(kvUtil, sim.WaitQ != nil && sim.WaitQ.Len() > 0)
}

// CurrentClock returns the current simulation clock (in ticks).
func (sim *Simulator) CurrentClock() int64 { return sim.Clock }

// SimHorizon returns the simulation horizon (in ticks).
func (sim *Simulator) SimHorizon() int64 { return sim.Horizon }

// ScheduleStepIfIdle schedules a batch-formation step if the instance is idle
// with pending work. Used by gateway eviction to maintain INV-8 after removing
// the last request from RunningBatch.
func (sim *Simulator) ScheduleStepIfIdle(time int64) {
	if sim.stepEvent == nil && sim.WaitQ.Len() > 0 {
		step := &StepEvent{time: time}
		sim.stepEvent = step
		sim.Schedule(step)
	}
}

// PostDecodeFixedOverhead returns the latency model's fixed per-request post-decode
// overhead in microseconds. Used by the cluster layer to include overhead in
// parent.CompletionTime when disaggregated decode sub-requests complete.
// Returns 0 for all backends except trained-physics (BC-1, issue #846).
func (sim *Simulator) PostDecodeFixedOverhead() int64 {
	return sim.latencyModel.PostDecodeFixedOverhead()
}

// EnqueueRequest adds a newly arrived request to the waiting queue.
//
// Preprocessing: auto-fills MaxOutputLen when the client doesn't set a budget
// (MaxOutputLen == 0) and maxModelLen > 0. Sets MaxOutputLen = maxModelLen - len(InputTokens),
// mirroring vLLM's input_processor.py:554 (max_tokens = max_model_len - seq_len).
// Workload generators normally set MaxOutputLen = len(OutputTokens) (tight budget);
// this auto-fill is a safety net for requests that bypass generators.
//
// Three guards then prevent unservable requests from entering the queue:
//  0. MaxOutputLen validation (R3): drops requests with negative MaxOutputLen.
//  1. MaxModelLen guard (when maxModelLen > 0): validates the request fits within
//     the model's context window. First checks input >= maxModelLen (vLLM uses >=:
//     input filling the entire context leaves no room for output). Then, when
//     MaxOutputLen > 0 (client budget), checks input + budget <= maxModelLen.
//  2. KV capacity guard (defense-in-depth, always active): drops requests whose input
//     tokens alone require more KV blocks than total cache capacity (R19: livelock protection).
//
// All guards mirror real vLLM behavior where oversized requests are rejected
// before entering the engine. The control plane never peeks at len(OutputTokens) —
// respecting the oracle knowledge boundary (INV-9, #567).
func (sim *Simulator) EnqueueRequest(r *Request) {
	// Guard -1: Already timed out (race: TimeoutEvent fired before QueuedEvent).
	// Request was timed out during the queueing delay (alpha overhead) before the server
	// processed the input. TotalInputTokens is NOT counted for this path.
	// INV-1 holds: request is counted in timed_out bucket.
	if r.State == StateTimedOut {
		return
	}

	// Auto-fill: if client didn't set a budget, cap at remaining context window.
	if r.MaxOutputLen == 0 && sim.maxModelLen > 0 && int64(len(r.InputTokens)) < sim.maxModelLen {
		r.MaxOutputLen = int(sim.maxModelLen) - len(r.InputTokens)
	}

	// Guard 0: Negative MaxOutputLen check (R3)
	if r.MaxOutputLen < 0 {
		logrus.Warnf("dropping request %s: MaxOutputLen %d is negative",
			r.ID, r.MaxOutputLen)
		sim.Metrics.DroppedUnservable++
		delete(sim.Metrics.Requests, r.ID)
		// Callback for dropped requests (R1: don't silently discard, BC-17)
		if sim.OnRequestDone != nil {
			for _, next := range sim.OnRequestDone(r, sim.Clock) {
				sim.InjectArrival(next)
			}
		}
		return
	}

	// Guard 1: MaxModelLen check
	if sim.maxModelLen > 0 {
		if int64(len(r.InputTokens)) >= sim.maxModelLen {
			logrus.Warnf("dropping request %s: input length %d >= MaxModelLen %d (no room for output)",
				r.ID, len(r.InputTokens), sim.maxModelLen)
			sim.Metrics.DroppedUnservable++
			delete(sim.Metrics.Requests, r.ID)
			if sim.OnRequestDone != nil {
				for _, next := range sim.OnRequestDone(r, sim.Clock) {
					sim.InjectArrival(next)
				}
			}
			return
		}
		if r.MaxOutputLen > 0 {
			totalSeqLen := int64(len(r.InputTokens)) + int64(r.MaxOutputLen)
			if totalSeqLen > sim.maxModelLen {
				logrus.Warnf("dropping request %s: total sequence length %d (input=%d + budget=%d) exceeds MaxModelLen %d",
					r.ID, totalSeqLen, len(r.InputTokens), r.MaxOutputLen, sim.maxModelLen)
				sim.Metrics.DroppedUnservable++
				delete(sim.Metrics.Requests, r.ID)
				if sim.OnRequestDone != nil {
					for _, next := range sim.OnRequestDone(r, sim.Clock) {
						sim.InjectArrival(next)
					}
				}
				return
			}
		}
	}

	// Guard 2: KV capacity check (defense-in-depth, always active)
	blocksNeeded := (int64(len(r.InputTokens)) + sim.KVCache.BlockSize() - 1) / sim.KVCache.BlockSize()
	if blocksNeeded > sim.KVCache.TotalCapacity() {
		logrus.Warnf("dropping request %s: input requires %d KV blocks but cache has only %d total",
			r.ID, blocksNeeded, sim.KVCache.TotalCapacity())
		sim.Metrics.DroppedUnservable++
		delete(sim.Metrics.Requests, r.ID)
		if sim.OnRequestDone != nil {
			for _, next := range sim.OnRequestDone(r, sim.Clock) {
				sim.InjectArrival(next)
			}
		}
		return
	}

	// Input tokens counted BEFORE past-due check (request was received)
	sim.Metrics.TotalInputTokens += len(r.InputTokens)

	// Past-due guard (EC-2): check BEFORE enqueue to avoid enqueue-then-remove.
	// Request is counted as timed_out, not dropped_unservable.
	if r.Deadline > 0 && r.Deadline <= sim.Clock {
		r.State = StateTimedOut
		sim.Metrics.TimedOutRequests++
		if sim.OnRequestDone != nil {
			for _, next := range sim.OnRequestDone(r, sim.Clock) {
				sim.InjectArrival(next)
			}
		}
		return
	}

	// Pre-processor: convert cluster-convention SLO priority to vLLM instance convention.
	// Mirrors the llm-d → vLLM dispatch boundary in production (lower = more urgent).
	// Overwrites any routing-hint Priority (which was always transient; see routing.go:59).
	if sim.sloMap == nil {
		// sloMap should always be set by NewSimulator; this path indicates manual struct construction.
		logrus.Warnf("Simulator.sloMap not initialized — using DefaultSLOPriorityMap; prefer NewSimulator()")
		sim.sloMap = DefaultSLOPriorityMap()
	}
	r.Priority = float64(sim.sloMap.InvertForVLLM(r.SLOClass))

	// RED probabilistic drop (B3): applied at admission before WaitQ enqueue.
	// Uses queueing-based probability: drop probability increases linearly from
	// 0 at minTh to maxP at maxTh. RED uses a dedicate RNG subsystem for determinism.
	if sim.cfg.EnforcementPolicy == "red" {
		qLen := sim.WaitQ.Len()
		const minTh, maxTh = 30, 80
		const maxP = 0.1
		if qLen > minTh {
			p := float64(qLen-minTh) / float64(maxTh-minTh) * maxP
			if p > maxP {
				p = maxP
			}
			if sim.rng.ForSubsystem("enforcement").Float64() < p {
				sim.redDropped++
				sim.Metrics.REDDropped++
				delete(sim.Metrics.Requests, r.ID)
				return
			}
		}
	}

	// Track active tenants and initialize per-tenant state for enforcement policies.
	if r.TenantID != "" {
		if _, seen := sim.creditBalance[r.TenantID]; !seen {
			sim.numActiveTenants++
			// Initialize credit balance for externality-credit policy.
			if sim.cfg.EnforcementPolicy == "externality-credit" {
				sim.creditBalance[r.TenantID] = 100_000_000.0 // 100M µs initial credit
				sim.creditRate[r.TenantID] = 1.0               // 1 credit/µs replenishment
			}
			// Initialize cumulative accumulators for all policies.
			sim.tenantCumExternality[r.TenantID] = 0.0
			sim.tenantCumStepTime[r.TenantID] = 0.0
			sim.tenantCumKVBlocks[r.TenantID] = 0.0
			sim.tenantCumTokens[r.TenantID] = 0
		}
	}

	sim.WaitQ.Enqueue(r)

	// Schedule timeout event (after all guards + enqueue — BC-5)
	// Skip scheduling when deadline > horizon (perf: avoids orphaned events)
	if r.Deadline > 0 && r.Deadline <= sim.Horizon {
		sim.Schedule(&TimeoutEvent{time: r.Deadline, Request: r})
	}
}

// EnqueueDecodeSubRequest enqueues a decode sub-request that already has KV blocks
// pre-allocated (via PD disaggregation transfer). Bypasses the oversized-request guard
// (blocks already allocated, guard would leak them) and does NOT increment TotalInputTokens
// (input tokens were already counted by the prefill sub-request).
// clusterTime is the cluster-level clock when this request is injected (from
// KVTransferCompletedEvent.Execute()). The StepEvent is scheduled at
// max(sim.Clock, clusterTime) to prevent the instance from processing the
// decode sub-request at a stale internal time that precedes the request's arrival.
// Triggers StepEvent if the instance is idle (INV-8: work-conserving).
func (sim *Simulator) EnqueueDecodeSubRequest(r *Request, clusterTime int64) {
	// Pre-processor: decode sub-requests inherit SLOClass from parent (pd_events.go:215).
	// Apply the same vLLM-convention priority as EnqueueRequest.
	if sim.sloMap == nil {
		logrus.Warnf("Simulator.sloMap not initialized — using DefaultSLOPriorityMap; prefer NewSimulator()")
		sim.sloMap = DefaultSLOPriorityMap()
	}
	r.Priority = float64(sim.sloMap.InvertForVLLM(r.SLOClass))

	sim.WaitQ.Enqueue(r)
	// Do NOT add len(r.InputTokens) to TotalInputTokens — already counted by prefill sub-request.

	// Schedule timeout for decode sub-request (R23: parity with EnqueueRequest)
	if r.Deadline > 0 && r.Deadline <= sim.Horizon {
		sim.Schedule(&TimeoutEvent{time: r.Deadline, Request: r})
	}

	// Trigger StepEvent if idle (work-conserving: INV-8).
	// Use max(sim.Clock, clusterTime) so the decode sub-request is not processed
	// at a stale instance time that precedes the cluster time when it was injected.
	if (sim.RunningBatch == nil || len(sim.RunningBatch.Requests) == 0) && sim.stepEvent == nil {
		stepTime := sim.Clock
		if clusterTime > stepTime {
			stepTime = clusterTime
		}
		step := &StepEvent{time: stepTime}
		sim.stepEvent = step
		sim.Schedule(step)
	}
}

// recordQueueSnapshots records the wait queue and running batch sizes at this step.
// Called after batch formation, before execution.
func (sim *Simulator) recordQueueSnapshots() {
	sim.Metrics.NumWaitQRequests = append(sim.Metrics.NumWaitQRequests, sim.WaitQ.Len())
	sim.Metrics.NumRunningBatchRequests = append(sim.Metrics.NumRunningBatchRequests, len(sim.RunningBatch.Requests))
}

// accumulateExternalityPrices computes per-request externality prices for the current step.
// Uses a proportional decomposition: each request's δᵢ is its share of currStepAdvance
// proportional to its contribution weight (new prefill tokens or decode context size).
// Shadow price μ̂ is non-zero only when KV utilization exceeds 0.9 and queue is non-empty.
// This is a purely read-only pass — no state outside the Request accumulators is modified.
func (sim *Simulator) accumulateExternalityPrices(scheduled []*Request, currStepAdvance int64) {
	if len(scheduled) == 0 {
		return
	}

	// Compute total contribution weight for normalization.
	var totalContribution float64
	for _, req := range scheduled {
		if req.ProgressIndex < util.Len64(req.InputTokens) {
			// Prefill: contribution = new tokens being processed
			totalContribution += float64(req.NumNewTokens)
		} else {
			// Decode: contribution = context size (ProgressIndex)
			totalContribution += float64(req.ProgressIndex)
		}
	}
	if totalContribution == 0 {
		return
	}

	// Compute KV utilization and shadow price μ̂.
	stepDur := float64(currStepAdvance)
	kvUtil := float64(sim.KVCache.UsedBlocks()) / float64(sim.KVCache.TotalCapacity())
	var muHat float64
	if kvUtil > 0.9 && sim.WaitQ.Len() > 0 {
		// Shadow price: queue pressure × step duration / total capacity.
		// μ̂ = Λ_Q^wait · T / K, where Λ_Q^wait = WaitQ.Len(), T = step duration, K = total blocks.
		muHat = float64(sim.WaitQ.Len()) * stepDur / float64(sim.KVCache.TotalCapacity())
	}

	// Accumulate per-request prices.
	for _, req := range scheduled {
		var contribution float64
		if req.ProgressIndex < util.Len64(req.InputTokens) {
			contribution = float64(req.NumNewTokens)
		} else {
			contribution = float64(req.ProgressIndex)
		}
		// δᵢ: proportional step-time contribution
		delta := stepDur * contribution / totalContribution
		// κᵢ: blocks held by this request
		kappa := float64(sim.KVCache.BlocksHeldByRequest(req.ID))

		req.ExternalityDeltaSum += delta
		req.ExternalityKappaBlockSteps += kappa * stepDur
		req.ExternalityPStep += stepDur * delta      // Λ_B^step = stepDur; price = Λ_B^step · δᵢ
		req.ExternalityPCap += muHat * kappa
		req.ExternalityStepCount++
		req.ExternalityKVUtilSum += kvUtil
		req.ExternalityHarmScore += delta * float64(sim.WaitQ.Len())
	}
}

// recordKVUsageMetrics records peak and time-weighted KV block usage.
// Called after execution, before completion processing.
func (sim *Simulator) recordKVUsageMetrics(stepDuration int64) {
	used := sim.KVCache.UsedBlocks()
	if used > sim.Metrics.PeakKVBlocksUsed {
		sim.Metrics.PeakKVBlocksUsed = used
	}
	sim.Metrics.KVBlocksUsed += float64(used) * float64(stepDuration)
}

// recordRequestCompletion records per-request metrics for a completed request.
// Called after state transitions (req.State, req.ITL, req.FinishedStepIdx)
// and KV cleanup are done.
//
// NOTE: E2E (lat) includes PostDecodeFixedOverhead and OutputTokenProcessingTime, both of
// which model non-blocking CPU overhead (concurrent with GPU execution). These inflate
// E2E and RequestCompletionTimes beyond the RequestLeftEvent timestamp by the overhead
// amount. This is architecturally intentional: real vLLM's post-processing (detokenization,
// response serialization) is non-blocking but still contributes to client-perceived latency.
// For trained-physics, PostDecodeFixedOverhead adds ~777µs to E2E; for other backends it's 0.
func (sim *Simulator) recordRequestCompletion(req *Request) {
	// INV-1 conservation: Always increment CompletedRequests.
	// For redirected requests: the source instance drained the request from its WaitQ
	// (StillQueued=0 at end), so source contributes 0 to InjectedRequests.
	// The destination is the sole completion site. Skipping CompletedRequests++ here
	// would cause the request to vanish from conservation accounting entirely.
	sim.Metrics.CompletedRequests++
	sim.Metrics.TTFTSum += req.FirstTokenTime

	// Count output tokens at completion time (not inline per step) to avoid
	// double-counting under preemption (ProgressIndex reset to 0 on eviction).
	// PI - InputLen counts decode-step increments (= OutputLen - 1 for normal completion).
	// Add 1 for the prefill-generated first token (#1097) when decodeTokens falls short
	// of OutputLen. PD 1-output decode sub-requests are the exception: their PI_final
	// lands at InputLen+1 (one step past the InputLen threshold), so decodeTokens==OutputLen
	// already — the guard prevents double-counting in that case.
	decodeTokens := int(req.ProgressIndex) - len(req.InputTokens)
	if decodeTokens < len(req.OutputTokens) {
		decodeTokens++ // prefill-generated first token (vLLM parity)
	}
	if decodeTokens > 0 {
		sim.Metrics.TotalOutputTokens += decodeTokens
	}

	var itlSum int64
	for _, v := range req.ITL {
		itlSum += v
	}
	// PostDecodeFixedOverhead: fixed per-request overhead at completion (e.g., response setup).
	// Only applied to requests that went through a decode phase. Zero-output-token requests
	// (prefill-only) skip this overhead since they never entered the post-decode path.
	var postDecodeOverhead int64
	if len(req.OutputTokens) > 0 {
		postDecodeOverhead = sim.latencyModel.PostDecodeFixedOverhead()
	}
	lat := req.FirstTokenTime + itlSum + postDecodeOverhead
	sim.Metrics.RequestE2Es[req.ID] = float64(lat)
	logrus.Debugf("Finished req: ID: %s at time: %d", req.ID, lat+req.ArrivalTime)
	if len(req.OutputTokens) > 0 {
		// Compute average ITL from itlSum directly (not from lat - FirstTokenTime)
		// to avoid contaminating per-token ITL with the fixed post-decode overhead.
		reqTotalOutput := itlSum
		if req.LengthCapped {
			// #588: Use actual decode step count for length-capped requests.
			// len(req.OutputTokens) is the pre-determined count; len(req.ITL) is actual.
			// TPOT convention: exclude first generated token → denominator is len(ITL)-1.
			sim.Metrics.RequestITLs[req.ID] = float64(reqTotalOutput) / float64(max(len(req.ITL)-1, 1))
		} else {
			// TPOT calculation in vLLM excludes the first generated token.
			sim.Metrics.RequestITLs[req.ID] = float64(reqTotalOutput) / float64(max(len(req.OutputTokens)-1, 1))
		}
	} else {
		sim.Metrics.RequestITLs[req.ID] = 0
	}
	sim.Metrics.RequestStepCounters = append(sim.Metrics.RequestStepCounters, req.FinishedStepIdx-req.ScheduledStepIdx)
	sim.Metrics.RequestCompletionTimes[req.ID] = float64(lat + req.ArrivalTime)
	sim.Metrics.AllITLs = append(sim.Metrics.AllITLs, req.ITL...)

	// Externality pricing: emit accumulated values to per-request output.
	if req.ExternalityDeltaSum > 0 {
		// VTC: increment per-tenant cumulative token count, snapshot on request.
		sim.tenantTokenCount[req.TenantID] += int64(len(req.OutputTokens))
		req.ExternalityVTC = sim.tenantTokenCount[req.TenantID]

		kvUtil := float64(sim.KVCache.UsedBlocks()) / float64(sim.KVCache.TotalCapacity())
		var avgKVUtil float64
		if req.ExternalityStepCount > 0 {
			avgKVUtil = req.ExternalityKVUtilSum / float64(req.ExternalityStepCount)
		}
		sim.Metrics.RequestExternality[req.ID] = RequestExternalityMetrics{
			DeltaStepUs:        req.ExternalityDeltaSum,
			KappaBlockSteps:    req.ExternalityKappaBlockSteps,
			PStepUs:            req.ExternalityPStep,
			PCapUs:             req.ExternalityPCap,
			PTotalUs:           req.ExternalityPStep + req.ExternalityPCap,
			KVUtilAtCompletion: kvUtil,
			AvgKVUtil:          avgKVUtil,
			HarmScore:          req.ExternalityHarmScore,
			VTC:                req.ExternalityVTC,
		}
	}

	// Credit enforcement: record per-request credit state at completion.
	// Emitted for any active enforcement policy so callers can inspect throttling behavior.
	if sim.cfg.EnforcementPolicy != "" && sim.cfg.EnforcementPolicy != "none" && req.TenantID != "" {
		sim.Metrics.RequestCredit[req.ID] = RequestCreditMetrics{
			CreditAtCompletion: sim.creditBalance[req.TenantID],
			Throttled:          req.ThrottledDuringRun,
			ThrottleDurationUs: req.ThrottleTotalUs,
		}
	}
}

// Step simulates a single vllm step(): batch scheduling, model execution, mirroring, and completion.
// Phases: (1) schedule batch, (2) execute prefill/decode, (2.5) mirror to CPU, (3) process completions, (4) schedule next step.
//
// Orphaned StepEvent guard: when a TimeoutEvent empties the RunningBatch it leaves
// sim.stepEvent pointing to the already-scheduled StepEvent (preventing the cascade
// described in #1096). If that StepEvent fires and finds nothing to do, clearing
// sim.stepEvent here prevents future QueuedEvent INV-8 guards from seeing a stale
// non-nil pointer and skipping their step-scheduling.
func (sim *Simulator) Step(now int64) {
	if sim.RunningBatch == nil && sim.WaitQ.Len() == 0 {
		sim.stepEvent = nil
		return
	}
	sim.scheduleBatch(now)
	currStepAdvance := sim.executeBatchStep(now)
	// Mirror in-use blocks to CPU tier (no-op for single-tier KVCacheState).
	// Runs after execution (new full blocks exist) and before completions
	// (completing requests' blocks are still in-use and can be mirrored).
	sim.KVCache.MirrorToCPU(sim.RunningBatch.Requests)
	remaining := sim.processCompletions(now, currStepAdvance)
	sim.scheduleNextStep(now, currStepAdvance, remaining)
}

// scheduleBatch handles Phase 1: priority assignment, queue reordering, batch formation,
// and event scheduling for preemptions and newly scheduled requests.
func (sim *Simulator) scheduleBatch(now int64) {
	sim.stepCount += 1

	// Synchronize KV cache clock for thrashing detection (no-op for single-tier KVCacheState)
	sim.KVCache.SetClock(now)

	// Order queue per scheduler policy. Priorities are static — set once at
	// EnqueueRequest/EnqueueDecodeSubRequest via SLOPriorityMap.InvertForVLLM
	// (vLLM static priority model; per-step recomputation removed).
	sim.WaitQ.Reorder(func(reqs []*Request) {
		sim.scheduler.OrderQueue(reqs, now)
	})

	// Policy-specific WaitQ reorder (overwrites scheduler ordering for soft-priority policies).
	// Runs AFTER the scheduler ordering so the policy sort is the final ordering.
	sim.applyPolicyReorder(now)

	// Build CreditGate for gate-based policies (returns nil for reorder-only policies).
	creditGate, throttleNotify := sim.buildCreditGate(now)

	// Delegate batch composition to the pluggable BatchFormation strategy.
	// Event scheduling and metrics recording happen after FormBatch returns (kernel concerns).
	batchCtx := BatchContext{
		RunningBatch:          sim.RunningBatch,
		WaitQ:                 sim.WaitQ,
		KVCache:               sim.KVCache,
		MaxScheduledTokens:    sim.maxScheduledTokens,
		MaxRunningReqs:        sim.maxRunningReqs,
		PrefillTokenThreshold: sim.longPrefillTokenThreshold,
		MaxModelLen:           sim.maxModelLen,
		Now:                   now,
		StepCount:             sim.stepCount,
		ComputedTokens:        sim.reqNumComputedTokens,
		CreditGate:            creditGate,
		ThrottleNotify:        throttleNotify,
		// Self-reference: Simulator implements TenantExternalityTracker.
		// Used by EA-aware preemption (hard victim selection) and soft
		// preemption (per-tick decode-token scaling). Pre-existing
		// PreemptionFCFS / PreemptionPriority paths ignore this field.
		Tracker: sim,
	}
	batchResult := sim.batchFormation.FormBatch(batchCtx)

	// Apply result: update running batch
	sim.RunningBatch = batchResult.RunningBatch

	// Record preemption metrics and emit debug log for each preempted request
	for _, p := range batchResult.Preempted {
		logrus.Debugf("<< Preemption: %s at %d ticks", p.Request.ID, now)
		sim.Metrics.PreemptionCount++
	}

	// Schedule events for newly scheduled requests and record scheduling metrics
	for _, s := range batchResult.NewlyScheduled {
		sim.Schedule(&ScheduledEvent{
			time:    now,
			Request: s.Request,
		})
		sim.Metrics.RequestSchedulingDelays[s.Request.ID] = now - s.Request.ArrivalTime
	}

	// Record queue depth observations after batch formation
	sim.recordQueueSnapshots()
}

// executeBatchStep handles Phase 2: model execution (prefill + decode) for all requests
// in the running batch. Returns the step time advance in ticks.
func (sim *Simulator) executeBatchStep(now int64) int64 {
	// Match vLLM's scheduled_running_reqs: only requests that were allocated
	// tokens by FormBatch participate in the forward pass latency computation.
	// Requests with NumNewTokens=0 (past Phase 1 break point, token budget
	// exhaustion, or MaxModelLen boundary) retain their KV blocks and remain
	// in RunningBatch for the next step, but do not contribute to this step's
	// compute time. See vllm/v1/core/sched/scheduler.py scheduled_running_reqs.
	// Note: scheduled may be empty when all requests are idle (e.g., after
	// Phase 1 preemption cascade). All StepTime backends handle empty batches
	// correctly (return >= 1), and the max(1, ...) floor below guarantees INV-3.
	scheduled := make([]*Request, 0, len(sim.RunningBatch.Requests))
	for _, req := range sim.RunningBatch.Requests {
		if req.NumNewTokens > 0 {
			scheduled = append(scheduled, req)
		}
	}
	currStepAdvance := sim.latencyModel.StepTime(scheduled)

	// Add transfer latency from CPU→GPU reloads (0 for single-tier)
	currStepAdvance += sim.KVCache.ConsumePendingTransferLatency()

	// INV-3 defense-in-depth: guarantee clock advancement regardless of backend.
	// All LatencyModel implementations must return >= 1 per interface contract;
	// this floor catches violations that would cause infinite livelock.
	currStepAdvance = max(1, currStepAdvance)

	// Subprocess: Model Execution - this could be prefill or decode depending on the request.
	// similar to vLLM's execute_model()
	// Note: Per-request TTFT fields (FirstTokenTime, RequestTTFTs) are recorded inline
	// because they are tightly coupled to the prefill/decode state transitions in this
	// loop. Safety across preemption comes from the !req.TTFTSet guard below (TTFTSet is
	// reset to false on preemption by batch_formation.go), not from overwrite idempotency —
	// the guard ensures the block fires exactly once per prefill completion so FirstTokenTime
	// always reflects the final re-prefill. TTFTSum and TotalOutputTokens are computed at
	// completion time in recordRequestCompletion to avoid double-counting when a preempted
	// request re-runs from ProgressIndex=0.
	for _, req := range sim.RunningBatch.Requests {
		if req.ProgressIndex < util.Len64(req.InputTokens) {
			req.ProgressIndex = sim.reqNumComputedTokens[req.ID]
			// ToDo: Go through the newly allocated blocks for this request;
			// Make sure they are cached, if they're full
		} else {
			// Decode phase: only generate a token if FormBatch allocated one.
			// Without this guard, a request at the MaxModelLen boundary (NumNewTokens=0
			// from proactive cap) would get a phantom ProgressIndex increment.
			// Also prevents phantom tokens from token budget exhaustion (pre-existing edge case).
			if req.NumNewTokens > 0 {
				req.ProgressIndex++
				req.ITL = append(req.ITL, currStepAdvance+sim.latencyModel.OutputTokenProcessingTime())
			}
		}
		// !req.TTFTSet guard: fires once per prefill completion (including re-prefill after
		// preemption). TTFTSet is reset to false on preemption (batch_formation.go) so this
		// block fires again on re-prefill, overwriting FirstTokenTime with the correct
		// post-preemption TTFT. req.FirstTokenTime is a scalar assignment (not an
		// accumulation), so overwriting it is safe. TTFTSum is not accumulated here;
		// it is accumulated exactly once at completion time in recordRequestCompletion.
		if req.ProgressIndex == util.Len64(req.InputTokens) && !req.TTFTSet {
			req.TTFTSet = true
			req.FirstTokenTime = now + currStepAdvance + sim.latencyModel.OutputTokenProcessingTime() - req.ArrivalTime
			sim.Metrics.RequestTTFTs[req.ID] = float64(req.FirstTokenTime)
		}
	}

	// Record KV cache usage observations after execution
	sim.recordKVUsageMetrics(currStepAdvance)

	// Externality pricing: passive per-request metering (read-only, no enforcement).
	// Accumulate δᵢ (step-time contribution) and capacity price μ̂·κᵢ for each
	// scheduled request. Uses proportional decomposition: δᵢ = currStepAdvance ×
	// (request_contribution / total_contribution) where contribution is new tokens
	// for prefill and ProgressIndex for decode.
	sim.accumulateExternalityPrices(scheduled, currStepAdvance)

	// Per-policy state updates: credit deduction, virtual-time accumulation, etc.
	// Runs AFTER accumulateExternalityPrices so per-step delta/kappa are already
	// accumulated onto the Request fields.
	sim.updatePolicyState(scheduled, currStepAdvance, now)

	return currStepAdvance
}

// processCompletions handles Phase 3: identifies completed requests, performs state
// transitions, releases KV blocks, and records completion metrics.
// Returns the remaining (non-completed) requests.
//
// IMPORTANT: This MUST run as a separate pass after executeBatchStep (BC-5).
// For zero-output-token requests, both "prefill completed" and "request completed"
// conditions are true in the same step. The two-pass design ensures prefill metrics
// (TTFT) are recorded before completion metrics (E2E). If these were ever
// consolidated into a single pass, both branches would fire for the same request
// in the same step.
func (sim *Simulator) processCompletions(now, currStepAdvance int64) []*Request {
	remaining := []*Request{}
	for _, req := range sim.RunningBatch.Requests {
		// in cases where there are 0 output tokens, set it to 1 manually to avoid errors
		if req.ProgressIndex >= util.Len64(req.InputTokens)+max(util.Len64(req.OutputTokens), 1)-1 {
			// State transitions
			req.State = StateCompleted
			// Zero-output requests complete at prefill end with no decode phase.
			// The guard below has two distinct roles depending on output length:
			//
			// 1-output-token PD decode sub-requests: FormBatch Phase 2 already
			// allocated the single decode token's KV block. After executeBatchStep
			// runs, ProgressIndex = inputLen+1, so the guard
			// (req.ProgressIndex < inputLen+outputLen) evaluates to
			// (inputLen+1) < (inputLen+1) = false — preventing a duplicate allocation.
			//
			// Requests with 2+ output tokens (PD or non-PD): after executeBatchStep
			// runs, ProgressIndex = inputLen+outputLen-1 on the final decode step,
			// so the guard evaluates to true — this is the first and only allocation
			// for the final token.
			// ITL is NOT appended here — executeBatchStep already recorded it
			// for this decode step (fix for #524 phantom ITL entry).
			if len(req.OutputTokens) > 0 && req.ProgressIndex < util.Len64(req.InputTokens)+util.Len64(req.OutputTokens) {
				ok := sim.KVCache.AllocateKVBlocks(req, req.ProgressIndex, req.ProgressIndex+1, []int64{})
				if !ok {
					logrus.Errorf("[tick %07d] KV allocation failed for completing request %s (request will still complete) — this indicates a cache accounting bug", now, req.ID)
					sim.Metrics.KVAllocationFailures++
				}
			}
			// ReleaseKVBlocks is safe even when the final-token allocation failed:
			// the decode pre-check returns false before any state mutation (check-then-act
			// pattern, matching vLLM kv_cache_manager.py:334-336), so RequestMap is
			// preserved and Release frees all blocks from prior successful allocations.
			sim.KVCache.ReleaseKVBlocks(req)
			req.FinishedStepIdx = sim.stepCount
			sim.Schedule(&RequestLeftEvent{
				time:    now + currStepAdvance,
				Request: req,
			})

			// Record completion metrics
			sim.recordRequestCompletion(req)

			// Invoke completion callback for session management
			if sim.OnRequestDone != nil {
				for _, next := range sim.OnRequestDone(req, now+currStepAdvance) {
					sim.InjectArrival(next)
				}
			}
		} else if sim.maxModelLen > 0 && req.ProgressIndex >= sim.maxModelLen-1 {
			// BC-5: Proactive MaxModelLen cap — force-complete at boundary.
			// After the proactive cap in FormBatch prevents scheduling tokens beyond
			// maxModelLen-1, and the decode guard in executeBatchStep prevents phantom
			// ProgressIndex increments, the request reaches PI=maxModelLen-1 and needs
			// a completion path. This matches vLLM's effective behavior where the scheduler
			// cap at max_model_len-1-num_computed prevents further scheduling.
			// Note: vLLM completes length-capped requests via check_stop (num_tokens >= max_model_len)
			// which fires AFTER the model appends the generated token, producing maxModelLen-input
			// output tokens. BLIS completes at PI >= maxModelLen-1 (before the final token),
			// producing maxModelLen-1-input tokens (1 fewer). This is because BLIS lacks vLLM's
			// post-execution check_stop loop; processCompletions is the DES equivalent.
			//
			// NOTE (R23 exception): Final-token KV allocation is intentionally skipped here.
			// The normal completion path's AllocateKVBlocks for the last token is not useful
			// for a force-terminated request whose blocks are immediately released.
			logrus.Warnf("[tick %07d] force-completing request %s: ProgressIndex %d >= MaxModelLen-1 %d (length-capped)",
				now, req.ID, req.ProgressIndex, sim.maxModelLen-1)
			sim.Metrics.LengthCappedRequests++
			req.LengthCapped = true
			// Refresh Metrics.Requests: NewRequestMetrics was called at enqueue before
			// LengthCapped was known. Update so per-request JSON reflects the flag.
			if rm, ok := sim.Metrics.Requests[req.ID]; ok {
				rm.LengthCapped = true
				sim.Metrics.Requests[req.ID] = rm
			}
			req.State = StateCompleted
			sim.KVCache.ReleaseKVBlocks(req)
			req.FinishedStepIdx = sim.stepCount
			sim.Schedule(&RequestLeftEvent{
				time:    now + currStepAdvance,
				Request: req,
			})
			sim.recordRequestCompletion(req)

			// Invoke completion callback for session management (length-capped)
			if sim.OnRequestDone != nil {
				for _, next := range sim.OnRequestDone(req, now+currStepAdvance) {
					sim.InjectArrival(next)
				}
			}
		} else {
			remaining = append(remaining, req)
		}
	}
	return remaining
}

// scheduleNextStep handles Phase 4: schedules the next step event based on
// remaining requests, or starts a new batch if only WaitQ has pending work
// (work-conserving property, INV-8).
func (sim *Simulator) scheduleNextStep(now, currStepAdvance int64, remaining []*Request) {
	if len(remaining) > 0 {
		sim.RunningBatch.Requests = remaining
		// estimate queue overhead from LR (sim.features)
		//
		pbe := StepEvent{time: now + currStepAdvance}
		sim.Schedule(&pbe)
		sim.stepEvent = &pbe
	} else {
		sim.RunningBatch = nil
		sim.stepEvent = nil
		// Work-conserving: if WaitQ has pending requests, immediately
		// schedule a new step to form the next batch. Without this,
		// queued requests are stranded until the next arrival event
		// triggers a QueuedEvent — violating the work-conserving
		// property that real vLLM maintains.
		if sim.WaitQ.Len() > 0 {
			pbe := StepEvent{time: now + currStepAdvance}
			sim.Schedule(&pbe)
			sim.stepEvent = &pbe
		}
	}
}

// ============================================================================
// Enforcement policy helpers
// ============================================================================

// alphaForSLO returns the welfare weight (α) for an SLO class.
// critical=5.0 (interactive, highest value), standard=3.0 (agentic/long-context),
// everything else=1.0 (batch, sheddable, background).
// NOTE: distinct from SLOPriorityMap values (scheduling priorities, different scale).
func alphaForSLO(slo string) float64 {
	switch slo {
	case "critical":
		return 5.0
	case "standard":
		return 3.0
	default:
		return 1.0
	}
}

// tenantKVBlocksHeld returns the number of KV blocks currently held by the given tenant.
// Scans RunningBatch for matching TenantID. O(n) in batch size — acceptable because
// batch sizes are bounded by MaxRunningReqs (~256 max) and this is called once per step.
func (sim *Simulator) tenantKVBlocksHeld(tid string) int64 {
	var total int64
	if sim.RunningBatch == nil {
		return 0
	}
	for _, req := range sim.RunningBatch.Requests {
		if req.TenantID == tid {
			total += sim.KVCache.BlocksHeldByRequest(req.ID)
		}
	}
	return total
}

// applyPolicyReorder applies policy-specific WaitQ ordering after the scheduler ordering.
// Reorder-only policies (wfq, ea-wfq, drf, oracle) override scheduler order.
// Gate-based policies (externality-credit, token-rate, kv-quota) use a stable partition:
// non-throttled requests first, throttled last, to ensure the CreditGate stops at the
// right place in Phase 2.
func (sim *Simulator) applyPolicyReorder(now int64) {
	switch sim.cfg.EnforcementPolicy {

	case "externality-credit":
		// Stable partition: non-throttled (credit >= threshold) first.
		sim.WaitQ.Reorder(func(reqs []*Request) {
			sort.SliceStable(reqs, func(i, j int) bool {
				iThrottled := sim.creditBalance[reqs[i].TenantID] < sim.creditThreshold
				jThrottled := sim.creditBalance[reqs[j].TenantID] < sim.creditThreshold
				return !iThrottled && jThrottled
			})
		})

	case "token-rate":
		// Stable partition: under-quota tenants first.
		elapsedSec := float64(now) / 1e6
		if elapsedSec <= 0 {
			return
		}
		sim.WaitQ.Reorder(func(reqs []*Request) {
			sort.SliceStable(reqs, func(i, j int) bool {
				iOver := sim.isOverTokenQuota(reqs[i].TenantID, elapsedSec)
				jOver := sim.isOverTokenQuota(reqs[j].TenantID, elapsedSec)
				return !iOver && jOver
			})
		})

	case "kv-quota":
		// Stable partition: under-quota tenants first.
		// Pre-compute quota status per tenant to avoid O(batch) scan inside comparison.
		sim.WaitQ.Reorder(func(reqs []*Request) {
			overQuota := make(map[string]bool, sim.numActiveTenants)
			for _, r := range reqs {
				if _, seen := overQuota[r.TenantID]; !seen {
					overQuota[r.TenantID] = sim.isOverKVQuota(r.TenantID)
				}
			}
			sort.SliceStable(reqs, func(i, j int) bool {
				return !overQuota[reqs[i].TenantID] && overQuota[reqs[j].TenantID]
			})
		})

	case "wfq":
		// Sort by virtual time: tenantCumTokens[tid] / alpha ascending (lowest = most deserving).
		sim.WaitQ.Reorder(func(reqs []*Request) {
			sort.SliceStable(reqs, func(i, j int) bool {
				vi := float64(sim.tenantCumTokens[reqs[i].TenantID]) / alphaForSLO(reqs[i].SLOClass)
				vj := float64(sim.tenantCumTokens[reqs[j].TenantID]) / alphaForSLO(reqs[j].SLOClass)
				return vi < vj
			})
		})

	case "ea-wfq":
		// Sort by externality virtual time: tenantCumExternality[tid] / alpha ascending.
		sim.WaitQ.Reorder(func(reqs []*Request) {
			sort.SliceStable(reqs, func(i, j int) bool {
				vi := sim.tenantCumExternality[reqs[i].TenantID] / alphaForSLO(reqs[i].SLOClass)
				vj := sim.tenantCumExternality[reqs[j].TenantID] / alphaForSLO(reqs[j].SLOClass)
				return vi < vj
			})
		})

	case "drf":
		// Sort by dominant resource share ascending (equalize max(step-time-share, KV-share)).
		sim.WaitQ.Reorder(func(reqs []*Request) {
			sort.SliceStable(reqs, func(i, j int) bool {
				di := max(sim.tenantCumStepTime[reqs[i].TenantID], sim.tenantCumKVBlocks[reqs[i].TenantID])
				dj := max(sim.tenantCumStepTime[reqs[j].TenantID], sim.tenantCumKVBlocks[reqs[j].TenantID])
				return di < dj
			})
		})

	case "oracle":
		// Sort by alpha / (1 + cumExternality) descending (highest value-per-externality first).
		sim.WaitQ.Reorder(func(reqs []*Request) {
			sort.SliceStable(reqs, func(i, j int) bool {
				vi := alphaForSLO(reqs[i].SLOClass) / (1.0 + sim.tenantCumExternality[reqs[i].TenantID])
				vj := alphaForSLO(reqs[j].SLOClass) / (1.0 + sim.tenantCumExternality[reqs[j].TenantID])
				return vi > vj // descending
			})
		})

	// "red", "none", "": no reorder needed
	}
}

// isOverTokenQuota returns true when a tenant's token rate exceeds the per-tenant quota.
// Lazily computes perTenantTokenQuota on first call.
func (sim *Simulator) isOverTokenQuota(tid string, elapsedSec float64) bool {
	if elapsedSec <= 0 || tid == "" {
		return false
	}
	if sim.perTenantTokenQuota <= 0 && sim.numActiveTenants > 0 {
		// Compute quota: aggregate_rate × mean_tokens / numActiveTenants
		// Based on 50-tenant workload at rate=12 req/s, ~2790 tokens/req average.
		sim.perTenantTokenQuota = 12.0 * 2790.0 / float64(sim.numActiveTenants)
	}
	if sim.perTenantTokenQuota <= 0 {
		return false
	}
	return float64(sim.tenantCumTokens[tid])/elapsedSec > sim.perTenantTokenQuota
}

// isOverKVQuota returns true when a tenant holds more KV blocks than its fair share.
func (sim *Simulator) isOverKVQuota(tid string) bool {
	if tid == "" || sim.numActiveTenants <= 0 {
		return false
	}
	quota := sim.KVCache.TotalCapacity() / int64(sim.numActiveTenants)
	return sim.tenantKVBlocksHeld(tid) >= quota
}

// buildCreditGate returns the CreditGate and ThrottleNotify functions for the active policy.
// Gate-based policies (externality-credit, token-rate, kv-quota) return a gate.
// Reorder-only policies and RED return nil (no gate).
func (sim *Simulator) buildCreditGate(now int64) (func(*Request) bool, func(*Request, int64)) {
	throttleNotify := func(req *Request, t int64) {
		if !req.ThrottledDuringRun {
			req.ThrottledDuringRun = true
			req.ThrottleStartUs = t
		}
	}

	switch sim.cfg.EnforcementPolicy {
	case "externality-credit":
		return func(req *Request) bool {
			if req.TenantID == "" {
				return true
			}
			credit, ok := sim.creditBalance[req.TenantID]
			if !ok {
				return true // new tenant, full credit
			}
			return credit >= sim.creditThreshold
		}, throttleNotify

	case "token-rate":
		elapsedSec := float64(now) / 1e6
		return func(req *Request) bool {
			return !sim.isOverTokenQuota(req.TenantID, elapsedSec)
		}, throttleNotify

	case "kv-quota":
		return func(req *Request) bool {
			return !sim.isOverKVQuota(req.TenantID)
		}, throttleNotify
	}

	return nil, nil
}

// updatePolicyState performs per-step state updates for the active enforcement policy.
// Called after accumulateExternalityPrices so per-step delta and kappa values are
// already accumulated onto the Request externality accumulators.
func (sim *Simulator) updatePolicyState(scheduled []*Request, currStepAdvance int64, now int64) {
	if len(scheduled) == 0 || sim.cfg.EnforcementPolicy == "" || sim.cfg.EnforcementPolicy == "none" {
		return
	}

	stepDur := float64(currStepAdvance)

	// Compute total contribution weight (shared by multiple policies).
	var totalContrib float64
	for _, req := range scheduled {
		if req.ProgressIndex < util.Len64(req.InputTokens) {
			totalContrib += float64(req.NumNewTokens)
		} else {
			totalContrib += float64(req.ProgressIndex)
		}
	}

	// Compute shadow price μ̂ (same formula as accumulateExternalityPrices, RP-2).
	kvUtil := float64(sim.KVCache.UsedBlocks()) / float64(sim.KVCache.TotalCapacity())
	var muHat float64
	if kvUtil > 0.9 && sim.WaitQ.Len() > 0 {
		muHat = float64(sim.WaitQ.Len()) * stepDur / float64(sim.KVCache.TotalCapacity())
	}

	switch sim.cfg.EnforcementPolicy {

	case "externality-credit":
		// Replenish all known tenants first, then deduct for scheduled requests.
		for tid := range sim.creditBalance {
			sim.creditBalance[tid] += sim.creditRate[tid] * stepDur
		}
		if totalContrib > 0 {
			for _, req := range scheduled {
				if req.TenantID == "" {
					continue
				}
				var contrib float64
				if req.ProgressIndex < util.Len64(req.InputTokens) {
					contrib = float64(req.NumNewTokens)
				} else {
					contrib = float64(req.ProgressIndex)
				}
				delta := stepDur * contrib / totalContrib
				kappa := float64(sim.KVCache.BlocksHeldByRequest(req.ID))
				pTotal := delta + muHat*kappa
				sim.creditBalance[req.TenantID] -= pTotal
			}
		}
		// Update throttle duration for requests currently blocked in WaitQ.
		for _, req := range sim.WaitQ.Items() {
			if req.ThrottledDuringRun && req.ThrottleStartUs > 0 {
				req.ThrottleTotalUs += currStepAdvance
			}
		}

	case "ea-wfq", "oracle":
		// Accumulate cumulative externality for virtual-time ordering.
		if totalContrib > 0 {
			for _, req := range scheduled {
				if req.TenantID == "" {
					continue
				}
				var contrib float64
				if req.ProgressIndex < util.Len64(req.InputTokens) {
					contrib = float64(req.NumNewTokens)
				} else {
					contrib = float64(req.ProgressIndex)
				}
				delta := stepDur * contrib / totalContrib
				kappa := float64(sim.KVCache.BlocksHeldByRequest(req.ID))
				sim.tenantCumExternality[req.TenantID] += delta + muHat*kappa
			}
		}

	case "wfq", "token-rate":
		// Track cumulative new tokens processed (for virtual-time / token-rate gate).
		for _, req := range scheduled {
			if req.TenantID == "" {
				continue
			}
			sim.tenantCumTokens[req.TenantID] += int64(req.NumNewTokens)
		}

	case "drf":
		// Update dominant resource shares: step-time fraction and KV-blocks fraction.
		totalKVBlocks := float64(sim.KVCache.TotalCapacity())
		if totalContrib > 0 && totalKVBlocks > 0 {
			for _, req := range scheduled {
				if req.TenantID == "" {
					continue
				}
				var contrib float64
				if req.ProgressIndex < util.Len64(req.InputTokens) {
					contrib = float64(req.NumNewTokens)
				} else {
					contrib = float64(req.ProgressIndex)
				}
				stepFrac := contrib / totalContrib
				kvFrac := float64(sim.KVCache.BlocksHeldByRequest(req.ID)) / totalKVBlocks
				sim.tenantCumStepTime[req.TenantID] += stepFrac
				sim.tenantCumKVBlocks[req.TenantID] += kvFrac
			}
		}

	// "red", "none": no per-step accumulation needed
	}

	_ = now // now available for future use (e.g., time-series snapshots)
}
