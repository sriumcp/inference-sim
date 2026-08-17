// Tests for meteredScheduler interface forwarding (Step 4 of P0 spec).
//
// These tests exist because v1's meteredScheduler.inner was a NAMED field, so
// Go did not promote inner methods (e.g. KVQuotaScheduler.AllowAdmission) onto
// the outer wrapper — and the simulator's type assertions at simulator.go:702
// silently dropped them. The tests verify that v2's explicit forwarders
// restore reachability for admission-aware and preemption-aware inners.
package main

import (
	"testing"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
	"github.com/inference-sim/inference-sim/sim/kvtime"
)

func TestMeteredScheduler_AdmissionAwareForwarding_KVQuota(t *testing.T) {
	kvc := kv.NewKVCacheState(100, 16)
	inner := kvtime.NewKVQuotaScheduler(kvc, map[string]float64{"A": 0.5}, 100)

	var simRef *sim.Simulator
	m := newMeteredScheduler(inner, nil, kvc, &simRef)

	// Outer must satisfy AdmissionAwareScheduler so the simulator's type
	// assertion at simulator.go:702 succeeds.
	if _, ok := interface{}(m).(sim.AdmissionAwareScheduler); !ok {
		t.Fatal("meteredScheduler with KVQuotaScheduler inner does not satisfy AdmissionAwareScheduler")
	}

	// Forwarder should reach inner.AllowAdmission. Empty running batch +
	// small request → cap is satisfied → admission allowed.
	req := &sim.Request{ID: "r1", TenantID: "A", InputTokens: make([]int, 10)}
	if got := m.AllowAdmission(req, 0); !got {
		t.Errorf("expected admission allowed (within cap), got false")
	}
}

func TestMeteredScheduler_PreemptionAwareForwarding_KVQuota(t *testing.T) {
	kvc := kv.NewKVCacheState(100, 16)
	inner := kvtime.NewKVQuotaScheduler(kvc, map[string]float64{"A": 0.3, "B": 0.5}, 100)

	var simRef *sim.Simulator
	m := newMeteredScheduler(inner, nil, kvc, &simRef)

	// Outer must satisfy PreemptionAwareScheduler.
	if _, ok := interface{}(m).(sim.PreemptionAwareScheduler); !ok {
		t.Fatal("meteredScheduler with KVQuotaScheduler inner does not satisfy PreemptionAwareScheduler")
	}

	// Test forwarding: A is over cap (40 blocks > 30 cap).
	aReq := &sim.Request{ID: "rA", TenantID: "A", ArrivalTime: 100}
	kvc.RequestMap["rA"] = make([]int64, 40)
	cand := &sim.Request{ID: "rB", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 200}

	victims := m.ChooseVictims(cand, []*sim.Request{aReq}, 300)
	if len(victims) != 1 || victims[0] != 0 {
		t.Errorf("expected [0] from forwarded ChooseVictims, got %v", victims)
	}
}

func TestMeteredScheduler_FCFSInnerSafeDefaults(t *testing.T) {
	// FCFSScheduler implements InstanceScheduler but neither AdmissionAware
	// nor PreemptionAware. Forwarders must return safe no-op defaults.
	inner := &kvtime.FCFSScheduler{}
	kvc := kv.NewKVCacheState(100, 16)

	var simRef *sim.Simulator
	m := newMeteredScheduler(inner, nil, kvc, &simRef)

	// Outer satisfies the interfaces (defensive forwarding) but inner does not,
	// so cached fields stay nil.
	if m.innerAdmit != nil {
		t.Error("innerAdmit should be nil for FCFS inner")
	}
	if m.innerPreempt != nil {
		t.Error("innerPreempt should be nil for FCFS inner")
	}

	// AllowAdmission must default to true (no veto, matches v1 nil-callback behavior).
	req := &sim.Request{ID: "r1", TenantID: "A", InputTokens: make([]int, 10)}
	if !m.AllowAdmission(req, 0) {
		t.Error("expected true (no veto) for FCFS inner")
	}

	// ChooseVictims must default to nil (no eviction proposed).
	if v := m.ChooseVictims(req, []*sim.Request{}, 0); v != nil {
		t.Errorf("expected nil for FCFS inner, got %v", v)
	}
}

