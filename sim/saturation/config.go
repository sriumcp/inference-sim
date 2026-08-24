// sim/saturation/config.go
package saturation

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SaturationConfig is the strict-YAML replacement for the 11 saturation tuning
// flags (#1516). It carries one optional block per parameterized detector:
//
//   - threshold:      the ThresholdDetector's single knob (threshold_ms)
//   - backlog_drift:  the BacklogDriftDetector's tuning knobs (mirrors
//     BacklogDriftConfig)
//
// composite gained a single knob (sensitivity) for FPR calibration, and
// swd/owd carry the work-drift block; every parameterized detector now has
// exactly one block, which is what makes an equal-FPR comparison possible.
//
// Fields are pointers so an absent key keeps the detector's default while a
// present key overrides only the field it names (R9: distinguish "unset" from
// "zero"). An empty file parses to a SaturationConfig with all-nil blocks, which
// means "all defaults" — not an error.
type SaturationConfig struct {
	Threshold    *ThresholdBlock    `yaml:"threshold,omitempty"`
	BacklogDrift *BacklogDriftBlock `yaml:"backlog_drift,omitempty"`
	Composite    *CompositeBlock    `yaml:"composite,omitempty"`
	SWD          *WorkDriftBlock    `yaml:"swd,omitempty"`
	OWD          *WorkDriftBlock    `yaml:"owd,omitempty"`
}

// CompositeBlock overrides the CompositeDetector's noise-floor multiplier.
// composite had NO tunable parameters until the FPR-calibration work: an
// equal-false-alarm-rate comparison (metamorphic_tests.md §3.4) is impossible
// for a detector whose sensitivity cannot be moved, so scores against it were
// not commensurable with the others.
type CompositeBlock struct {
	Sensitivity *float64 `yaml:"sensitivity"`
}

// WorkDriftBlock overrides the SWD/OWD work-conservation residual detectors
// (detection_strategies.md §2e). Shared by both because they differ only in how
// the threshold h is obtained (SWD computes it from the spec's burst envelope;
// OWD learns it from a running quantile) -- every other knob is identical.
type WorkDriftBlock struct {
	Kappa0       *float64 `yaml:"kappa0"`         // prefill:decode cost prior
	RDec0        *float64 `yaml:"rdec0"`          // seeded drain rate, tok-equiv/sec
	Threshold    *float64 `yaml:"threshold"`      // h (SWD: envelope; OWD: floor)
	WindowSizeMs *int     `yaml:"window_size_ms"` // busy-window width
	NumWindows   *int     `yaml:"num_windows"`    // ridge-fit ring size
	ConsecutiveK *int     `yaml:"consecutive_k"`  // breaches before firing
	Quantile     *float64 `yaml:"quantile"`       // OWD learned-threshold quantile
	FreezeRDec   *bool    `yaml:"freeze_rdec"`    // pin r_dec => the VWD baseline
	FreezeKappa  *bool    `yaml:"freeze_kappa"`   // pin kappa (kappa=0 => VWD)
}

// ThresholdBlock overrides the ThresholdDetector's mean-E2E threshold.
type ThresholdBlock struct {
	ThresholdMs *float64 `yaml:"threshold_ms"`
}

// BacklogDriftBlock overrides fields of BacklogDriftConfig. Each field
// is optional; absent fields keep DefaultBacklogDriftConfig's value.
// window_size_sec is expressed in whole seconds (matching the retired
// --saturation-window flag, which was also seconds).
type BacklogDriftBlock struct {
	WindowSizeSec       *int     `yaml:"window_size_sec"`
	MinWindows          *int     `yaml:"min_windows"`
	PeakRatio           *float64 `yaml:"peak_ratio"`
	PeakRatioBand       *float64 `yaml:"peak_ratio_band"`
	ConfidenceCI        *float64 `yaml:"confidence_ci"`
	WarmupWindows       *int     `yaml:"warmup_windows"`
	TailWindows         *int     `yaml:"tail_windows"`
	SaturatedDrainRatio *float64 `yaml:"saturated_drain_ratio"`
	TransientDrainRatio *float64 `yaml:"transient_drain_ratio"`
}

