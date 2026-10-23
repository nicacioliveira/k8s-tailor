package recommender

import (
	"strings"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
)

// templateKind reports whether a workload kind has a pod template the engine
// can read (and a rollout can patch). Pods and TailorGroup units are read from
// live pod specs instead.
func templateKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ksvc":
		return true
	}
	return false
}

// MergeObservedContainers returns the workload's sizable containers: the ones
// it already lists (template containers + native sidecars declared there) plus
// any container the RUNNING pods carry that the template doesn't — i.e. a
// sidecar injected at admission by a mutating webhook. Such a container is
// classified injected-sidecar (even with an unknown name: being absent from the
// template IS the injection signal) and is never InTemplate, so the durable
// rollout path never patches it into the template. Its "current" values are the
// live ones (there is no template value). Init/ephemeral containers are skipped.
func MergeObservedContainers(w k8s.Workload, pods []k8s.RunningPod) []k8s.Container {
	out := append([]k8s.Container(nil), w.Containers...)
	idx := map[string]int{}
	for i, c := range out {
		idx[c.Name] = i
	}
	for _, p := range pods {
		if p.Owner.Kind != w.Kind || p.Owner.Name != w.Name {
			continue
		}
		for _, c := range p.Containers {
			if !c.Class.Resizable() {
				continue
			}
			if i, ok := idx[c.Name]; ok {
				if out[i].Mesh == "" {
					out[i].Mesh = c.Mesh // template can't carry the injector marker
				}
				continue
			}
			c.InTemplate = false
			if templateKind(w.Kind) && c.Class != k8s.ClassInjectedSidecar {
				c.Class = k8s.ClassInjectedSidecar
			}
			c.SpecCPURequest, c.SpecMemRequest = 0, 0
			idx[c.Name] = len(out)
			out = append(out, c)
		}
	}
	return out
}

// actuationPath describes, for the UI, how a container would be actuated under
// its effective policy: "none", "webhook", "in-place", "template", "ksvc",
// "proxy-annotations:<mesh>", or "none:not-in-template" when the durable path
// is impossible (a sidecar injected at admission, no mesh annotations to use).
// Several paths are joined with "+".
func actuationPath(w k8s.Workload, c k8s.Container, res policy.Resolved) string {
	if !res.Mode.Webhook() && !res.Mode.InPlace() {
		return "none"
	}
	var paths []string
	if res.Mode.Webhook() {
		paths = append(paths, "webhook")
	}
	if res.Mode.InPlace() {
		switch {
		case w.Kind == "Pod" || w.Kind == "group":
			paths = append(paths, "in-place")
		case res.Apply == policy.ApplyRollout:
			paths = append(paths, durablePath(w, c, res))
		case res.Apply == policy.ApplyInPlacePreferred:
			paths = append(paths, "in-place", "fallback:"+durablePath(w, c, res))
		default:
			paths = append(paths, "in-place")
		}
	}
	return strings.Join(paths, "+")
}

// durablePath is the template-level actuation for a container.
func durablePath(w k8s.Workload, c k8s.Container, res policy.Resolved) string {
	if c.InTemplate {
		if w.Knative.Service != "" {
			if c.Native {
				return "none:native-sidecar-in-ksvc"
			}
			return "ksvc"
		}
		return "template"
	}
	if ProxyAnnotationKeys(c.Mesh) != nil && res.ProxyAnnotations && w.Knative.Service == "" {
		return "proxy-annotations:" + c.Mesh
	}
	return "none:not-in-template"
}

// ProxyAnnotationKeys returns the pod annotations a mesh injector reads to size
// its proxy, as [cpuRequest, memRequest, cpuLimit, memLimit]; nil when the mesh
// has none we support.
//
//   - Istio: sidecar.istio.io/proxyCPU, proxyMemory, proxyCPULimit,
//     proxyMemoryLimit (istio.io annotations reference). NOTE: the injection
//     template drops the mesh-wide default resources as soon as ANY of the four
//     is present, so callers must write the limits too or the proxy loses them.
//   - Linkerd: config.linkerd.io/proxy-cpu-request, proxy-memory-request,
//     proxy-cpu-limit, proxy-memory-limit (linkerd.io proxy configuration).
func ProxyAnnotationKeys(mesh string) []string {
	switch mesh {
	case k8s.MeshIstio:
		return []string{"sidecar.istio.io/proxyCPU", "sidecar.istio.io/proxyMemory",
			"sidecar.istio.io/proxyCPULimit", "sidecar.istio.io/proxyMemoryLimit"}
	case k8s.MeshLinkerd:
		return []string{"config.linkerd.io/proxy-cpu-request", "config.linkerd.io/proxy-memory-request",
			"config.linkerd.io/proxy-cpu-limit", "config.linkerd.io/proxy-memory-limit"}
	}
	return nil
}