func TestMeteredScheduler_EnqueueValidatorForwarding_KVQuota(t *testing.T) {
	// KVQuotaScheduler implements EnqueueValidatorScheduler. The wrapper must
	// forward IsServeable so EnqueueRequest's Guard 3 type assertion reaches it.
	kvc := kv.NewKVCacheState(100, 16)
	inner := kvtime.NewKVQuotaScheduler(kvc, map[string]float64{"A": 0.45}, 100)

	var simRef *sim.Simulator
	m := newMeteredScheduler(inner, nil, kvc, &simRef)

	if _, ok := interface{}(m).(sim.EnqueueValidatorScheduler); !ok {
		t.Fatal("meteredScheduler with KVQuotaScheduler inner does not satisfy EnqueueValidatorScheduler")
	}
	if m.innerValidator == nil {
		t.Fatal("innerValidator not populated")
	}

	// Oversized: 800 tokens = 50 blocks, cap = 45 → reject.
	oversized := &sim.Request{ID: "rOver", TenantID: "A", InputTokens: make([]int, 800)}
	serveable, reason := m.IsServeable(oversized)
	if serveable {
		t.Errorf("expected oversized rejected via forwarder, got true reason=%q", reason)
	}

	// Normal-sized: serveable.
	normal := &sim.Request{ID: "rOK", TenantID: "A", InputTokens: make([]int, 160)}
	if ok, _ := m.IsServeable(normal); !ok {
		t.Error("expected normal-sized serveable via forwarder")
	}
}

func TestMeteredScheduler_FCFSInner_IsServeable_DefaultsTrue(t *testing.T) {
	// FCFSScheduler does not implement EnqueueValidatorScheduler. The wrapper
	// must return (true, "") for everything (no scheduler-level rejection).
	inner := &kvtime.FCFSScheduler{}
	kvc := kv.NewKVCacheState(100, 16)

	var simRef *sim.Simulator
	m := newMeteredScheduler(inner, nil, kvc, &simRef)

	if m.innerValidator != nil {
		t.Error("innerValidator should be nil for FCFS inner")
	}

	huge := &sim.Request{ID: "rHuge", TenantID: "A", InputTokens: make([]int, 99999)}
	if ok, _ := m.IsServeable(huge); !ok {
		t.Error("FCFS inner has no scheduler-level rejection; expected true")
	}
}

// ─── τ_idle tracker tests ───────────────────────────────────────────────────

// TestBacklogTracker_TauIdle_Accumulates verifies the τ_idle tracker accumulates
// post-warmup intervals where cache_used < idleThresholdBlocks AND queue is
// non-empty. Sets snapshotIntervalUs = 1000 µs and constructs a synthetic stream
// of ProgressSnapshots; asserts tau_idle equals the sum of qualifying intervals.
func TestBacklogTracker_TauIdle_Accumulates(t *testing.T) {
	const intervalUs = int64(1000)
	const warmupUs = int64(5000)
	const totalBlocks = int64(100)
	const thresholdBlocks = int64(90) // η=0.9
	bt := newBacklogTracker(warmupUs, intervalUs, thresholdBlocks)

	// Helper: build a snapshot at clock with given queueDepth and used blocks.
	mk := func(clock int64, queueDepth int, used int64) sim.ProgressSnapshot {
		return sim.ProgressSnapshot{
			Clock: clock,
			InstanceSnapshots: []sim.InstanceSnapshot{
				{
					QueueDepth:    queueDepth,
					KVTotalBlocks: totalBlocks,
					KVFreeBlocks:  totalBlocks - used,
				},
			},
		}
	}

	// During warmup: shouldn't count.
	bt.OnProgress(mk(1000, 5, 50)) // qualifies but warmup
	bt.OnProgress(mk(2000, 5, 50)) // qualifies but warmup
	if bt.tauIdleUs != 0 {
		t.Errorf("warmup tau_idle should be 0, got %d", bt.tauIdleUs)
	}

	// Post-warmup, all conditions met for 5 consecutive 1ms intervals.
	bt.OnProgress(mk(6000, 5, 50)) // first post-warmup; dt clamped to warmupUs => 6000-5000 = 1000
	bt.OnProgress(mk(7000, 5, 50))
	bt.OnProgress(mk(8000, 5, 50))
	bt.OnProgress(mk(9000, 5, 50))
	bt.OnProgress(mk(10000, 5, 50))
	if bt.tauIdleUs != 5000 {
		t.Errorf("expected 5000 µs after 5 qualifying snapshots, got %d", bt.tauIdleUs)
	}

	// Cache full (used >= threshold): should NOT count.
	bt.OnProgress(mk(11000, 5, 95)) // 95 >= 90 → cache NOT below threshold
	if bt.tauIdleUs != 5000 {
		t.Errorf("expected unchanged after cache-full snap, got %d", bt.tauIdleUs)
	}

	// Queue empty: should NOT count.
	bt.OnProgress(mk(12000, 0, 50)) // queue empty
	if bt.tauIdleUs != 5000 {
		t.Errorf("expected unchanged after queue-empty snap, got %d", bt.tauIdleUs)
	}

	// Resume qualifying: dt = 13000 - 12000 = 1000 µs (correct: prev was 12000).
	bt.OnProgress(mk(13000, 5, 50))
	if bt.tauIdleUs != 6000 {
		t.Errorf("expected 6000 µs after resumed snap, got %d", bt.tauIdleUs)
	}
}

