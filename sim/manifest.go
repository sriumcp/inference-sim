// Reproducibility manifest for BLIS metrics output.
//
// Embedded in MetricsOutput.Manifest, the manifest records run metadata
// that lets a future reader reconstruct (or audit) the exact configuration
// that produced a result JSON: source-tree state, workload-spec content
// hash, command-line arguments, runtime environment.
//
// The manifest is metadata about the run, NOT part of the simulation's
// deterministic output. Determinism checks should hash the JSON with the
// manifest stripped (see runs/iter-2/inputs/gap2_determinism.py for the
// canonical pattern).

package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Manifest captures reproducibility metadata for a single BLIS run.
type Manifest struct {
	BlisGitSHA       string    `json:"blis_git_sha,omitempty"`
	BlisGitDirty     bool      `json:"blis_git_dirty,omitempty"`
	WorkloadYAMLPath string    `json:"workload_yaml_path,omitempty"`
	WorkloadYAMLSHA  string    `json:"workload_yaml_sha256,omitempty"`
	Seed             int64     `json:"seed"`
	CommandLine      []string  `json:"command_line,omitempty"`
	GoVersion        string    `json:"go_version,omitempty"`
	OSArch           string    `json:"os_arch,omitempty"`
	Timestamp        time.Time `json:"timestamp"`
}

// NewManifest gathers reproducibility metadata at run time. Best-effort:
// missing fields are silently empty rather than fatal so a failure to
// read git VCS info or hash a workload doesn't kill the run.
//
// workloadPath may be "" if no workload spec was used (e.g. blis with
// --workload type instead of --workload-spec).
//
// commandLine should be os.Args at the cmd boundary; the sim layer
// shouldn't read os.Args directly.
func NewManifest(workloadPath string, seed int64, commandLine []string) *Manifest {
	m := &Manifest{
		Seed:        seed,
		CommandLine: commandLine,
		GoVersion:   runtime.Version(),
		OSArch:      runtime.GOOS + "/" + runtime.GOARCH,
		Timestamp:   time.Now().UTC(),
	}

	// Prefer runtime git invocation: in git worktrees, debug.ReadBuildInfo()
	// reports the PARENT repo's HEAD rather than the worktree's checked-out
	// HEAD (a known Go limitation). Shelling out to `git` from CWD picks up
	// the worktree's actual revision. Fall back to build VCS info only if
	// git isn't available (e.g. binary copied to a non-repo directory).
	if sha := runGit("rev-parse", "HEAD"); sha != "" {
		m.BlisGitSHA = sha
		// `git status --porcelain` empty ⇒ clean tree.
		m.BlisGitDirty = (runGit("status", "--porcelain") != "")
	} else if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				m.BlisGitSHA = s.Value
			case "vcs.modified":
				m.BlisGitDirty = (s.Value == "true")
			}
		}
	}

	if workloadPath != "" {
		m.WorkloadYAMLPath = workloadPath
		if data, err := os.ReadFile(workloadPath); err == nil {
			sum := sha256.Sum256(data)
			m.WorkloadYAMLSHA = hex.EncodeToString(sum[:])
		}
	}

	return m
}

// runGit executes `git <args...>` and returns trimmed stdout, or "" on
// any failure (binary missing, not a repo, etc.). Best-effort.
func runGit(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
