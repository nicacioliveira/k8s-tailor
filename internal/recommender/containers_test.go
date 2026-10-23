package recommender

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// fakeUsageVM answers every range query with one series per container of pod
// "web-abc-x" (constant usage), whatever the query.
func fakeUsageVM(t *testing.T, containers ...string) *vm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		var series []string
		for _, c := range containers {
			var vals []string
			for i := 0; i < 12; i++ {
				vals = append(vals, fmt.Sprintf(`[%d,"%s"]`, now-int64(i*300), "0.05"))
			}
			if strings.Contains(r.URL.Query().Get("query"), "memory") {
				vals = vals[:0]
				for i := 0; i < 12; i++ {
					vals = append(vals, fmt.Sprintf(`[%d,"%d"]`, now-int64(i*300), 64*1024*1024))
				}
			}
			series = append(series, fmt.Sprintf(`{"metric":{"namespace":"apps","pod":"web-abc-x","container":%q},"values":[%s]}`,
				c, strings.Join(vals, ",")))
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[%s]}}`, strings.Join(series, ","))
	}))
	t.Cleanup(srv.Close)
	return vm.New(srv.URL)
}

var always = corev1.ContainerRestartPolicyAlways

func reqCtr(name, cpu, mem string) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
	}}}
}

type meshCase struct {
	native   bool              // istio-proxy as a native sidecar (initContainers)
	depAnns  map[string]string // annotations on the Deployment
	policies []runtime.Object
	podMut   func(*corev1.Pod)    // optional: adjust the running pod
	cfgMut   func(*config.Config) // optional: adjust the engine config
}

func meshEngine(t *testing.T, mc meshCase) *Engine {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", Annotations: mc.depAnns},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{reqCtr("app", "1", "512Mi")},
		}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc-x", Namespace: "apps",
			Annotations:     map[string]string{k8s.AnnIstioStatus: `{"containers":["istio-proxy"]}`},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{reqCtr("istio-init", "10m", "10Mi")},
			Containers:     []corev1.Container{reqCtr("app", "1", "512Mi")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	proxy := reqCtr("istio-proxy", "100m", "128Mi")
	if mc.native {
		proxy.RestartPolicy = &always
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, proxy)
	} else {
		pod.Spec.Containers = append(pod.Spec.Containers, proxy)
	}
	if mc.podMut != nil {
		mc.podMut(pod)
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	ksvc := schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}
	kc := k8s.NewFromClients(fake.NewSimpleClientset(dep, rs, pod),
		dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{ksvc: "ServiceList"}), nil)
	cfg := config.Config{MinSamples: 1, Window: 2 * time.Hour, HalfLife: time.Hour, CPUQuant: 0.9, MemQuant: 0.95, Margin: 0.15}
	if mc.cfgMut != nil {
		mc.cfgMut(&cfg)
	}
	gvrs := map[schema.GroupVersionResource]string{
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
	}
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrs, mc.policies...), cfg)
	pol.Refresh(context.Background())
	e := NewEngine(cfg, fakeUsageVM(t, "app", "istio-proxy", "istio-init"), kc, pol, pricing.New(cfg, nil))
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

func polCR(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailor.k8s-tailor.dev/v1alpha1", "kind": "TailorPolicy",
		"metadata": map[string]any{"name": name}, "spec": spec,
	}}
}

var inPlacePolicy = polCR("all", map[string]any{"mode": "InPlace", "selector": map[string]any{"namespaces": []any{"apps"}},
	"cpu": map[string]any{"min": "500m"}})

func TestRefreshClassifiesInjectedSidecar(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			e := meshEngine(t, meshCase{native: native, policies: []runtime.Object{inPlacePolicy}})
			app, ok := e.Get("apps", "Deployment", "web", "app")
			if !ok || app.Class != k8s.ClassApp || app.NotInTemplate || app.Mode != policy.ModeInPlace {
				t.Fatalf("app entry = ok:%v %+v", ok, app)
			}
			px, ok := e.Get("apps", "Deployment", "web", "istio-proxy")
			if !ok {
				t.Fatal("istio-proxy has no recommendation (injected sidecars must be visible)")
			}
			if px.Class != k8s.ClassInjectedSidecar || !px.NotInTemplate || px.Mesh != k8s.MeshIstio || px.Native != native {
				t.Fatalf("istio-proxy entry = %+v", px)
			}
			if px.Mode != policy.ModeRecommend {
				t.Fatalf("istio-proxy mode = %q, want Recommend (safe default)", px.Mode)
			}
			// Own profile: the app's 500m floor must not leak into the proxy.
			if px.Rec.Recommended.CPURequest >= 0.5 {
				t.Fatalf("proxy cpu rec = %v: app floor leaked", px.Rec.Recommended.CPURequest)
			}
			if px.Rec.Current.CPURequest != 0.1 || px.Rec.ContainerClass != "injected-sidecar" || px.Rec.ContainerPolicy != "default:injected-sidecar" {
				t.Fatalf("proxy rec = current %v class %q src %q", px.Rec.Current.CPURequest, px.Rec.ContainerClass, px.Rec.ContainerPolicy)
			}
			if _, ok := e.Get("apps", "Deployment", "web", "istio-init"); ok {
				t.Fatal("istio-init (plain init container) must never get a recommendation")
			}
		})
	}
}

func TestRefreshContainerExclusionAndOptIn(t *testing.T) {
	cases := []struct {
		name     string
		mc       meshCase
		wantPx   bool
		wantMode policy.Mode
		wantPath string
	}{
		{"annotation exclude", meshCase{policies: []runtime.Object{inPlacePolicy},
			depAnns: map[string]string{"tailor.k8s-tailor.dev/container.istio-proxy.exclude": "true"}}, false, "", ""},
		{"annotation opt-in", meshCase{policies: []runtime.Object{inPlacePolicy},
			depAnns: map[string]string{"tailor.k8s-tailor.dev/container.istio-proxy.mode": "InPlace"}}, true, policy.ModeInPlace, "in-place"},
		{"policy rule exclude", meshCase{policies: []runtime.Object{polCR("p", map[string]any{"mode": "InPlace",
			"containers": []any{map[string]any{"match": map[string]any{"classes": []any{"injected-sidecar"}}, "exclude": true}}})}}, false, "", ""},
		{"policy rule rollout via proxy annotations", meshCase{policies: []runtime.Object{polCR("p", map[string]any{"mode": "InPlace",
			"containers": []any{map[string]any{"match": map[string]any{"names": []any{"istio-proxy"}}, "mode": "InPlace", "apply": "Rollout", "proxyAnnotations": true}}})}},
			true, policy.ModeInPlace, "proxy-annotations:istio"},
		{"rollout without proxy annotations is impossible", meshCase{policies: []runtime.Object{polCR("p", map[string]any{"mode": "InPlace",
			"containers": []any{map[string]any{"match": map[string]any{"names": []any{"istio-proxy"}}, "mode": "InPlace", "apply": "Rollout"}}})}},
			true, policy.ModeInPlace, "none:not-in-template"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := meshEngine(t, c.mc)
			px, ok := e.Get("apps", "Deployment", "web", "istio-proxy")
			if ok != c.wantPx {
				t.Fatalf("proxy entry present = %v, want %v", ok, c.wantPx)
			}
			if !ok {
				return
			}
			if px.Mode != c.wantMode || px.Rec.ActuationPath != c.wantPath {
				t.Fatalf("mode=%q path=%q, want %q %q", px.Mode, px.Rec.ActuationPath, c.wantMode, c.wantPath)
			}
			if _, ok := e.Get("apps", "Deployment", "web", "app"); !ok {
				t.Fatal("app lost its recommendation")
			}
		})
	}
}

// The per-row workload toggle must not enable an injected sidecar.
func TestSetWorkloadDoesNotEnableSidecar(t *testing.T) {
	e := meshEngine(t, meshCase{})
	e.refreshImpl = func(context.Context) error { return nil } // keep the seeded state
	m := "InPlace"
	if err := e.SetWorkload(context.Background(), "apps", "Deployment", "web", &m, nil); err != nil {
		t.Fatal(err)
	}
	if px, _ := e.Get("apps", "Deployment", "web", "istio-proxy"); px.Mode != policy.ModeRecommend {
		t.Fatalf("proxy mode after workload toggle = %q, want Recommend", px.Mode)
	}
	if app, _ := e.Get("apps", "Deployment", "web", "app"); app.Mode != policy.ModeInPlace {
		t.Fatalf("app mode = %q", app.Mode)
	}
}

func TestMergeObservedContainers(t *testing.T) {
	w := k8s.Workload{Kind: "Deployment", Name: "web", Containers: []k8s.Container{{Name: "app", Class: k8s.ClassApp, InTemplate: true}}}
	pods := []k8s.RunningPod{
		{Owner: k8s.Owner{Kind: "Deployment", Name: "web"}, Containers: []k8s.Container{
			{Name: "app", Class: k8s.ClassApp}, {Name: "mystery", Class: k8s.ClassApp, CPURequest: 0.2},
			{Name: "istio-init", Class: k8s.ClassInit},
		}},
		{Owner: k8s.Owner{Kind: "Deployment", Name: "other"}, Containers: []k8s.Container{{Name: "x", Class: k8s.ClassApp}}},
	}
	got := MergeObservedContainers(w, pods)
	if len(got) != 2 || got[1].Name != "mystery" || got[1].Class != k8s.ClassInjectedSidecar || got[1].InTemplate || got[1].CPURequest != 0.2 {
		t.Fatalf("merged = %+v", got)
	}
}

// A native sidecar (initContainer, restartPolicy Always) reports its OOMKill in
// initContainerStatuses; the refresh learns the floor from it end to end, and
// the sidecar's recommendation carries the OOM risk.
func TestRefreshLearnsOOMFloorFromNativeSidecar(t *testing.T) {
	killed := time.Now().Add(-time.Minute)
	e := meshEngine(t, meshCase{native: true, policies: []runtime.Object{inPlacePolicy},
		cfgMut: func(c *config.Config) {
			c.OOMLearn, c.OOMGrowthFactor, c.OOMFloorTTL = true, 1.5, 24*time.Hour
		},
		podMut: func(p *corev1.Pod) {
			for i := range p.Spec.InitContainers {
				if p.Spec.InitContainers[i].Name == "istio-proxy" {
					p.Spec.InitContainers[i].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
				}
			}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "istio-proxy",
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", FinishedAt: metav1.NewTime(killed)}}}}
		},
	})
	if got, want := e.oomFloorFor("apps/Deployment/web/istio-proxy"), 1.5*256*1024*1024; got != want {
		t.Fatalf("learned floor = %v, want %v (256Mi x 1.5)", got, want)
	}
	px, ok := e.Get("apps", "Deployment", "web", "istio-proxy")
	if !ok || !px.Rec.OOMRisk || !px.Native {
		t.Fatalf("istio-proxy entry ok=%v OOMRisk=%v native=%v, want an OOM-risk native sidecar", ok, px.Rec.OOMRisk, px.Native)
	}
}
