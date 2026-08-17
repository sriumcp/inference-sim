// Tests for GreedyKVScheduler.ChooseVictims (paper §6 line 644 density-ordered eviction).
//
// Tests assert observable behavior — returned victim indices for given inputs —
// not internal struct state. They survive refactor of ChooseVictims internals.
package kvtime

import (
	"testing"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
)

// setRequestBlocks populates kvCache.RequestMap[reqID] with `numBlocks` synthetic
// block IDs so ChooseVictims sees `numBlocks` blocks resident for that request.
// Lets tests control resident-token totals directly without a full FormBatch run.
func setRequestBlocks(kvc *kv.KVCacheState, reqID string, numBlocks int) {
	blocks := make([]int64, numBlocks)
	for i := range blocks {
		blocks[i] = int64(i) // synthetic IDs; ChooseVictims only reads len()
	}
	kvc.RequestMap[reqID] = blocks
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newGreedyKVTestSched(t *testing.T, balances map[string]float64, totalBlocks, blockSize int64) (*GreedyKVScheduler, *kv.KVCacheState, *BucketManager) {
	t.Helper()
	kvc := kv.NewKVCacheState(totalBlocks, blockSize)
	cfgs := make(map[string]TenantBucketConfig, len(balances))
	for tenant := range balances {
		cfgs[tenant] = TenantBucketConfig{OmegaI: 1.0 / float64(len(balances)), BetaSeconds: 1.0, HSeconds: 0.0}
	}
	bm := NewBucketManager(totalBlocks, blockSize, cfgs)
	for tenant, bal := range balances {
		bm.tenantBalance[tenant] = bal
	}
	return NewGreedyKVScheduler(kvc, nil, bm), kvc, bm
}

func TestKVtime_ChooseVictims_StrictlyHigherScoreEvicts(t *testing.T) {
	// A: low balance, large resident → low density score.
	// B: high balance, small candidate → high density score.
	// Expectation: A is evicted to admit B.
	sched, kvc, _ := newGreedyKVTestSched(t, map[string]float64{
		"A": 100.0,
		"B": 1e10,
	}, 100, 16)

	aReq := &sim.Request{ID: "rA", TenantID: "A", ArrivalTime: 1000}
	setRequestBlocks(kvc, "rA", 50) // 50*16 = 800 resident tokens; score(A) = 100/800 ≈ 0.125
	running := []*sim.Request{aReq}

	bReq := &sim.Request{ID: "rB", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 2000}
	// candScore = 1e10/10 = 1e9, much greater than A's score → A in victims.

	victims := sched.ChooseVictims(bReq, running, 3000)
	if len(victims) != 1 || victims[0] != 0 {
		t.Errorf("expected [0], got %v", victims)
	}
}

func TestKVtime_ChooseVictims_EqualScoresNoEviction(t *testing.T) {
	// Identical balances and identical k_r → identical scores.
	// Strict `<` comparator must NOT evict equal-score incumbents.
	sched, kvc, _ := newGreedyKVTestSched(t, map[string]float64{
		"A": 1000.0,
		"B": 1000.0,
	}, 100, 16)

	aReq := &sim.Request{ID: "rA", TenantID: "A", ArrivalTime: 1000}
	setRequestBlocks(kvc, "rA", 5) // 80 tokens; score = 1000/80 = 12.5
	running := []*sim.Request{aReq}

	bReq := &sim.Request{ID: "rB", TenantID: "B", InputTokens: make([]int, 80), ArrivalTime: 2000}
	// candScore = 1000/80 = 12.5 = A's score; strict < fails.

	victims := sched.ChooseVictims(bReq, running, 3000)
	if len(victims) != 0 {
		t.Errorf("expected no victims (strict <), got %v", victims)
	}
}

func TestKVtime_ChooseVictims_OverdrawnCandidateNoEviction(t *testing.T) {
	// Candidate's tenant has balance ≤ 0 → defensive guard fires, return nil.
	// AllowAdmission should have rejected upstream; this is belt-and-suspenders.
	sched, kvc, _ := newGreedyKVTestSched(t, map[string]float64{
		"A": 1000.0,
		"B": 0.0, // overdrawn
	}, 100, 16)

	aReq := &sim.Request{ID: "rA", TenantID: "A", ArrivalTime: 1000}
	setRequestBlocks(kvc, "rA", 5)
	running := []*sim.Request{aReq}

	bReq := &sim.Request{ID: "rB", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 2000}

	victims := sched.ChooseVictims(bReq, running, 3000)
	if victims != nil {
		t.Errorf("expected nil for overdrawn candidate, got %v", victims)
	}
}

func TestKVtime_ChooseVictims_EmptyRunning(t *testing.T) {
	sched, _, _ := newGreedyKVTestSched(t, map[string]float64{"A": 1000.0}, 100, 16)
	bReq := &sim.Request{ID: "rB", TenantID: "A", InputTokens: make([]int, 10), ArrivalTime: 1000}
	victims := sched.ChooseVictims(bReq, nil, 2000)
	if victims != nil {
		t.Errorf("expected nil for empty running, got %v", victims)
	}
}

func TestKVtime_ChooseVictims_FreshlyAdmittedIncumbentShielded(t *testing.T) {
	// Edge case: incumbent in running has 0 blocks (RequestMap entry empty).
	// Score(A, 0) returns raw bal (huge) per bucket.go:215 special case.
	// Strict-< correctly excludes such incumbents from eviction.
	sched, _, _ := newGreedyKVTestSched(t, map[string]float64{
		"A": 100.0,
		"B": 100.0,
	}, 100, 16)

	aReq := &sim.Request{ID: "rA", TenantID: "A", ArrivalTime: 1000}
	// do NOT setRequestBlocks; RequestMap[rA] does not exist → len() = 0
	running := []*sim.Request{aReq}

	bReq := &sim.Request{ID: "rB", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 2000}
	// candScore = 100/10 = 10. A's score = 100 (raw bal, k_r=0 special case).
	// 100 < 10 is FALSE; A NOT in victims.

	victims := sched.ChooseVictims(bReq, running, 3000)
	if len(victims) != 0 {
		t.Errorf("expected fresh incumbent shielded, got %v", victims)
	}
}

func TestKVtime_ChooseVictims_DeterministicTieBreak(t *testing.T) {
	// Two incumbents with identical score AND identical arrival time, idx differs.
	// Repeated runs must produce identical victim ordering (INV-6).
	runOnce := func() []int {
		sched, kvc, _ := newGreedyKVTestSched(t, map[string]float64{
			"A": 100.0,
			"B": 100.0,
			"C": 1e10,
		}, 100, 16)
		aReq := &sim.Request{ID: "rA", TenantID: "A", ArrivalTime: 1000}
		bReq := &sim.Request{ID: "rB", TenantID: "B", ArrivalTime: 1000}
		setRequestBlocks(kvc, "rA", 5)
		setRequestBlocks(kvc, "rB", 5)
		running := []*sim.Request{aReq, bReq}
		cReq := &sim.Request{ID: "rC", TenantID: "C", InputTokens: make([]int, 10), ArrivalTime: 2000}
		return sched.ChooseVictims(cReq, running, 3000)
	}
	first := runOnce()
	for i := 0; i < 10; i++ {
		next := runOnce()
		if !slicesEqual(first, next) {
			t.Fatalf("non-deterministic: first=%v iter%d=%v", first, i, next)
		}
	}
}
