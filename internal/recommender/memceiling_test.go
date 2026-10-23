package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// tailor.rbac.clusterLookups: the memory request never exceeds MemCeiling, the
// lower of the largest eligible node's allocatable memory and the namespace's
// LimitRange max. 0 = no lookup (the ceiling that applied before).
func TestRecommendMemCeiling(t *testing.T) {
	now := float64(time.Now().Unix())
	t0 := now - 200*300
	cpu := series(t0, 300, 200, constant(0.2))
	mem := series(t0, 300, 200, constant(1024*mib))
	noLimit := k8s.Container{Name: "app", CPURequest: 1, MemRequest: 1024 * mib}
	withLimit := k8s.Container{Name: "app", CPURequest: 1, MemRequest: 1024 * mib, MemLimit: 2048 * mib}

	run := func(c k8s.Container, oomFloor, ceiling float64) Recommendation {
		return Recommend(RecommendInput{
			Workload: testWL, Container: c, CPU: cpu, Mem: mem, Policy: testResolved(),
			HalfLifeSec: 86400, OOMFloor: oomFloor, OOMGrowth: 1.5, OOMMemCapFactor: 4,
			Now: time.Unix(int64(now), 0), MinSamples: 1, MemCeiling: ceiling,
		})
	}

	t.Run("OOM request raise of a container without a limit stops at the ceiling", func(t *testing.T) {
		if r := run(noLimit, 8192*mib, 0); r.Recommended.MemRequest <= 3000*mib {
			t.Fatalf("precondition: without a ceiling the raise should be large, got %vMi", r.Recommended.MemRequest/mib)
		}
		r := run(noLimit, 8192*mib, 2048*mib)
		if r.Recommended.MemRequest > 2048*mib {
			t.Fatalf("request %vMi above the 2048Mi ceiling", r.Recommended.MemRequest/mib)
		}
		if !r.Signals.MemCeilingCapped || r.Signals.MemCeiling != 2048*mib {
			t.Fatalf("signal not set: %+v", r.Signals)
		}
	})
	t.Run("an ordinary request is capped too", func(t *testing.T) {
		r := run(withLimit, 0, 1024*mib)
		if r.Recommended.MemRequest > 1024*mib {
			t.Fatalf("request %vMi above the ceiling", r.Recommended.MemRequest/mib)
		}
		if r.Recommended.MemLimit < r.Recommended.MemRequest {
			t.Fatalf("limit %v below request %v", r.Recommended.MemLimit, r.Recommended.MemRequest)
		}
	})
	t.Run("no ceiling: nothing capped, no signal", func(t *testing.T) {
		r := run(withLimit, 0, 0)
		if r.Signals.MemCeilingCapped || r.Signals.MemCeiling != 0 {
			t.Fatalf("signal set without a ceiling: %+v", r.Signals)
		}
	})
	t.Run("ceiling above the request: untouched", func(t *testing.T) {
		a, b := run(withLimit, 0, 0), run(withLimit, 0, 64*1024*mib)
		if a.Recommended != b.Recommended || b.Signals.MemCeilingCapped {
			t.Fatalf("a roomy ceiling changed the recommendation: %+v vs %+v", a.Recommended, b.Recommended)
		}
	})
}
