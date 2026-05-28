package sim

import (
	"math"
	"testing"
)

// TestKVPressureFromUtil_BelowThreshold verifies that pressure is zero
// when kvUtil <= 0.9, matching the simulator's μ̂ gate condition.
func TestKVPressureFromUtil_BelowThreshold(t *testing.T) {
	tests := []float64{0.0, 0.1, 0.5, 0.85, 0.89, 0.9}
	for _, util := range tests {
		got := kvPressureFromUtil(util, true) // queue non-empty
		if got != 0 {
			t.Errorf("kvUtil=%.2f: expected pressure=0 (below threshold), got %v", util, got)
		}
	}
}

// TestKVPressureFromUtil_EmptyQueue verifies that pressure is zero when
// the wait queue is empty, regardless of kvUtil. Cache slack at full
// utilization with no waiters means no contention.
func TestKVPressureFromUtil_EmptyQueue(t *testing.T) {
	tests := []float64{0.0, 0.5, 0.9, 0.95, 0.99, 1.0}
	for _, util := range tests {
		got := kvPressureFromUtil(util, false) // queue empty
		if got != 0 {
			t.Errorf("kvUtil=%.2f, empty queue: expected pressure=0, got %v", util, got)
		}
	}
}

// TestKVPressureFromUtil_AboveThreshold verifies pressure ramps from 0
// at kvUtil=0.9 to 1.0 at kvUtil=1.0, capped at 1.0.
func TestKVPressureFromUtil_AboveThreshold(t *testing.T) {
	tests := []struct {
		util, want float64
	}{
		{0.9, 0.0},  // exactly at threshold: returns 0 (kvUtil > 0.9 is strict)
		{0.91, 0.1}, // 1% above threshold
		{0.95, 0.5},
		{0.99, 0.9},
		{1.0, 1.0},
		{1.5, 1.0}, // capped (though kvUtil > 1.0 is normally impossible, we don't crash)
	}
	for _, tt := range tests {
		got := kvPressureFromUtil(tt.util, true)
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("kvUtil=%.2f: expected pressure≈%.2f, got %v", tt.util, tt.want, got)
		}
	}
}

// TestSimulator_TrackerInterface verifies that *Simulator satisfies the
// TenantExternalityTracker contract. Compile-time check; if Simulator
// drops the methods, this fails to build.
func TestSimulator_TrackerInterface(_ *testing.T) {
	var _ TenantExternalityTracker = (*Simulator)(nil)
}
