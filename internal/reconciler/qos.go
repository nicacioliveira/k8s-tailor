package reconciler

import (
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// after returns container cc's requests/limits once container name is resized
// to (cpu, mem) with new limits (0 = limit unchanged). Values are quantized the
// way the API receives them.
func after(cc k8s.Container, name string, cpu, mem, cpuLim, memLim float64) (reqCPU, reqMem, limCPU, limMem float64) {
	reqCPU, reqMem, limCPU, limMem = cc.CPURequest, cc.MemRequest, cc.CPULimit, cc.MemLimit
	if cc.Name != name {
		return
	}
	reqCPU, reqMem = QuantizeCPU(cpu), QuantizeMem(mem)
	if cpuLim > 0 {
		limCPU = QuantizeCPU(cpuLim)
	}
	if memLim > 0 {
		limMem = QuantizeMem(memLim)
	}
	return
}

// becomesGuaranteed reports whether resizing container name would make the pod
// QoS Guaranteed: every container (app, sidecars AND init containers — the QoS
// class is computed over all of them) has CPU and memory limits equal to its
// requests. A Burstable pod may not change class in place, so the API server
// would reject the resize.
func becomesGuaranteed(p k8s.RunningPod, name string, cpu, mem, cpuLim, memLim float64) bool {
	seen := false
	for _, cc := range p.Containers {
		if cc.Class == k8s.ClassEphemeral {
			continue // no resources
		}
		seen = true
		rc, rm, lc, lm := after(cc, name, cpu, mem, cpuLim, memLim)
		if lc <= 0 || lm <= 0 {
			return false
		}
		// An unset request defaults to the limit.
		if rc <= 0 {
			rc = lc
		}
		if rm <= 0 {
			rm = lm
		}
		if !approxEq(rc, lc) || !approxEq(rm, lm) {
			return false
		}
	}
	return seen
}

// podLevelFits checks the pod-level resources constraints (PodLevelResources):
// the sum of the long-running containers' requests (app + sidecars) must stay
// <= the pod-level requests, and each container limit <= the pod-level limit.
func podLevelFits(p k8s.RunningPod, name string, cpu, mem, cpuLim, memLim float64) bool {
	pr := p.PodResources
	if pr == nil {
		return true
	}
	var sumCPU, sumMem float64
	for _, cc := range p.Containers {
		if !cc.Class.Resizable() {
			continue
		}
		rc, rm, lc, lm := after(cc, name, cpu, mem, cpuLim, memLim)
		sumCPU += rc
		sumMem += rm
		if cc.Name == name {
			if pr.CPULimit > 0 && lc > pr.CPULimit+1e-9 {
				return false
			}
			if pr.MemLimit > 0 && lm > pr.MemLimit+1 {
				return false
			}
		}
	}
	if pr.CPURequest > 0 && sumCPU > pr.CPURequest+1e-9 {
		return false
	}
	if pr.MemRequest > 0 && sumMem > pr.MemRequest+1 {
		return false
	}
	return true
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	m := a
	if b > m {
		m = b
	}
	return d <= 1e-6*m+1e-9
}
