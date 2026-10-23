package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
)

// guaranteed: resizeBoth on a Guaranteed-shaped container (request == limit):
// the current limit is not a ceiling and the recommendation carries ONE value
// per resource, so request and limit can be patched together and the pod keeps
// its QoS class. Without the opt-in the request stays capped at the limit.
func TestRecommendGuaranteedResizeBoth(t *testing.T) {
	now := float64(time.Now().Unix())
	t0 := now - 200*300
	cpu := series(t0, 300, 200, constant(1.5))
	mem := series(t0, 300, 200, constant(700*mib))
	guaranteed := k8s.Container{Name: "app", CPURequest: 1, CPULimit: 1, MemRequest: 512 * mib, MemLimit: 512 * mib}
	burstable := guaranteed
	burstable.CPULimit, burstable.MemLimit = 2, 1024*mib

	run := func(c k8s.Container, g policy.Guaranteed) Recommendation {
		res := testResolved()
		res.Guaranteed = g
		return testEngine(1).recommend(testWL, c, cpu, mem, 0, res, 0)
	}

	t.Run("resizeBoth lifts the cap and equalizes", func(t *testing.T) {
		r := run(guaranteed, policy.GuaranteedResizeBoth).Recommended
		if r.CPURequest <= 1.4 || r.CPULimit != r.CPURequest {
			t.Fatalf("cpu request %v limit %v, want request above 1.4 and limit == request", r.CPURequest, r.CPULimit)
		}
		if r.MemRequest <= 700*mib || r.MemLimit != r.MemRequest {
			t.Fatalf("mem request %vMi limit %vMi, want request above 700Mi and limit == request", r.MemRequest/mib, r.MemLimit/mib)
		}
	})
	t.Run("skip (default) keeps the cap at the current limit", func(t *testing.T) {
		r := run(guaranteed, policy.GuaranteedSkip).Recommended
		if r.CPURequest > 1 || r.MemRequest > 512*mib {
			t.Fatalf("request above the current limit without the opt-in: %+v", r)
		}
	})
	t.Run("a Burstable container is untouched by the opt-in", func(t *testing.T) {
		a := run(burstable, policy.GuaranteedResizeBoth).Recommended
		b := run(burstable, policy.GuaranteedSkip).Recommended
		if a != b {
			t.Fatalf("opt-in changed a Burstable recommendation: %+v vs %+v", a, b)
		}
	})
	t.Run("memory.max bounds the single value", func(t *testing.T) {
		res := testResolved()
		res.Guaranteed = policy.GuaranteedResizeBoth
		res.Mem.Max = 800 * mib
		r := testEngine(1).recommend(testWL, guaranteed, cpu, mem, 0, res, 0).Recommended
		if r.MemRequest > 800*mib || r.MemLimit != r.MemRequest {
			t.Fatalf("mem %v/%v exceeds memory.max or differs", r.MemRequest/mib, r.MemLimit/mib)
		}
	})
	t.Run("OOM floor raises the single value", func(t *testing.T) {
		res := testResolved()
		res.Guaranteed = policy.GuaranteedResizeBoth
		r := testEngine(1).recommend(testWL, guaranteed, cpu, mem, 0, res, 2048*mib).Recommended
		if r.MemRequest < 2048*mib || r.MemLimit != r.MemRequest {
			t.Fatalf("OOM floor not applied to both: %vMi / %vMi", r.MemRequest/mib, r.MemLimit/mib)
		}
	})
}
