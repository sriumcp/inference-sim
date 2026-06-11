// Tests for KVQuotaScheduler.ChooseVictims (paper §22.3 cap-restoration).
package kvtime

import (
	"testing"

	"github.com/inference-sim/inference-sim/sim"
	"github.com/inference-sim/inference-sim/sim/kv"
)

func newKVQuotaTestSched(omegas map[string]float64, totalBlocks int64) (*KVQuotaScheduler, *kv.KVCacheState) {
	kvc := kv.NewKVCacheState(totalBlocks, 16)
	return NewKVQuotaScheduler(kvc, omegas, totalBlocks), kvc
}

func TestKVQuota_ChooseVictims_OnlyOverCapTenantsTargeted(t *testing.T) {
	// A: 60 blocks > cap 50 (over). B: 20 blocks < cap 50 (within).
	// Victims must include only A's requests, never B's.
	sched, kvc := newKVQuotaTestSched(map[string]float64{"A": 0.5, "B": 0.5}, 100)

	running := []*sim.Request{
		{ID: "rA1", TenantID: "A", ArrivalTime: 100},
		{ID: "rA2", TenantID: "A", ArrivalTime: 200},
		{ID: "rB1", TenantID: "B", ArrivalTime: 150},
	}
	setRequestBlocks(kvc, "rA1", 40)
	setRequestBlocks(kvc, "rA2", 20) // A total = 60 > 50
	setRequestBlocks(kvc, "rB1", 20) // B total = 20 < 50

	cand := &sim.Request{ID: "rNew", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 300}
	victims := sched.ChooseVictims(cand, running, 400)

	if len(victims) != 2 {
		t.Errorf("expected 2 A victims, got %d: %v", len(victims), victims)
	}
	for _, v := range victims {
		if running[v].TenantID != "A" {
			t.Errorf("non-A victim at idx %d: %s tenant=%s", v, running[v].ID, running[v].TenantID)
		}
	}
}

func TestKVQuota_ChooseVictims_FCFSTailOrder(t *testing.T) {
	// A is over cap with 3 running requests at arrival 100, 200, 300.
	// Expected: latest-first → [idx_300, idx_200, idx_100] = [2, 1, 0].
	sched, kvc := newKVQuotaTestSched(map[string]float64{"A": 0.3, "B": 0.5}, 100)

	running := []*sim.Request{
		{ID: "r100", TenantID: "A", ArrivalTime: 100},
		{ID: "r200", TenantID: "A", ArrivalTime: 200},
		{ID: "r300", TenantID: "A", ArrivalTime: 300},
	}
	setRequestBlocks(kvc, "r100", 15)
	setRequestBlocks(kvc, "r200", 15)
	setRequestBlocks(kvc, "r300", 15) // A total = 45, cap = 30 → over

	cand := &sim.Request{ID: "rNew", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 400}
	victims := sched.ChooseVictims(cand, running, 500)

	if len(victims) != 3 {
		t.Fatalf("expected 3 victims, got %d", len(victims))
	}
	expected := []int{2, 1, 0}
	for i, v := range victims {
		if v != expected[i] {
			t.Errorf("position %d: expected idx %d, got %d", i, expected[i], v)
		}
	}
}

func TestKVQuota_ChooseVictims_AllWithinCapNoEviction(t *testing.T) {
	// All tenants strictly within cap → ChooseVictims must return nil
	// (paper §22.3: cap-restoration only, never within-cap eviction).
	sched, kvc := newKVQuotaTestSched(map[string]float64{"A": 0.5, "B": 0.5}, 100)

	running := []*sim.Request{
		{ID: "rA", TenantID: "A", ArrivalTime: 100},
		{ID: "rB", TenantID: "B", ArrivalTime: 200},
	}
	setRequestBlocks(kvc, "rA", 30)
	setRequestBlocks(kvc, "rB", 30)

	cand := &sim.Request{ID: "rNew", TenantID: "A", InputTokens: make([]int, 10), ArrivalTime: 300}
	victims := sched.ChooseVictims(cand, running, 400)
	if victims != nil {
		t.Errorf("expected nil, got %v", victims)
	}
}

func TestKVQuota_ChooseVictims_DeterministicAcrossRuns(t *testing.T) {
	// Two over-cap tenants, each with two requests at identical arrivals.
	// Verifies the R2 fix: map iteration order does NOT leak into output.
	runOnce := func() []int {
		sched, kvc := newKVQuotaTestSched(map[string]float64{"A": 0.3, "B": 0.3}, 100)
		running := []*sim.Request{
			{ID: "rA1", TenantID: "A", ArrivalTime: 100},
			{ID: "rB1", TenantID: "B", ArrivalTime: 100},
			{ID: "rA2", TenantID: "A", ArrivalTime: 200},
			{ID: "rB2", TenantID: "B", ArrivalTime: 200},
		}
		setRequestBlocks(kvc, "rA1", 20)
		setRequestBlocks(kvc, "rA2", 20) // A=40 > 30
		setRequestBlocks(kvc, "rB1", 20)
		setRequestBlocks(kvc, "rB2", 20) // B=40 > 30
		cand := &sim.Request{ID: "rNew", InputTokens: make([]int, 10), ArrivalTime: 300}
		return sched.ChooseVictims(cand, running, 400)
	}

	first := runOnce()
	for i := 0; i < 20; i++ {
		next := runOnce()
		if !slicesEqual(first, next) {
			t.Fatalf("non-deterministic at iter %d: first=%v this=%v", i, first, next)
		}
	}
}

