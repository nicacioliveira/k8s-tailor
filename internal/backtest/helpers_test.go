package backtest

import (
	"math"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

// constTrace builds a trace where every pod uses exactly cpu/mem at all n points.
func constTrace(pods, n int, cpu, mem float64, init Resources) *Trace {
	tr := &Trace{Namespace: "backtest", Workload: "w", Container: "app", Start: SyntheticEpoch, StepSec: 300, Initial: init}
	for p := 0; p < pods; p++ {
		pt := PodTrace{Name: podName("w", p), CPU: make([]float64, n), Mem: make([]float64, n)}
		for i := 0; i < n; i++ {
			pt.CPU[i], pt.Mem[i] = cpu, mem
		}
		tr.Pods = append(tr.Pods, pt)
	}
	return tr
}

// fnTrace builds a trace from per-(pod, index) functions.
func fnTrace(pods, n int, cpu, mem func(p, i int) float64, init Resources) *Trace {
	tr := constTrace(pods, n, 0, 0, init)
	for p := range tr.Pods {
		for i := 0; i < n; i++ {
			tr.Pods[p].CPU[i], tr.Pods[p].Mem[i] = cpu(p, i), mem(p, i)
		}
	}
	return tr
}

func testEngineCfg() EngineConfig {
	cfg := config.Load()
	return EngineConfig{Base: cfg, RefreshEverySec: 2 * 3600}
}

func testOpts() Options {
	return Options{WarmupSec: 86400, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
}

func metricsOf(t *testing.T, r *Result, name string) Metrics {
	t.Helper()
	for _, m := range r.Metrics {
		if m.Strategy == name {
			return m
		}
	}
	t.Fatalf("strategy %s not in result", name)
	return Metrics{}
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