// LoadSaturationConfig reads and strictly parses a saturation config file. An
// empty path returns the zero config (all defaults) without touching disk.
// Unknown keys (including a "composite:" block) error via KnownFields(true).
func LoadSaturationConfig(path string) (SaturationConfig, error) {
	var cfg SaturationConfig
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read saturation config %s: %w", path, err)
	}
	// An empty file is valid — decode leaves cfg at its zero value (all defaults).
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse saturation config %s: %w", path, err)
	}
	return cfg, nil
}

// defaultThresholdMs is the ThresholdDetector's default mean-E2E threshold when
// no threshold.threshold_ms override is supplied (matches the retired
// --saturation-threshold-ms default and NewThresholdDetector's own fallback).
const defaultThresholdMs = 5000.0

// BuildDetector constructs the named detector, applying any relevant overrides
// from cfg. Returns an error (never panics — R6) when a name is unknown, a
// supplied parameter is out of range, or a config block is present that does not
// belong to the selected detector; the error names the offending field.
//
// This is the SINGLE-detector entry point (#1516): it enforces block↔detector
// ownership (checkBlockOwnership) because exactly one detector runs, so any
// foreign block is a user mistake. The bank (#1519) drives several detectors and
// enforces ownership over the selected SET once (checkBlockOwnershipSet in
// NewBank), then calls buildDetector per detector — so a block whose owner is not
// in the bank's selection is likewise a hard error, not a silent drop (R1).
func BuildDetector(name string, cfg SaturationConfig) (Detector, error) {
	// Reject config blocks that do not belong to the selected detector rather
	// than silently dropping the user's tuning (R1). SaturationConfig always
	// knows both keys (strict parsing can't tell which detector is active), so
	// the block↔detector match is enforced here.
	if err := checkBlockOwnership(name, cfg); err != nil {
		return nil, err
	}
	return buildDetector(name, cfg)
}

// buildDetector constructs the named detector, applying only the block that
// belongs to name and ignoring the rest of cfg. It does NOT enforce block
// ownership — that is the caller's job (checkBlockOwnership for the single-detector
// path, checkBlockOwnershipSet for the bank). It still validates the values of the
// block it reads (range/finiteness), so a selected detector with an out-of-range
// parameter errors (never panics — R6).
func buildDetector(name string, cfg SaturationConfig) (Detector, error) {
	switch name {
	case "composite":
		sens := 1.0
		if cfg.Composite != nil && cfg.Composite.Sensitivity != nil {
			sens = *cfg.Composite.Sensitivity
			if sens <= 0 || math.IsNaN(sens) || math.IsInf(sens, 0) {
				return nil, fmt.Errorf("saturation config: composite.sensitivity must be a finite value > 0, got %v", sens)
			}
		}
		return NewCompositeDetectorWithSensitivity(sens), nil
	case "swd", "owd":
		wd, err := resolveWorkDriftConfig(name, cfg)
		if err != nil {
			return nil, err
		}
		return NewWorkDriftDetector(wd), nil
	case "threshold":
		thresholdMs := defaultThresholdMs
		if cfg.Threshold != nil && cfg.Threshold.ThresholdMs != nil {
			thresholdMs = *cfg.Threshold.ThresholdMs
			if thresholdMs <= 0 || math.IsNaN(thresholdMs) || math.IsInf(thresholdMs, 0) {
				return nil, fmt.Errorf("saturation config: threshold.threshold_ms must be a finite value > 0, got %v", thresholdMs)
			}
		}
		return NewThresholdDetector(thresholdMs), nil
	case "backlog-drift":
		bdc, err := resolveBacklogDriftConfig(cfg.BacklogDrift)
		if err != nil {
			return nil, err
		}
		return NewBacklogDriftDetectorWithConfig(bdc), nil
	default:
		return nil, fmt.Errorf("unknown saturation detector %q; valid: composite, threshold, backlog-drift, swd, owd", name)
	}
}

