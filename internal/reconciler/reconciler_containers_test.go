package reconciler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

var restartAlways = corev1.ContainerRestartPolicyAlways

func res(cpu, mem, limCPU, limMem string) corev1.ResourceRequirements {
	r := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	if cpu != "" {
		r.Requests[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		r.Requests[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if limCPU != "" {
		r.Limits[corev1.ResourceCPU] = resource.MustParse(limCPU)
	}
	if limMem != "" {
		r.Limits[corev1.ResourceMemory] = resource.MustParse(limMem)
	}
	return r
}

// withProxy adds an Istio-injected istio-proxy (classic container, or native
// sidecar in initContainers) plus the istio-init init container.
func withProxy(native bool, r corev1.ResourceRequirements) podOpt {
	return func(p *corev1.Pod) {
		p.Annotations = map[string]string{k8s.AnnIstioStatus: `{"containers":["istio-proxy"]}`}
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: "istio-init", Resources: res("10m", "10Mi", "", "")})
		px := corev1.Container{Name: "istio-proxy", Resources: r}
		if native {
			px.RestartPolicy = &restartAlways
			p.Spec.InitContainers = append(p.Spec.InitContainers, px)
		} else {
			p.Spec.Containers = append(p.Spec.Containers, px)
		}
	}
}

// proxyEntry is a confident entry for the injected istio-proxy.
func proxyEntry(cpu, mem float64, mut ...func(*recommender.Entry)) recommender.Entry {
	e := entry(cpu, mem)
	e.Rec.Container = "istio-proxy"
	e.Rec.Current = recommender.Resources{CPURequest: 0.1, MemRequest: 128 * mib}
	e.Class, e.NotInTemplate, e.Mesh = k8s.ClassInjectedSidecar, true, k8s.MeshIstio
	for _, m := range mut {
		m(&e)
	}
	return e
}

func (h *harness) seedMany(es map[string]recommender.Entry) {
	m := map[string]recommender.Entry{}
	var list []recommender.Recommendation
	for c, e := range es {
		m[recommender.Key(testNS, "Deployment", testDep, c)] = e
		list = append(list, e.Rec)
	}
	h.eng.Seed(list, m)
}

func resizedContainers(t *testing.T, calls []resizeCall) map[string]string {
	t.Helper()
	out := map[string]string{} // container -> list key (containers|initContainers)
	for _, c := range calls {
		spec := c.Body["spec"].(map[string]any)
		for _, key := range []string{"containers", "initContainers"} {
			if l, ok := spec[key].([]any); ok {
				for _, x := range l {
					out[x.(map[string]any)["name"].(string)] = key
				}
			}
		}
	}
	return out
}

func TestReconcileMultiContainerInPlace(t *testing.T) {
	cases := []struct {
		name    string
		native  bool
		proxy   *recommender.Entry // nil = no entry (excluded / unseen)
		want    map[string]string  // container -> list patched
		extraIn bool               // also seed an entry for the plain init container
	}{
		{name: "proxy in Recommend: only app resized", proxy: ptr(proxyEntry(0.02, 64*mib, func(e *recommender.Entry) { e.Mode = policy.ModeRecommend })),
			want: map[string]string{"app": "containers"}},
		{name: "proxy excluded (no entry): only app", want: map[string]string{"app": "containers"}},
		{name: "proxy opted in, classic sidecar", proxy: ptr(proxyEntry(0.02, 64*mib)),
			want: map[string]string{"app": "containers", "istio-proxy": "containers"}},
		{name: "proxy opted in, native sidecar → initContainers", native: true, proxy: ptr(proxyEntry(0.02, 64*mib, func(e *recommender.Entry) { e.Native = true })),
			want: map[string]string{"app": "containers", "istio-proxy": "initContainers"}},
		{name: "plain init container never resized even with an entry", extraIn: true,
			want: map[string]string{"app": "containers"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, mkPod("p", "1", "512Mi", withProxy(c.native, res("100m", "128Mi", "", ""))))
			es := map[string]recommender.Entry{"app": entry(0.2, 128*mib)}
			if c.proxy != nil {
				es["istio-proxy"] = *c.proxy
			}
			if c.extraIn {
				ie := entry(0.5, 256*mib)
				ie.Rec.Container = "istio-init"
				es["istio-init"] = ie
			}
			h.seedMany(es)
			h.cycle(t)
			got := resizedContainers(t, h.calls("resize"))
			if len(got) != len(c.want) {
				t.Fatalf("resized %v, want %v", got, c.want)
			}
			for n, key := range c.want {
				if got[n] != key {
					t.Fatalf("%s patched in %q, want %q (all: %v)", n, got[n], key, got)
				}
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// An injected proxy is NEVER patched into the Deployment template: without
// proxy annotations nothing is patched; with them, only pod-template
// annotations change (sidecar.istio.io/proxy*), limits preserved.
func TestRolloutInjectedSidecar(t *testing.T) {
	cases := []struct {
		name       string
		proxyAnns  bool
		mesh       string
		wantPatch  bool
		wantAnns   map[string]any
		curLimCPU  float64
		curLimMem  float64
		recLimCPU  float64
		recMemLim  float64
		recCPU     float64
		recMem     float64
		expectNull []string
	}{
		{name: "no proxy annotations: no template patch at all", mesh: k8s.MeshIstio, recCPU: 0.02, recMem: 64 * mib},
		{name: "unknown mesh: no patch even if enabled", proxyAnns: true, recCPU: 0.02, recMem: 64 * mib},
		{name: "istio: requests + preserved limits as annotations", proxyAnns: true, mesh: k8s.MeshIstio,
			curLimCPU: 2, curLimMem: 1024 * mib, recCPU: 0.02, recMem: 64 * mib, wantPatch: true,
			wantAnns: map[string]any{"sidecar.istio.io/proxyCPU": "20m", "sidecar.istio.io/proxyMemory": "64Mi",
				"sidecar.istio.io/proxyCPULimit": "2000m", "sidecar.istio.io/proxyMemoryLimit": "1024Mi"}},
		{name: "istio: no live limit → limit annotations removed", proxyAnns: true, mesh: k8s.MeshIstio,
			recCPU: 0.02, recMem: 64 * mib, wantPatch: true,
			wantAnns:   map[string]any{"sidecar.istio.io/proxyCPU": "20m", "sidecar.istio.io/proxyMemory": "64Mi"},
			expectNull: []string{"sidecar.istio.io/proxyCPULimit", "sidecar.istio.io/proxyMemoryLimit"}},
		{name: "linkerd keys", proxyAnns: true, mesh: k8s.MeshLinkerd, recCPU: 0.02, recMem: 64 * mib, wantPatch: true,
			wantAnns: map[string]any{"config.linkerd.io/proxy-cpu-request": "20m", "config.linkerd.io/proxy-memory-request": "64Mi"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, mkPod("a", "1", "512Mi", withProxy(false, res("100m", "128Mi", "", ""))))
			px := proxyEntry(c.recCPU, c.recMem, func(e *recommender.Entry) {
				e.Apply = policy.ApplyRollout
				e.ProxyAnnotations = c.proxyAnns
				e.Mesh = c.mesh
				e.Rec.Current.CPULimit, e.Rec.Current.MemLimit = c.curLimCPU, c.curLimMem
			})
			h.seedMany(map[string]recommender.Entry{"istio-proxy": px})
			h.cycle(t)
			var tmpl []resizeCall
			for _, call := range h.calls("") {
				if call.Kind == "deployments" {
					tmpl = append(tmpl, call)
				}
			}
			if !c.wantPatch {
				if len(tmpl) != 0 {
					t.Fatalf("template patched for an injected sidecar: %v", tmpl[0].Body)
				}
				return
			}
			if len(tmpl) != 1 {
				t.Fatalf("deployment patches = %d, want 1", len(tmpl))
			}
			tm := tmpl[0].Body["spec"].(map[string]any)["template"].(map[string]any)
			if _, has := tm["spec"]; has {
				t.Fatalf("pod spec touched (container patch) for an injected sidecar: %v", tm)
			}
			anns := tm["metadata"].(map[string]any)["annotations"].(map[string]any)
			for k, v := range c.wantAnns {
				if anns[k] != v {
					t.Errorf("%s = %v, want %v (all %v)", k, anns[k], v, anns)
				}
			}
			for _, k := range c.expectNull {
				if v, ok := anns[k]; !ok || v != nil {
					t.Errorf("%s = %v (present %v), want explicit null", k, v, ok)
				}
			}
			if _, ok := anns[k8s.AnnLastApplied]; !ok {
				t.Errorf("last-applied stamp missing")
			}
		})
	}
}

// A native sidecar declared IN the template is patched in initContainers.
func TestRolloutNativeSidecarInTemplate(t *testing.T) {
	e := entry(0.02, 64*mib)
	e.Rec.Container = "log-shipper"
	e.Rec.Current = recommender.Resources{CPURequest: 0.1, MemRequest: 128 * mib}
	e.Class, e.Native = k8s.ClassNativeSidecar, true
	// the running pod carries a native sidecar named log-shipper
	h := newHarness(t, false, mkPod("a", "1", "512Mi", func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "log-shipper", RestartPolicy: &restartAlways, Resources: res("100m", "128Mi", "", "")}}
	}))
	h.seedMany(map[string]recommender.Entry{"log-shipper": e})
	h.cycle(t)
	calls := h.calls("")
	if len(calls) != 1 {
		t.Fatalf("template patches = %d", len(calls))
	}
	spec := calls[0].Body["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if _, ok := spec["initContainers"]; !ok {
		t.Fatalf("native sidecar not patched in initContainers: %v", spec)
	}
	if _, ok := spec["containers"]; ok {
		t.Fatalf("native sidecar must not be patched as a regular container: %v", spec)
	}
}

func TestReconcileQoSGuards(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		px   recommender.Entry
		want int // proxy resize calls
	}{
		{
			name: "BestEffort pod: never add requests in place",
			pod: mkPod("p", "1", "512Mi", func(p *corev1.Pod) {
				p.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
				p.Status.QOSClass = corev1.PodQOSBestEffort
			}, withProxy(false, corev1.ResourceRequirements{})),
			px: proxyEntry(0.05, 64*mib, func(e *recommender.Entry) { e.Rec.Current = recommender.Resources{} }),
		},
		{
			name: "Burstable → Guaranteed (app req==lim, proxy would match its limits): skipped",
			pod: mkPod("p", "1", "512Mi", withLimits("1", "512Mi"),
				withProxy(false, res("100m", "128Mi", "200m", "256Mi")), func(p *corev1.Pod) {
					p.Spec.InitContainers[0].Resources = res("10m", "10Mi", "10m", "10Mi") // istio-init req==lim
				}),
			px: proxyEntry(0.2, 256*mib, func(e *recommender.Entry) {
				e.Rec.Current.CPULimit, e.Rec.Current.MemLimit = 0.2, 256*mib
			}),
		},
		{
			name: "Burstable stays Burstable (init has no limits): resized",
			pod:  mkPod("p", "1", "512Mi", withProxy(false, res("100m", "128Mi", "200m", "256Mi"))),
			px: proxyEntry(0.2, 256*mib, func(e *recommender.Entry) {
				e.Rec.Current.CPULimit, e.Rec.Current.MemLimit = 0.2, 256*mib
			}),
			want: 1,
		},
		{
			name: "pod-level requests exceeded by growth: skipped",
			pod: mkPod("p", "1", "512Mi", withProxy(false, res("100m", "128Mi", "", "")), func(p *corev1.Pod) {
				r := res("1200m", "1Gi", "", "")
				p.Spec.Resources = &r
			}),
			px: proxyEntry(0.5, 128*mib),
		},
		{
			name: "pod-level requests respected by shrink: resized",
			pod: mkPod("p", "1", "512Mi", withProxy(false, res("100m", "128Mi", "", "")), func(p *corev1.Pod) {
				r := res("1200m", "1Gi", "", "")
				p.Spec.Resources = &r
			}),
			px:   proxyEntry(0.05, 64*mib),
			want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, c.pod)
			h.seedMany(map[string]recommender.Entry{"istio-proxy": c.px})
			h.cycle(t)
			n := 0
			for name := range resizedContainers(t, h.calls("resize")) {
				if name == "istio-proxy" {
					n++
				}
			}
			if n != c.want {
				t.Fatalf("proxy resize calls = %d, want %d", n, c.want)
			}
		})
	}
}

// Partial limit: a container with a CPU limit and a recommendation above it
// (allowExceedLimit, no limitFactor) gets its limit lifted with the request —
// never a request > limit patch.
func TestPlanLimitsLiftsExistingLimit(t *testing.T) {
	e := entry(2, 512*mib)
	live := k8s.Container{Name: "app", CPURequest: 1, CPULimit: 1.5, MemRequest: 512 * mib}
	p := PlanInPlace(e, live)
	l, m := p.Limits(e.Rec.Recommended, live)
	if l < 2 {
		t.Fatalf("cpu limit = %v, want >= request 2", l)
	}
	if m != 0 {
		t.Fatalf("mem limit = %v, want untouched", m)
	}
}
