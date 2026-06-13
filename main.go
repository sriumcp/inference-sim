// Idiomatic entrypoint for Cobra CLI that deletes handling to the Cobra root command in cmd/root.go

package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/inference-sim/inference-sim/cmd"
)

// memProfilingFromEnv installs optional, env-gated memory diagnostics. This is a
// leak-investigation aid only: it adds no CLI flags and writes nothing to stdout,
// so it cannot affect INV-6 (byte-identical deterministic stdout). All output goes
// to a file (heap profile) or stderr (MemStats samples), matching BC-6/BC-7.
//
//	BLIS_MEMPROFILE=<path>   write an inuse_space heap profile at clean exit
//	BLIS_MEMSTATS=<ms>       emit runtime.MemStats to stderr every <ms> milliseconds
//
// Returns a cleanup func to be deferred by main().
func memProfilingFromEnv() func() {
	cleanup := func() {}

	if ms := os.Getenv("BLIS_MEMSTATS"); ms != "" {
		var everyMs int
		if _, err := fmt.Sscanf(ms, "%d", &everyMs); err == nil && everyMs > 0 {
			stop := make(chan struct{})
			go func() {
				var m runtime.MemStats
				t := time.NewTicker(time.Duration(everyMs) * time.Millisecond)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						runtime.ReadMemStats(&m)
						// stderr only — diagnostics channel, never stdout.
						fmt.Fprintf(os.Stderr,
							"[memstats] HeapAlloc=%dMiB HeapInuse=%dMiB HeapObjects=%d Sys=%dMiB NumGC=%d\n",
							m.HeapAlloc>>20, m.HeapInuse>>20, m.HeapObjects, m.Sys>>20, m.NumGC)
					}
				}
			}()
			prev := cleanup
			cleanup = func() { close(stop); prev() }
		}
	}

	if path := os.Getenv("BLIS_MEMPROFILE"); path != "" {
		prev := cleanup
		cleanup = func() {
			prev()
			f, err := os.Create(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[memprofile] create %s: %v\n", path, err)
				return
			}
			defer f.Close()
			runtime.GC() // settle live heap so inuse_space reflects retained objects
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "[memprofile] write: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "[memprofile] wrote heap profile to %s\n", path)
		}
	}

	return cleanup
}

func main() {
	cleanup := memProfilingFromEnv()
	defer cleanup()
	cmd.Execute()
}