// checkBlockOwnership rejects a config that carries a tuning block for a detector
// other than the selected one. composite has no tunable params, so ANY block is
// a mistake when composite is selected; threshold accepts only threshold:;
// backlog-drift accepts only backlog_drift:.
func checkBlockOwnership(name string, cfg SaturationConfig) error {
	// Phrased WITHOUT quoting the name, preserving the single-detector message
	// users and tests already match on; the bank's variant names the whole
	// selection instead. Both read the same blockOwners table, so the two can
	// never disagree about WHICH block belongs to WHOM.
	for _, bo := range blockOwners() {
		if bo.present(cfg) && bo.owner != name {
			if name == "composite" && cfg.Composite == nil {
				return fmt.Errorf("saturation config: %s block is not valid for --detectors composite (composite has no tunable parameters beyond sensitivity)", bo.block)
			}
			return fmt.Errorf("saturation config: %s block is not valid for --detectors %s", bo.block, name)
		}
	}
	return nil
}

// blockOwners maps each config block to the detector that owns it. ONE table
// drives both the single-detector and bank ownership checks, so registering a
// new parameterized detector means adding one row here rather than editing two
// parallel switch statements that can silently drift apart.
//
// present(cfg) reports whether the block appears in the parsed config; the
// pointer-per-block design (R9) is what makes "absent" distinguishable from
// "present but zero".
func blockOwners() []struct {
	block   string
	owner   string
	present func(SaturationConfig) bool
} {
	return []struct {
		block   string
		owner   string
		present func(SaturationConfig) bool
	}{
		{"threshold", "threshold", func(c SaturationConfig) bool { return c.Threshold != nil }},
		{"backlog_drift", "backlog-drift", func(c SaturationConfig) bool { return c.BacklogDrift != nil }},
		{"composite", "composite", func(c SaturationConfig) bool { return c.Composite != nil }},
		{"swd", "swd", func(c SaturationConfig) bool { return c.SWD != nil }},
		{"owd", "owd", func(c SaturationConfig) bool { return c.OWD != nil }},
	}
}

// checkBlockOwnershipSet is the multi-detector generalization of
// checkBlockOwnership for the bank (#1519). A tuning block is valid only if the
// detector that owns it is among the selected names; a block whose owner is NOT
// selected is a hard error rather than a silent drop (R1), matching the
// single-detector path's contract. `--detectors all` selects every owner, so it
// trivially passes; the check bites only on subset selections that omit a
// detector whose block the user nonetheless supplied.
//
// composite owns no block, so it never appears here as an owner — a threshold:
// or backlog_drift: block is justified purely by threshold / backlog-drift being
// in the selection.
func checkBlockOwnershipSet(names []string, cfg SaturationConfig) error {
	selected := make(map[string]bool, len(names))
	for _, n := range names {
		selected[n] = true
	}
	for _, bo := range blockOwners() {
		if bo.present(cfg) && !selected[bo.owner] {
			return fmt.Errorf("saturation config: %s block is not valid for --detectors %q (%s is not among the selected detectors)",
				bo.block, strings.Join(names, ","), bo.owner)
		}
	}
	return nil
}

