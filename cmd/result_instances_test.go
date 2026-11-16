package cmd

// Per-instance figures over the run's measured window (#2087, design §8.7): each engine instance's
// block in the engine report carries, under "measured", the cluster's per-request metrics taken
// over that instance's requests and over the run's window.

import (
	"math"
	"sort"
	"testing"

	"github.com/inference-sim/blis-substrate/contracts/spec/result"
)

// instanceMeasuredOf returns each instance's "measured" block from the engine report.
func instanceMeasuredOf(t *testing.T, r *result.Result) map[string]map[string]map[string]any {
	t.Helper()
	insts, ok := r.Reports.Engine["instances"].(map[string]any)
	if !ok || len(insts) < 2 {
		t.Fatalf("the engine report has no instances block for a multi-instance run: %v", r.Reports.Engine["instances"])
	}
	out := map[string]map[string]map[string]any{}
	for id, v := range insts {
		sec, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("instance %s: block is %T", id, v)
		}
		m, ok := sec["measured"].(map[string]any)
		if !ok {
			continue
		}
		out[id] = map[string]map[string]any{}
		for k, f := range m {
			fm, ok := f.(map[string]any)
			if !ok {
				t.Fatalf("instance %s: figure %s is %T", id, k, f)
			}
			out[id][k] = fm
		}
	}
	return out
}

func figNum(t *testing.T, f map[string]any, key string) float64 {
	t.Helper()
	switch v := f[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	t.Fatalf("figure field %s is %T", key, f[key])
	return 0
}

// numpyLinear is numpy's default percentile, written here independently of the simulator's.
func numpyLinear(xs []float64, q float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	k := q / 100 * float64(len(s)-1)
	lo := int(math.Floor(k))
	hi := min(lo+1, len(s)-1)
	return s[lo] + (s[hi]-s[lo])*(k-float64(lo))
}

func instanceRun(t *testing.T) *result.Result {
	t.Helper()
	spec := capSpec(t, 40, 160,
		capClient("short", "standard", 0.5, 300, 40, ""),
		capClient("long", "standard", 0.5, 1500, 120, ""))
	stdout, stderr, err := runKernelCLI(t, runArgs(colocated(4), "--workload-spec", spec)...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, lastLines(stderr, 5))
	}
	return parseResult(t, stdout)
}

// TestResult_InstanceMeasured_IsEachInstancesRequests checks that an instance's measured
// latencies are the statistics of the requests that instance served, and nothing else: the p99
// and the mean of their TTFT and E2E, and the number of requests behind them.
func TestResult_InstanceMeasured_IsEachInstancesRequests(t *testing.T) {
	r := instanceRun(t)
	got := instanceMeasuredOf(t, r)
	ttft, e2e := map[string][]float64{}, map[string][]float64{}
	for _, row := range r.RequestRows {
		if row.Fate != result.FateCompleted || row.TTFTUs == nil || row.E2EUs == nil {
			continue
		}
		ttft[row.Instance] = append(ttft[row.Instance], float64(*row.TTFTUs)/1e3)
		e2e[row.Instance] = append(e2e[row.Instance], float64(*row.E2EUs)/1e3)
	}
	if len(ttft) < 2 {
		t.Fatalf("the run spread its requests over %d instance(s); the check needs several", len(ttft))
	}
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-3*math.Max(1, math.Abs(b)) }
	for id, xs := range ttft {
		figs, ok := got[id]
		if !ok {
			t.Errorf("instance %s served %d requests but states no measured figures", id, len(xs))
			continue
		}
		for metric, want := range map[string]float64{
			"ttft_p99_ms": numpyLinear(xs, 99), "ttft_p50_ms": numpyLinear(xs, 50),
			"e2e_p99_ms": numpyLinear(e2e[id], 99),
		} {
			f, ok := figs[metric]
			if !ok {
				t.Errorf("instance %s states no %s", id, metric)
				continue
			}
			if v := figNum(t, f, "value"); !near(v, want) {
				t.Errorf("instance %s %s = %v; its %d requests give %v", id, metric, v, len(xs), want)
			}
			if n := figNum(t, f, "samples"); int(n) != len(xs) {
				t.Errorf("instance %s %s over %v samples; it served %d", id, metric, n, len(xs))
			}
			if f["scope"] != "instance" || f["member"] != id {
				t.Errorf("instance %s %s states scope %v member %v", id, metric, f["scope"], f["member"])
			}
		}
	}
}

// TestResult_InstanceMeasured_OverTheRunsWindow checks that every instance's rates are over the
// run's measured window, not over the span of its own requests: the instances' output token
// rates add up to the cluster's, and their request counts to the cluster's measured count.
func TestResult_InstanceMeasured_OverTheRunsWindow(t *testing.T) {
	r := instanceRun(t)
	got := instanceMeasuredOf(t, r)
	var rate, n float64
	for id, figs := range got {
		f, ok := figs["output_tok_s"]
		if !ok {
			t.Fatalf("instance %s states no output_tok_s", id)
		}
		rate += figNum(t, f, "value")
		n += figNum(t, f, "samples")
	}
	cluster, ok := r.Metrics["output_tok_s"]
	if !ok {
		t.Fatal("the Result states no cluster output_tok_s")
	}
	if math.Abs(rate-cluster.Value) > 1e-6*cluster.Value {
		t.Errorf("instances' output_tok_s add to %v; the cluster's over the run's window is %v", rate, cluster.Value)
	}
	if int(n) != cluster.Samples {
		t.Errorf("instances' output_tok_s cover %v requests; the cluster's %d", n, cluster.Samples)
	}
}