// TestBacklogTracker_TauIdle_WarmupBoundary verifies that when a step jumps
// across the warmup boundary, only the post-warmup portion counts toward τ_idle.
func TestBacklogTracker_TauIdle_WarmupBoundary(t *testing.T) {
	const intervalUs = int64(1000)
	const warmupUs = int64(10000)
	const totalBlocks = int64(100)
	const thresholdBlocks = int64(90)
	bt := newBacklogTracker(warmupUs, intervalUs, thresholdBlocks)

	mk := func(clock int64, qd int, used int64) sim.ProgressSnapshot {
		return sim.ProgressSnapshot{
			Clock: clock,
			InstanceSnapshots: []sim.InstanceSnapshot{
				{QueueDepth: qd, KVTotalBlocks: totalBlocks, KVFreeBlocks: totalBlocks - used},
			},
		}
	}

	// Last warmup snapshot at 9 ms; qualifies but warmup → ignored.
	bt.OnProgress(mk(9000, 5, 50))
	// Step jumps to 15 ms (post-warmup). dt should be clamped: 15000 - max(9000, 10000) = 5000 µs.
	bt.OnProgress(mk(15000, 5, 50))
	if bt.tauIdleUs != 5000 {
		t.Errorf("expected 5000 µs (clamped to post-warmup window), got %d", bt.tauIdleUs)
	}
}

// TestBacklogTracker_NonEmptyCount_Independent verifies the existing
// nonEmptyCount metric is not affected by the τ_idle additions.
func TestBacklogTracker_NonEmptyCount_Independent(t *testing.T) {
	bt := newBacklogTracker(0, 1000, 90)
	mk := func(qd int) sim.ProgressSnapshot {
		return sim.ProgressSnapshot{
			Clock: 1,
			InstanceSnapshots: []sim.InstanceSnapshot{
				{QueueDepth: qd, KVTotalBlocks: 100, KVFreeBlocks: 50},
			},
		}
	}
	bt.OnProgress(mk(0))
	bt.OnProgress(mk(5))
	bt.OnProgress(mk(0))
	bt.OnProgress(mk(3))
	if bt.nonEmptyCount != 2 {
		t.Errorf("expected 2 non-empty snapshots, got %d", bt.nonEmptyCount)
	}
}

func TestMeteredScheduler_RequestRRInnerSafeDefaults(t *testing.T) {
	// RequestRRScheduler is another non-AdmissionAware competitor.
	inner := kvtime.NewRequestRRScheduler()
	kvc := kv.NewKVCacheState(100, 16)

	var simRef *sim.Simulator
	m := newMeteredScheduler(inner, nil, kvc, &simRef)

	if m.innerAdmit != nil || m.innerPreempt != nil {
		t.Error("inner cached fields should be nil for RequestRRScheduler")
	}

	req := &sim.Request{ID: "r1", TenantID: "A", InputTokens: make([]int, 10)}
	if !m.AllowAdmission(req, 0) {
		t.Error("expected true (no veto) for RequestRR inner")
	}
	if v := m.ChooseVictims(req, []*sim.Request{}, 0); v != nil {
		t.Errorf("expected nil, got %v", v)
	}
}
