package reconciler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// guaranteedEntry builds an entry for a container whose request equals its limit
// (1 core / 512Mi). The recommender already hands such a container a Guaranteed
// shaped recommendation (limit == request) when guaranteed: resizeBoth is on.
func guaranteedEntry(cpu, mem float64, mut ...func(*recommender.Entry)) recommender.Entry {
	return entry(cpu, mem, func(e *recommender.Entry) {
		e.GuaranteedResize = true
		e.Rec.Recommended.CPULimit = cpu
		e.Rec.Recommended.MemLimit = mem
		for _, m := range mut {
			if m != nil {
				m(e)
			}
		}
	})
}

var guaranteedLive = k8s.Container{Name: "app", CPURequest: 1, CPULimit: 1, MemRequest: 512 * mib, MemLimit: 512 * mib}

// With resizeBoth, request and limit always leave PlanInPlace equal: CPU moves in
// both directions, memory only grows.
func TestPlanGuaranteedResizeBoth(t *testing.T) {
	cases := []struct {
		name        string
		cpu, mem    float64
		mut         func(*recommender.Entry)
		wantCPU     float64 // patched cpu limit (0 = untouched)
		wantMem     float64 // patched mem limit (0 = untouched)
		wantWants   bool
		wantMemSkip bool
		wantTgtCPU  float64
		wantTgtMem  float64
	}{
		{name: "grow both", cpu: 2, mem: 1024 * mib, wantCPU: 2, wantMem: 1024 * mib, wantWants: true, wantTgtCPU: 2, wantTgtMem: 1024 * mib},
		{name: "shrink cpu, memory steady", cpu: 0.5, mem: 512 * mib, wantCPU: 0.5, wantWants: true, wantTgtCPU: 0.5, wantTgtMem: 512 * mib},
		{name: "shrink both: memory is held", cpu: 0.5, mem: 256 * mib, wantCPU: 0.5, wantWants: true, wantMemSkip: true, wantTgtCPU: 0.5, wantTgtMem: 512 * mib},
		{name: "memory shrink alone: nothing to do", cpu: 1, mem: 256 * mib, wantWants: false, wantMemSkip: true, wantTgtCPU: 1, wantTgtMem: 512 * mib},
		{name: "grow memory, shrink cpu", cpu: 0.5, mem: 1024 * mib, wantCPU: 0.5, wantMem: 1024 * mib, wantWants: true, wantTgtCPU: 0.5, wantTgtMem: 1024 * mib},
		{
			name: "OOM floor raises request and limit together", cpu: 1, mem: 512 * mib, wantMem: 768 * mib, wantWants: true, wantTgtCPU: 1, wantTgtMem: 768 * mib,
			mut: func(e *recommender.Entry) {
				e.Rec.OOMRisk = true
				e.Rec.Recommended.MemLimit = 768 * mib
			},
		},
		{
			name: "censored CPU raises request and limit together", cpu: 1.5, mem: 512 * mib, wantCPU: 1.5, wantWants: true, wantTgtCPU: 1.5, wantTgtMem: 512 * mib,
			mut: func(e *recommender.Entry) {
				e.Rec.Signals.CPUCensored = true
				e.Rec.Recommended.CPULimit = 1.5
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := guaranteedEntry(c.cpu, c.mem, c.mut)
			p := PlanInPlace(e, guaranteedLive)
			if !p.Guaranteed {
				t.Fatalf("plan not marked Guaranteed: %+v", p)
			}
			if p.Wants() != c.wantWants {
				t.Fatalf("Wants = %v, want %v (%+v)", p.Wants(), c.wantWants, p)
			}
			if p.GuaranteedMemShrinkHeld != c.wantMemSkip {
				t.Fatalf("GuaranteedMemShrinkHeld = %v, want %v", p.GuaranteedMemShrinkHeld, c.wantMemSkip)
			}
			if p.TargetCPU != c.wantTgtCPU || p.TargetMem != c.wantTgtMem {
				t.Fatalf("targets = %v / %vMi, want %v / %vMi", p.TargetCPU, p.TargetMem/mib, c.wantTgtCPU, c.wantTgtMem/mib)
			}
			cpuLim, memLim := p.Limits(e.Rec.Recommended, guaranteedLive)
			if cpuLim != c.wantCPU || memLim != c.wantMem {
				t.Fatalf("limits = %v / %vMi, want %v / %vMi", cpuLim, memLim/mib, c.wantCPU, c.wantMem/mib)
			}
			// Whatever moves, request and limit end equal.
			finalCPUReq, finalCPULim := p.TargetCPU, guaranteedLive.CPULimit
			if cpuLim > 0 {
				finalCPULim = cpuLim
			}
			finalMemReq, finalMemLim := p.TargetMem, guaranteedLive.MemLimit
			if memLim > 0 {
				finalMemLim = memLim
			}
			if finalCPUReq != finalCPULim || finalMemReq != finalMemLim {
				t.Fatalf("QoS would change: cpu %v/%v mem %v/%v", finalCPUReq, finalCPULim, finalMemReq, finalMemLim)
			}
		})
	}
}

