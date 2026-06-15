package kv

import (
	"testing"

	"github.com/inference-sim/inference-sim/sim"
)

func TestBlocksForRequest_CountsHeldBlocks(t *testing.T) {
	kvc := NewKVCacheState(100, 16)
	req := &sim.Request{ID: "r1", InputTokens: make([]int, 48)} // 3 blocks of prefill
	ok := kvc.AllocateKVBlocks(req, 0, 48, nil)
	if !ok {
		t.Fatal("allocation failed")
	}
	if n := kvc.BlocksForRequest("r1"); n != 3 {
		t.Errorf("BlocksForRequest(r1) = %d, want 3", n)
	}
	if n := kvc.BlocksForRequest("absent"); n != 0 {
		t.Errorf("BlocksForRequest(absent) = %d, want 0", n)
	}
}
