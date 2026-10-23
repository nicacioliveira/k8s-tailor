package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// After a restart the stabilization history is empty: without a seed the first
// window publishes the raw recommendation. Seeded from the pure Recommend at the
// past query steps, a fresh engine publishes what a process that kept running
// would have — here a scale-down held by the 15-minute window.
func TestStabilizerSeededAfterRestart(t *testing.T) {
	end := time.Unix(1_800_000_000, 0)
	n := 40
	t0 := float64(end.Unix()) - float64((n-1)*stepSec)
	// Usage drops sharply in the last 15 minutes: the raw request falls step by
	// step, the stabilized one must not.
	cpu := series(t0, stepSec, n, func(i int) float64 {
		if i >= n-3 {
			return 0.05
		}
		return 1
	})
	mem := series(t0, stepSec, n, constant(100*mib))
	res := testResolved()
	res.CPU.Quantile = 0.5
	res.HalfLife = 10 * time.Minute
	res.ScaleDownStabilization = 15 * time.Minute
	in := func(now time.Time) RecommendInput {
		return RecommendInput{Workload: testWL, Container: k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib},
			CPU: samplesUpTo(cpu, now), Mem: samplesUpTo(mem, now), Policy: res, Now: now, MinSamples: 1}
	}
	key := "ns/Deployment/web/app"

	running := testEngine(1)
	var want Recommendation
	for k := 3; k >= 0; k-- {
		want = running.recommendStabilized(key, in(end.Add(-time.Duration(k*stepSec)*time.Second)))
	}
	if !want.Signals.Stabilized {
		t.Fatalf("fixture: the running engine did not stabilize (raw %v)", want.Signals.RawCPURequest)
	}

	restarted := testEngine(1)
	got := restarted.recommendStabilized(key, in(end))
	if got.Recommended.CPURequest != want.Recommended.CPURequest || !got.Signals.Stabilized {
		t.Fatalf("after restart: cpu %v (stabilized=%v), want %v as the running engine",
			got.Recommended.CPURequest, got.Signals.Stabilized, want.Recommended.CPURequest)
	}

	// No window, no seeding (and no extra cost).
	res.ScaleDownStabilization = 0
	e := testEngine(1)
	if r := e.recommendStabilized(key, in(end)); r.Signals.Stabilized || e.stab.has(key) {
		t.Fatal("stabilization 0 must not seed or stabilize")
	}
}