// Without the opt-in the plan is the ordinary one (and the reconciler skips the
// pod before it ever gets here).
func TestPlanGuaranteedOffByDefault(t *testing.T) {
	e := guaranteedEntry(2, 1024*mib)
	e.GuaranteedResize = false
	if p := PlanInPlace(e, guaranteedLive); p.Guaranteed {
		t.Fatalf("plan marked Guaranteed without the opt-in: %+v", p)
	}
	// A Burstable container (request < limit) is never treated as Guaranteed.
	burst := guaranteedLive
	burst.CPURequest = 0.5
	if p := PlanInPlace(guaranteedEntry(2, 1024*mib), burst); p.Guaranteed {
		t.Fatalf("burstable container marked Guaranteed: %+v", p)
	}
}

func patchedResources(t *testing.T, c resizeCall) (req, lim map[string]any) {
	t.Helper()
	res := c.Body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
	req, _ = res["requests"].(map[string]any)
	lim, _ = res["limits"].(map[string]any)
	return req, lim
}

// A Guaranteed pod stays Guaranteed: the resize patches request and limit to the
// same value, for CPU and memory.
func TestReconcileGuaranteedResizeBoth(t *testing.T) {
	g := func(opts ...podOpt) *corev1.Pod {
		return mkPod("p", "1", "512Mi", append([]podOpt{withLimits("1", "512Mi"), withQoS(corev1.PodQOSGuaranteed)}, opts...)...)
	}
	t.Run("grow", func(t *testing.T) {
		h := newHarness(t, true, g())
		h.seed(guaranteedEntry(2, 1024*mib))
		h.cycle(t)
		calls := h.calls("resize")
		if len(calls) != 1 {
			t.Fatalf("resize calls = %d, want 1", len(calls))
		}
		req, lim := patchedResources(t, calls[0])
		if req["cpu"] != "2" && req["cpu"] != "2000m" || lim["cpu"] != req["cpu"] || req["memory"] != "1024Mi" || lim["memory"] != "1024Mi" {
			t.Fatalf("patch breaks req==lim: requests %v limits %v", req, lim)
		}
	})
	t.Run("shrink cpu only", func(t *testing.T) {
		before := metrics.Skips.Value("guaranteed_mem_shrink")
		h := newHarness(t, true, g())
		h.seed(guaranteedEntry(0.5, 128*mib))
		h.cycle(t)
		calls := h.calls("resize")
		if len(calls) != 1 {
			t.Fatalf("resize calls = %d, want 1", len(calls))
		}
		req, lim := patchedResources(t, calls[0])
		if req["cpu"] != "500m" || lim["cpu"] != "500m" {
			t.Fatalf("cpu not shrunk together: %v %v", req, lim)
		}
		if _, ok := req["memory"]; ok && req["memory"] != "512Mi" {
			t.Fatalf("memory shrank on a Guaranteed pod: %v", req)
		}
		if lim["memory"] != nil && lim["memory"] != "512Mi" {
			t.Fatalf("memory limit shrank: %v", lim)
		}
		if d := metrics.Skips.Value("guaranteed_mem_shrink") - before; d != 1 {
			t.Fatalf("guaranteed_mem_shrink skips delta = %v, want 1", d)
		}
	})
	t.Run("memory shrink alone is a skip with a reason", func(t *testing.T) {
		before := metrics.Skips.Value("guaranteed_mem_shrink")
		h := newHarness(t, true, g())
		h.seed(guaranteedEntry(1, 128*mib))
		h.cycle(t)
		if n := len(h.calls("resize")); n != 0 {
			t.Fatalf("resize calls = %d, want 0", n)
		}
		if d := metrics.Skips.Value("guaranteed_mem_shrink") - before; d != 1 {
			t.Fatalf("guaranteed_mem_shrink skips delta = %v, want 1", d)
		}
	})
	t.Run("default (skip) still leaves the pod alone", func(t *testing.T) {
		before := metrics.Skips.Value("guaranteed_qos")
		h := newHarness(t, true, g())
		h.seed(guaranteedEntry(2, 1024*mib, func(e *recommender.Entry) { e.GuaranteedResize = false }))
		h.cycle(t)
		if n := len(h.calls("resize")); n != 0 {
			t.Fatalf("resize calls = %d, want 0", n)
		}
		if d := metrics.Skips.Value("guaranteed_qos") - before; d != 1 {
			t.Fatalf("guaranteed_qos skips delta = %v, want 1", d)
		}
	})
}
