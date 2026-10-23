package reconciler

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func decideEntry(cpu, mem, memLim float64) recommender.Entry {
	return recommender.Entry{
		Rec: recommender.Recommendation{Confident: true,
			Recommended: recommender.Resources{CPURequest: cpu, MemRequest: mem, MemLimit: memLim}},
		Direction: policy.DirBoth, ScaleUpCooldown: 30 * time.Second, ScaleDownCooldown: time.Hour,
	}
}

func TestDecide(t *testing.T) {
	live := k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib, MemLimit: 1024 * mib}
	now := time.Unix(100000, 0)
	var never time.Time

	if _, ok := Decide(decideEntry(0.9, 480*mib, 1024*mib), live, now, never); ok {
		t.Fatal("10% drift must not resize")
	}
	if _, ok := Decide(decideEntry(0.5, 512*mib, 1024*mib), live, now, now.Add(-30*time.Minute)); ok {
		t.Fatal("scale-down inside the 1h cooldown")
	}
	p, ok := Decide(decideEntry(0.5, 512*mib, 600*mib), live, now, now.Add(-time.Hour))
	if !ok || p.TargetCPU != 0.5 || p.TargetMem != 512*mib {
		t.Fatalf("scale-down after cooldown: %+v %v", p, ok)
	}
	if _, memLim := p.Limits(recommender.Resources{CPURequest: 0.5, MemRequest: 512 * mib, MemLimit: 600 * mib}, live); memLim != 0 {
		t.Fatal("mem limit patched without a mem drift")
	}
	if p, ok := Decide(decideEntry(2, 512*mib, 1024*mib), live, now, now.Add(-time.Minute)); !ok || !p.Up(live) {
		t.Fatal("scale-up must pass the short cooldown")
	}
	e := decideEntry(0.1, 100*mib, 1024*mib)
	e.Rec.Confident = false
	if _, ok := Decide(e, live, now, never); ok {
		t.Fatal("unconfident recommendation actuated")
	}
	// OOM risk raises the limit even with no request drift, keeping the request.
	e = decideEntry(1, 512*mib, 1536*mib)
	e.Rec.OOMRisk = true
	p, ok = Decide(e, live, now, never)
	if !ok || !p.MemLimitRaise || p.TargetMem != live.MemRequest {
		t.Fatalf("OOM limit raise: %+v %v", p, ok)
	}
	if _, memLim := p.Limits(e.Rec.Recommended, live); memLim != 1536*mib {
		t.Fatalf("OOM limit = %vMi", memLim/mib)
	}
	// A limit is never lowered in place.
	p, _ = Decide(decideEntry(0.5, 300*mib, 400*mib), live, now, never)
	if _, memLim := p.Limits(recommender.Resources{MemLimit: 400 * mib}, live); memLim != 1024*mib {
		t.Fatalf("limit lowered in place: %vMi", memLim/mib)
	}
	// Shrink-only never grows; HPA on CPU holds CPU.
	e = decideEntry(2, 512*mib, 1024*mib)
	e.Direction = policy.DirDown
	if _, ok := Decide(e, live, now, never); ok {
		t.Fatal("Down policy grew CPU")
	}
	e = decideEntry(0.2, 512*mib, 1024*mib)
	e.Rec.CPUHeldByHPA = true
	if _, ok := Decide(e, live, now, never); ok {
		t.Fatal("CPU held by HPA must not be resized")
	}
}

func TestQuantizeMatchesStrings(t *testing.T) {
	for _, v := range []float64{0.0004, 0.0125, 0.3449, 1.2345} {
		if got, want := cpuStr(QuantizeCPU(v)), cpuStr(v); got != want {
			t.Fatalf("QuantizeCPU(%v) → %s, cpuStr → %s", v, got, want)
		}
	}
	for _, v := range []float64{1, 100.4 * mib, 511.6 * mib} {
		if got, want := memStr(QuantizeMem(v)), memStr(v); got != want {
			t.Fatalf("QuantizeMem(%v) → %s, memStr → %s", v, got, want)
		}
	}
}

// P1-14: a limitFactor < 1 yields rec limit < rec request. The patched limit
// must never fall below the patched request (the API rejects requests > limits).
func TestPlanLimitsNeverBelowRequest(t *testing.T) {
	e := decideEntry(1, 1024*mib, 0)
	e.Rec.Recommended.CPULimit = 0.5       // limitFactor 0.5
	e.Rec.Recommended.MemLimit = 512 * mib // limitFactor 0.5
	c := k8s.Container{Name: "app", CPURequest: 0.2, MemRequest: 256 * mib, MemLimit: 300 * mib}
	p := PlanInPlace(e, c)
	cpuLim, memLim := p.Limits(e.Rec.Recommended, c)
	if cpuLim < p.TargetCPU {
		t.Fatalf("cpu limit %v < request %v", cpuLim, p.TargetCPU)
	}
	if memLim < p.TargetMem {
		t.Fatalf("mem limit %v < request %v", memLim, p.TargetMem)
	}
}

// P1-13: a resource without enough data of its own is held, even when the
// other one is confident.
func TestPlanHoldsUnconfidentResource(t *testing.T) {
	e := decideEntry(0.1, 128*mib, 0)
	e.Rec.CPUConfident, e.Rec.MemConfident = false, true
	c := k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib}
	p := PlanInPlace(e, c)
	if p.CPUDrift || p.TargetCPU != 1 {
		t.Fatalf("CPU moved without CPU confidence: %+v", p)
	}
	if !p.MemDrift {
		t.Fatalf("memory (confident) should move: %+v", p)
	}
}
