package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// P0-2: a container WITHOUT a memory limit that was OOMKilled (node pressure —
// nothing in the pod capped it) must not get a limit derived from its request.
// Before: request 256Mi, real usage 1.5Gi → floor 256Mi×1.5 = 384Mi became the
// LIMIT and the request was cut to it → limit < real usage → OOMKill loop.
func TestOOMFloorUnboundedContainerNeverCapsBelowUsage(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(1536*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	e := testEngine(1)
	res := testResolved()
	floor := 256 * mib * 1.5 // what learnOOM derives from the request of an unbounded container
	rec := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 256 * mib}, cpu, mem, 0, res, floor)
	if rec.Recommended.MemLimit != 0 {
		t.Fatalf("MemLimit = %vMi, want 0: never introduce a limit on an unbounded container", rec.Recommended.MemLimit/mib)
	}
	if rec.Recommended.MemRequest < 1536*mib {
		t.Fatalf("MemRequest = %vMi, below the observed peak 1536Mi", rec.Recommended.MemRequest/mib)
	}
	if !rec.OOMRisk {
		t.Fatalf("OOMRisk must stay set (the actuation gate relies on it)")
	}
}

// With a limit, the request must never be lowered below the observed peak to
// satisfy request<=limit: the limit is raised instead.
func TestOOMFloorNeverLowersRequestBelowPeak(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(900*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	e := testEngine(1)
	res := testResolved()
	res.AllowExceedLimit = true // the request may exceed the old 512Mi limit
	rec := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 256 * mib, MemLimit: 512 * mib}, cpu, mem, 0, res, 600*mib)
	if rec.Recommended.MemRequest < 900*mib {
		t.Fatalf("MemRequest = %vMi, below the 900Mi peak", rec.Recommended.MemRequest/mib)
	}
	if rec.Recommended.MemLimit < rec.Recommended.MemRequest {
		t.Fatalf("limit %vMi < request %vMi", rec.Recommended.MemLimit/mib, rec.Recommended.MemRequest/mib)
	}
}

// P1-14: limitFactor < 1 must never produce limit < request.
func TestRecommendLimitFactorBelowOneClamped(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(400*mib))
	cpu := series(now-100*300, 300, 100, constant(0.5))
	e := testEngine(1)
	res := testResolved()
	res.CPU.LimitFactor, res.Mem.LimitFactor = 0.5, 0.5
	rec := e.recommend(testWL, k8s.Container{Name: "app", CPURequest: 1, CPULimit: 4, MemRequest: 512 * mib, MemLimit: 4096 * mib}, cpu, mem, 0, res, 0)
	r := rec.Recommended
	if r.CPULimit < r.CPURequest || r.MemLimit < r.MemRequest {
		t.Fatalf("limit < request: %+v", r)
	}
}

// P1-13: confidence is per resource. Without a CPU series the CPU request used
// to fall to the policy minimum with Confident=true (the MAX of the two
// coverages) — and the reconciler would shrink CPU blind.
func TestConfidencePerResource(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(400*mib))
	e := testEngine(10)
	res := testResolved()
	rec := e.recommend(testWL, k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib}, nil, mem, 0, res, 0)
	if rec.CPUConfident || !rec.MemConfident {
		t.Fatalf("cpuConfident=%v memConfident=%v, want false/true", rec.CPUConfident, rec.MemConfident)
	}
	if rec.Recommended.CPURequest != 1 {
		t.Fatalf("CPU request = %v with no CPU data, want the current 1 (no blind shrink)", rec.Recommended.CPURequest)
	}
	if cpu, mem := rec.ResourceConfidence(); cpu || !mem {
		t.Fatalf("ResourceConfidence = %v/%v", cpu, mem)
	}
}