// resolveBacklogDriftConfig merges a BacklogDriftBlock over the defaults and
// validates the result, returning errors (naming the YAML field) rather than
// panicking so the library boundary stays panic-free (R6). Bounds mirror
// NewBacklogDriftConfig so the subsequent construction cannot panic.
func resolveBacklogDriftConfig(block *BacklogDriftBlock) (BacklogDriftConfig, error) {
	def := DefaultBacklogDriftConfig()

	windowSize := def.WindowSize
	minWindows := def.MinWindows
	peakRatio := def.PeakRatio
	peakRatioBand := def.PeakRatioBand
	confidenceCI := def.ConfidenceCI
	warmupWindows := def.WarmupWindows
	tailWindows := def.TailWindows
	saturatedDrainRatio := def.SaturatedDrainRatio
	transientDrainRatio := def.TransientDrainRatio

	if block != nil {
		if block.WindowSizeSec != nil {
			if *block.WindowSizeSec <= 0 {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.window_size_sec must be > 0, got %d", *block.WindowSizeSec)
			}
			windowSize = time.Duration(*block.WindowSizeSec) * time.Second
		}
		if block.MinWindows != nil {
			if *block.MinWindows <= 0 {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.min_windows must be > 0, got %d", *block.MinWindows)
			}
			minWindows = *block.MinWindows
		}
		if block.PeakRatio != nil {
			if *block.PeakRatio <= 0 || math.IsNaN(*block.PeakRatio) || math.IsInf(*block.PeakRatio, 0) {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.peak_ratio must be a finite value > 0, got %v", *block.PeakRatio)
			}
			peakRatio = *block.PeakRatio
		}
		if block.PeakRatioBand != nil {
			if *block.PeakRatioBand < 0 || math.IsNaN(*block.PeakRatioBand) || math.IsInf(*block.PeakRatioBand, 0) {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.peak_ratio_band must be >= 0, got %v", *block.PeakRatioBand)
			}
			peakRatioBand = *block.PeakRatioBand
		}
		if block.ConfidenceCI != nil {
			if *block.ConfidenceCI <= 0 || *block.ConfidenceCI >= 1 || math.IsNaN(*block.ConfidenceCI) || math.IsInf(*block.ConfidenceCI, 0) {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.confidence_ci must be in (0, 1), got %v", *block.ConfidenceCI)
			}
			confidenceCI = *block.ConfidenceCI
		}
		if block.WarmupWindows != nil {
			if *block.WarmupWindows < 0 {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.warmup_windows must be >= 0, got %d", *block.WarmupWindows)
			}
			warmupWindows = *block.WarmupWindows
		}
		if block.TailWindows != nil {
			if *block.TailWindows < 0 {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.tail_windows must be >= 0, got %d", *block.TailWindows)
			}
			tailWindows = *block.TailWindows
		}
		if block.SaturatedDrainRatio != nil {
			if *block.SaturatedDrainRatio <= 0 || *block.SaturatedDrainRatio > 1 || math.IsNaN(*block.SaturatedDrainRatio) || math.IsInf(*block.SaturatedDrainRatio, 0) {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.saturated_drain_ratio must be in (0, 1], got %v", *block.SaturatedDrainRatio)
			}
			saturatedDrainRatio = *block.SaturatedDrainRatio
		}
		if block.TransientDrainRatio != nil {
			if *block.TransientDrainRatio <= 0 || *block.TransientDrainRatio > 1 || math.IsNaN(*block.TransientDrainRatio) || math.IsInf(*block.TransientDrainRatio, 0) {
				return BacklogDriftConfig{}, fmt.Errorf("saturation config: backlog_drift.transient_drain_ratio must be in (0, 1], got %v", *block.TransientDrainRatio)
			}
			transientDrainRatio = *block.TransientDrainRatio
		}
	}

	// Cross-field invariant (mirrors NewBacklogDriftConfig): the two drain-ratio
	// thresholds must not overlap. Checked here so we return an error instead of
	// letting NewBacklogDriftConfig panic.
	if saturatedDrainRatio > transientDrainRatio {
		return BacklogDriftConfig{}, fmt.Errorf(
			"saturation config: backlog_drift.saturated_drain_ratio (%v) must be <= transient_drain_ratio (%v); regions would overlap",
			saturatedDrainRatio, transientDrainRatio)
	}

	return NewBacklogDriftConfig(
		windowSize, minWindows, peakRatio, peakRatioBand, confidenceCI,
		warmupWindows, tailWindows, saturatedDrainRatio, transientDrainRatio,
	), nil
}