func TestKVQuota_IsServeable_OversizedDropped(t *testing.T) {
	// Per-tenant cap = 0.45 * 100 = 45 blocks. A request with prefill 50 blocks
	// (50*16 = 800 tokens) cannot fit in any completion sequence → IsServeable
	// must return false with a descriptive reason.
	sched, _ := newKVQuotaTestSched(map[string]float64{"A": 0.45}, 100)

	oversized := &sim.Request{ID: "rOver", TenantID: "A", InputTokens: make([]int, 800)}
	serveable, reason := sched.IsServeable(oversized)
	if serveable {
		t.Errorf("expected oversized request rejected, got serveable=true reason=%q", reason)
	}
	if reason == "" {
		t.Error("expected non-empty rejection reason")
	}
}

func TestKVQuota_IsServeable_NormalSizedAdmitted(t *testing.T) {
	// Cap = 45 blocks. A 10-block request (160 tokens) is well under cap.
	sched, _ := newKVQuotaTestSched(map[string]float64{"A": 0.45}, 100)

	normal := &sim.Request{ID: "rOK", TenantID: "A", InputTokens: make([]int, 160)}
	serveable, reason := sched.IsServeable(normal)
	if !serveable {
		t.Errorf("expected normal-sized admitted, got serveable=false reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason for serveable, got %q", reason)
	}
}

func TestKVQuota_IsServeable_AtCapBoundary(t *testing.T) {
	// Cap = 45 blocks exactly. A 45-block prefill (720 tokens) fits exactly.
	// A 46-block prefill (721+ tokens, ceil(721/16)=46) does not.
	sched, _ := newKVQuotaTestSched(map[string]float64{"A": 0.45}, 100)

	atCap := &sim.Request{ID: "rAt", TenantID: "A", InputTokens: make([]int, 720)}
	if ok, reason := sched.IsServeable(atCap); !ok {
		t.Errorf("expected at-cap request serveable, got false reason=%q", reason)
	}

	overByOne := &sim.Request{ID: "rOver1", TenantID: "A", InputTokens: make([]int, 721)}
	if ok, _ := sched.IsServeable(overByOne); ok {
		t.Error("expected over-by-one request rejected")
	}
}

func TestKVQuota_IsServeable_UntenantedAdmitted(t *testing.T) {
	// Untenanted requests bypass the per-tenant cap (no tenant means no cap).
	sched, _ := newKVQuotaTestSched(map[string]float64{"A": 0.45}, 100)
	huge := &sim.Request{ID: "rUntenanted", TenantID: "", InputTokens: make([]int, 5000)}
	if ok, _ := sched.IsServeable(huge); !ok {
		t.Error("untenanted requests should bypass per-tenant cap")
	}
}

func TestKVQuota_IsServeable_NilRequest(t *testing.T) {
	sched, _ := newKVQuotaTestSched(map[string]float64{"A": 0.45}, 100)
	if ok, _ := sched.IsServeable(nil); !ok {
		t.Error("nil request should not panic; expected true")
	}
}

func TestKVQuota_ChooseVictims_UntenantedRequestsIgnored(t *testing.T) {
	// Untenanted (TenantID == "") requests are outside the per-tenant quota
	// model — must not appear in victims even when other tenants are over cap.
	sched, kvc := newKVQuotaTestSched(map[string]float64{"A": 0.3, "B": 0.5}, 100)

	running := []*sim.Request{
		{ID: "rUntenanted", TenantID: "", ArrivalTime: 50},
		{ID: "rA1", TenantID: "A", ArrivalTime: 100},
		{ID: "rA2", TenantID: "A", ArrivalTime: 200},
	}
	setRequestBlocks(kvc, "rUntenanted", 20)
	setRequestBlocks(kvc, "rA1", 20)
	setRequestBlocks(kvc, "rA2", 20) // A=40 > 30; untenanted not counted

	cand := &sim.Request{ID: "rNew", TenantID: "B", InputTokens: make([]int, 10), ArrivalTime: 300}
	victims := sched.ChooseVictims(cand, running, 400)

	if len(victims) != 2 {
		t.Errorf("expected 2 A victims, got %v", victims)
	}
	for _, v := range victims {
		if running[v].TenantID == "" {
			t.Errorf("untenanted request at idx %d included in victims", v)
		}
	}
}
