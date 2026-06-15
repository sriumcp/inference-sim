package sim

import (
	"math"
	"testing"
)

func TestSimulator_OccupancyMeterAccumulates(t *testing.T) {
	sim := mustNewSimulator(t, SimConfig{
		Horizon:             math.MaxInt64,
		Seed:                42,
		KVCacheConfig:       NewKVCacheConfig(100, 4, 0, 0, 0, 0),
		BatchConfig:         NewBatchConfig(10, 1000, 0),
		LatencyCoeffs:       NewLatencyCoeffs([]float64{100, 0.5, 0.5}, []float64{100, 0.1, 50}),
		ModelHardwareConfig: NewModelHardwareConfig(rooflineModelConfig(), rooflineHWCalib(), "", "", 1, 1, false, "roofline", 0),
	})
	sim.EnableOccupancyMeter()
	req := &Request{
		ID:           "r1",
		TenantID:     "H",
		ArrivalTime:  0,
		InputTokens:  []int{1, 2, 3, 4, 5, 6, 7, 8},
		OutputTokens: []int{10, 20, 30},
		State:        StateQueued,
	}
	sim.InjectArrival(req)
	sim.Run()
	if sim.OccupancyMeter() == nil {
		t.Fatal("OccupancyMeter nil after EnableOccupancyMeter")
	}
	if sim.OccupancyMeter().ResidencyBlockUs("H") <= 0 {
		t.Error("expected positive H residency after a completed request")
	}
}