// resolveWorkDriftConfig turns the swd:/owd: YAML block into a workDriftConfig,
// validating every supplied value (never panics — R6) and naming the offending
// field on error. An absent block means all defaults.
//
// The two detectors share one block type but resolve from their OWN key, so an
// swd: block never tunes owd and vice versa — they are separate detectors in a
// head-to-head comparison, and cross-contaminated knobs would invalidate it.
func resolveWorkDriftConfig(name string, cfg SaturationConfig) (workDriftConfig, error) {
	out := workDriftConfig{
		Kind:         name,
		Kappa0:       defaultWorkDriftKappa0,
		RDec0:        defaultWorkDriftRDec0,
		Threshold:    defaultWorkDriftThreshold,
		WindowSizeUs: defaultWorkDriftWindowUs,
		NumWindows:   defaultWorkDriftNumWindows,
		ConsecutiveK: defaultWorkDriftConsecutiveK,
		Quantile:     defaultWorkDriftQuantile,
	}
	blk := cfg.SWD
	if name == "owd" {
		blk = cfg.OWD
	}
	if blk == nil {
		return out, nil
	}
	posFinite := func(field string, v float64) error {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("saturation config: %s.%s must be a finite value > 0, got %v", name, field, v)
		}
		return nil
	}
	if blk.Kappa0 != nil {
		// kappa0 == 0 is MEANINGFUL (Proposition 4: it reduces the statistic to
		// VWD), so this one is >= 0 rather than > 0.
		if *blk.Kappa0 < 0 || math.IsNaN(*blk.Kappa0) || math.IsInf(*blk.Kappa0, 0) {
			return out, fmt.Errorf("saturation config: %s.kappa0 must be a finite value >= 0, got %v", name, *blk.Kappa0)
		}
		out.Kappa0 = *blk.Kappa0
	}
	if blk.RDec0 != nil {
		if err := posFinite("rdec0", *blk.RDec0); err != nil {
			return out, err
		}
		out.RDec0 = *blk.RDec0
	}
	if blk.Threshold != nil {
		if err := posFinite("threshold", *blk.Threshold); err != nil {
			return out, err
		}
		out.Threshold = *blk.Threshold
	}
	if blk.WindowSizeMs != nil {
		if *blk.WindowSizeMs <= 0 {
			return out, fmt.Errorf("saturation config: %s.window_size_ms must be > 0, got %d", name, *blk.WindowSizeMs)
		}
		out.WindowSizeUs = int64(*blk.WindowSizeMs) * 1000
	}
	if blk.NumWindows != nil {
		if *blk.NumWindows < 3 {
			return out, fmt.Errorf("saturation config: %s.num_windows must be >= 3 (the ridge fit needs at least three windows), got %d", name, *blk.NumWindows)
		}
		out.NumWindows = *blk.NumWindows
	}
	if blk.ConsecutiveK != nil {
		if *blk.ConsecutiveK <= 0 {
			return out, fmt.Errorf("saturation config: %s.consecutive_k must be > 0, got %d", name, *blk.ConsecutiveK)
		}
		out.ConsecutiveK = *blk.ConsecutiveK
	}
	if blk.Quantile != nil {
		q := *blk.Quantile
		if q <= 0 || q >= 1 || math.IsNaN(q) {
			return out, fmt.Errorf("saturation config: %s.quantile must be in (0, 1), got %v", name, q)
		}
		out.Quantile = q
	}
	if blk.FreezeRDec != nil {
		out.FreezeRDec = *blk.FreezeRDec
	}
	if blk.FreezeKappa != nil {
		out.FreezeKappa = *blk.FreezeKappa
	}
	return out, nil
}
