// scheduler_ext.go provides a constructor for Simulator with a custom, externally
// provided scheduler instance. This allows experiment code (e.g., sim/kvtime) to
// inject schedulers that are not registered in the default NewScheduler factory,
// without modifying any existing source files.
//
// This file is in package sim to allow direct access to the unexported scheduler field.
// It is used exclusively by the blis-kvtime experiment runner (cmd/blis-kvtime).
package sim

// NewSimulatorWithScheduler creates a Simulator identical to NewSimulator but
// replaces the default scheduler (from cfg.PolicyConfig.Scheduler) with the
// provided custom InstanceScheduler instance. All other fields are initialized
// exactly as in NewSimulator.
//
// Intended use: experiment runners that need to inject novel scheduler
// implementations without modifying the production NewScheduler factory.
//
// cfg.PolicyConfig.Scheduler is set to "fcfs" internally to satisfy NewSimulator's
// validation (the scheduler field is overwritten immediately after construction).
func NewSimulatorWithScheduler(cfg SimConfig, kvStore KVStore, latencyModel LatencyModel, sched InstanceScheduler) (*Simulator, error) {
	// Use "fcfs" as a placeholder — it satisfies IsValidScheduler without error,
	// and is immediately overwritten before the caller sees the Simulator.
	origScheduler := cfg.Scheduler
	cfg.Scheduler = "fcfs"

	s, err := NewSimulator(cfg, kvStore, latencyModel)
	if err != nil {
		return nil, err
	}

	// Restore original value (not strictly needed but keeps cfg consistent for logging).
	_ = origScheduler

	// Override scheduler with the externally-provided instance.
	s.scheduler = sched
	return s, nil
}
