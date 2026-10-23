package policy

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

const mi = 1024 * 1024

func meshStore(t *testing.T, extra ...runtime.Object) *Store {
	t.Helper()
	objs := append([]runtime.Object{
		cr("TailorProfile", "envoy", map[string]any{
			"cpu":    map[string]any{"quantile": 0.99, "margin": 0.3, "min": "20m"},
			"memory": map[string]any{"min": "96Mi"},
		}),
		cr("TailorPolicy", "mesh", map[string]any{
			"priority": int64(10), "mode": "InPlace",
			"selector": map[string]any{"namespaces": []any{"apps"}},
			"cpu":      map[string]any{"min": "500m"}, // app floor: must NOT leak into the proxy
			"containers": []any{
				map[string]any{"match": map[string]any{"names": []any{"istio-proxy"}}, "mode": "InPlace", "profileRef": "envoy",
					"memory": map[string]any{"max": "512Mi"}, "proxyAnnotations": true},
				map[string]any{"match": map[string]any{"names": []any{"vault-*"}}, "exclude": true},
				map[string]any{"match": map[string]any{"classes": []any{"native-sidecar"}}, "mode": "Recommend"},
			},
		}),
	}, extra...)
	return storeWith(t, objs...)
}

func TestResolveContainer(t *testing.T) {
	s := meshStore(t)
	cases := []struct {
		name      string
		ns        string
		anns      map[string]string
		container string
		class     string
		wantMode  Mode
		wantExcl  bool
		wantCPUMn float64
		wantMemMx float64
		wantProxy bool
		wantSrc   string
	}{
		{name: "app follows the workload policy", ns: "apps", container: "app", class: "app",
			wantMode: ModeInPlace, wantCPUMn: 0.5, wantSrc: "policy"},
		{name: "istio-proxy rule: own profile, own max, enabled, proxy annotations", ns: "apps", container: "istio-proxy", class: "injected-sidecar",
			wantMode: ModeInPlace, wantCPUMn: 0.02, wantMemMx: 512 * mi, wantProxy: true, wantSrc: "policy:mesh containers[0]"},
		{name: "glob exclusion", ns: "apps", container: "vault-agent", class: "injected-sidecar",
			wantMode: ModeOff, wantExcl: true, wantCPUMn: 0.01, wantSrc: "policy:mesh containers[1]"},
		{name: "class match", ns: "apps", container: "log-shipper", class: "native-sidecar",
			wantMode: ModeRecommend, wantCPUMn: 0.5, wantSrc: "policy:mesh containers[2]"},
		{name: "unmatched injected sidecar: safe default Recommend + sidecar profile", ns: "apps", container: "linkerd-proxy", class: "injected-sidecar",
			wantMode: ModeRecommend, wantCPUMn: 0.01, wantSrc: "default:injected-sidecar"},
		{name: "no policy: injected sidecar under default Recommend stays Recommend", ns: "other", container: "istio-proxy", class: "injected-sidecar",
			wantMode: ModeRecommend, wantCPUMn: 0.01, wantSrc: "default:injected-sidecar"},
		{name: "annotation enables + bounds a sidecar", ns: "other", container: "istio-proxy", class: "injected-sidecar",
			anns: map[string]string{
				"tailor.k8s-tailor.dev/container.istio-proxy.mode":    "InPlace",
				"tailor.k8s-tailor.dev/container.istio-proxy.min-cpu": "50m",
				"tailor.k8s-tailor.dev/container.istio-proxy.max-mem": "1Gi",
			},
			wantMode: ModeInPlace, wantCPUMn: 0.05, wantMemMx: 1024 * mi, wantSrc: "annotation"},
		{name: "annotation exclude beats a policy rule", ns: "apps", container: "istio-proxy", class: "injected-sidecar",
			anns:     map[string]string{"tailor.k8s-tailor.dev/container.istio-proxy.exclude": "true"},
			wantMode: ModeOff, wantExcl: true, wantCPUMn: 0.02, wantMemMx: 512 * mi, wantProxy: true, wantSrc: "annotation"},
		{name: "annotation mode on app container", ns: "apps", container: "app", class: "app",
			anns:     map[string]string{"tailor.k8s-tailor.dev/container.app.mode": "Recommend"},
			wantMode: ModeRecommend, wantCPUMn: 0.5, wantSrc: "annotation"},
		{name: "workload Off: sidecar stays Off", ns: "apps", container: "linkerd-proxy", class: "injected-sidecar",
			anns:     map[string]string{AnnMode: "Off"},
			wantMode: ModeOff, wantCPUMn: 0.01, wantSrc: "default:injected-sidecar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := s.Resolve(c.ns, nil, c.anns)
			r := s.ResolveContainer(base, c.anns, c.container, c.class)
			if r.Mode != c.wantMode {
				t.Errorf("mode = %q, want %q", r.Mode, c.wantMode)
			}
			if r.Excluded != c.wantExcl {
				t.Errorf("excluded = %v, want %v", r.Excluded, c.wantExcl)
			}
			if r.CPU.Min != c.wantCPUMn {
				t.Errorf("cpu.min = %v, want %v", r.CPU.Min, c.wantCPUMn)
			}
			if r.Mem.Max != c.wantMemMx {
				t.Errorf("mem.max = %v, want %v", r.Mem.Max, c.wantMemMx)
			}
			if r.ProxyAnnotations != c.wantProxy {
				t.Errorf("proxyAnnotations = %v, want %v", r.ProxyAnnotations, c.wantProxy)
			}
			if r.ContainerPolicy != c.wantSrc {
				t.Errorf("containerPolicy = %q, want %q", r.ContainerPolicy, c.wantSrc)
			}
			if r.ContainerClass != c.class {
				t.Errorf("class = %q", r.ContainerClass)
			}
		})
	}
}

// Rules from a profile referenced by the policy apply after the policy's own.
func TestResolveContainerProfileRules(t *testing.T) {
	s := storeWith(t,
		cr("TailorProfile", "meshy", map[string]any{
			"containers": []any{map[string]any{"match": map[string]any{"classes": []any{"injected-sidecar"}}, "mode": "InPlace"}},
		}),
		cr("TailorPolicy", "p", map[string]any{"mode": "InPlace", "profileRef": "meshy"}),
	)
	r := s.ResolveContainer(s.Resolve("x", nil, nil), nil, "istio-proxy", "injected-sidecar")
	if r.Mode != ModeInPlace || r.ContainerPolicy != "profile:meshy containers[0]" {
		t.Fatalf("mode=%q src=%q", r.Mode, r.ContainerPolicy)
	}
}

// The UI writes TailorPolicy wholesale; it must not drop containers rules set
// with kubectl when the view carries none.
func TestUpsertPolicyPreservesContainers(t *testing.T) {
	s := meshStore(t)
	views := s.ListPolicies()
	if len(views) != 1 || len(views[0].Containers) != 3 {
		t.Fatalf("view containers = %+v", views)
	}
	v := views[0]
	v.Containers = nil
	v.CPUMargin = 0.3
	if err := s.UpsertPolicy(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	s.Refresh(context.Background())
	if got := s.ListPolicies()[0].Containers; len(got) != 3 {
		t.Fatalf("containers after upsert = %d, want 3 (preserved)", len(got))
	}
}

func TestSidecarNamesFromConfig(t *testing.T) {
	s := storeWith(t, cr("TailorConfig", "cluster", map[string]any{"sidecarNames": []any{"acme-*"}}))
	if g := s.Global(); len(g.SidecarNames) != 1 || g.SidecarNames[0] != "acme-*" {
		t.Fatalf("sidecarNames = %v", g.SidecarNames)
	}
}
