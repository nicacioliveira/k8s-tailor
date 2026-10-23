package k8s

import (
	corev1 "k8s.io/api/core/v1"
)

// containersOf extracts per-container requests/limits as cores/bytes.
func containersOf(cs []corev1.Container) []Container {
	out := make([]Container, 0, len(cs))
	for i := range cs {
		c := &cs[i]
		ct := Container{Name: c.Name}
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			ct.CPURequest = q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			ct.CPULimit = q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			ct.MemRequest = q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			ct.MemLimit = q.AsApproximateFloat64()
		}
		// A resize restarts the container when it opts in via resizePolicy
		// RestartContainer — for memory OR cpu. That IS a disruption: the
		// reconciler must roll it one pod at a time and respect PDBs.
		for _, rp := range c.ResizePolicy {
			if rp.RestartPolicy != corev1.RestartContainer {
				continue
			}
			switch rp.ResourceName {
			case corev1.ResourceMemory:
				ct.MemRestartOnResize = true
			case corev1.ResourceCPU:
				ct.CPURestartOnResize = true
			}
		}
		out = append(out, ct)
	}
	return out
}
