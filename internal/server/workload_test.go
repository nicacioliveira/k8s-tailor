package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// workloadCluster: Deployment portal/web with an app container and a native
// sidecar in the template; its running pod also carries an injected
// istio-proxy, a plain init container and an ephemeral debug container.
func workloadCluster(t *testing.T, cfg config.Config) (*Server, *fake.Clientset) {
	t.Helper()
	always := corev1.ContainerRestartPolicyAlways
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "portal"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "log-shipper", RestartPolicy: &always}, {Name: "migrate"}},
		}}},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "portal",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc-x", Namespace: "portal",
			Annotations:     map[string]string{k8s.AnnIstioStatus: "{}"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
		Spec: corev1.PodSpec{
			Containers:          []corev1.Container{{Name: "app"}, {Name: "istio-proxy"}},
			InitContainers:      []corev1.Container{{Name: "log-shipper", RestartPolicy: &always}, {Name: "migrate"}},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger"}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewSimpleClientset(dep, rs, pod)
	vmSrv := fakeVM(t)
	vmc := vm.New(vmSrv.URL)
	ksvc := schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}
	kc := k8s.NewFromClients(cs, dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ksvc: "ServiceList"}), nil)
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), cfg)
	eng := recommender.NewEngine(cfg, vmc, kc, pol, pricing.New(cfg, vmc))
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return New(cfg, eng, vmc, actuation.NewStore(10), nil), cs
}

func webAnnotations(t *testing.T, cs *fake.Clientset) map[string]string {
	t.Helper()
	d, err := cs.AppsV1().Deployments("portal").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Annotations
}

func TestPostWorkloadContainer(t *testing.T) {
	const tok = "Bearer s3cret"
	srv, cs := workloadCluster(t, config.Config{APIToken: "s3cret"})
	h := srv.Handler()
	modeKey := policy.AnnContainer("istio-proxy", "mode")
	exclKey := policy.AnnContainer("log-shipper", "exclude")

	// Auth: same Bearer gate as every other write.
	if rr := do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"istio-proxy","mode":"InPlace"}`, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rr.Code)
	}

	rr := do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"istio-proxy","mode":"InPlace"}`, tok)
	if rr.Code != 200 {
		t.Fatalf("set mode: %d %s", rr.Code, rr.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true || resp["container"] != "istio-proxy" || resp["mode"] != "InPlace" {
		t.Fatalf("response = %v", resp)
	}
	if _, has := resp["exclude"]; has {
		t.Fatalf("exclude was not sent and must not be echoed: %v", resp)
	}
	if got := webAnnotations(t, cs)[modeKey]; got != "InPlace" {
		t.Fatalf("%s = %q", modeKey, got)
	}

	// Native sidecar exclusion, then removal.
	rr = do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"log-shipper","exclude":true}`, tok)
	if rr.Code != 200 || webAnnotations(t, cs)[exclKey] != "true" {
		t.Fatalf("exclude: %d %s, ann %q", rr.Code, rr.Body, webAnnotations(t, cs)[exclKey])
	}
	rr = do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"log-shipper","exclude":false}`, tok)
	if _, still := webAnnotations(t, cs)[exclKey]; rr.Code != 200 || still {
		t.Fatalf("exclude=false must remove the annotation: %d %s", rr.Code, rr.Body)
	}
	// inherit removes the mode override; "" is the same.
	for _, m := range []string{"inherit", ""} {
		do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"istio-proxy","mode":"Off"}`, tok)
		rr = do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"istio-proxy","mode":"`+m+`"}`, tok)
		if _, still := webAnnotations(t, cs)[modeKey]; rr.Code != 200 || still || !strings.Contains(rr.Body.String(), `"mode":"inherit"`) {
			t.Fatalf("mode %q: %d %s (annotation still present: %v)", m, rr.Code, rr.Body, still)
		}
	}
}

func TestPostWorkloadErrors(t *testing.T) {
	srv, cs := workloadCluster(t, config.Config{})
	h := srv.Handler()
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"bad json", `{"namespace":`, 400, "bad_request"},
		{"missing fields", `{"namespace":"portal","container":"app","mode":"Off"}`, 400, "bad_request"},
		{"exclude without container", `{"namespace":"portal","kind":"Deployment","workload":"web","exclude":true}`, 400, "bad_request"},
		{"profile with container", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"app","profile":"p"}`, 400, "bad_request"},
		{"nothing to change", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"app"}`, 400, "nothing_to_change"},
		{"invalid container mode", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"app","mode":"Auto"}`, 400, "invalid_mode"},
		{"invalid workload mode", `{"namespace":"portal","kind":"Deployment","workload":"web","mode":"inplace"}`, 400, "invalid_mode"},
		{"init container", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"migrate","mode":"InPlace"}`, 400, "container_not_resizable"},
		{"ephemeral container", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"debugger","exclude":true}`, 400, "container_not_resizable"},
		{"ksvc kind", `{"namespace":"portal","kind":"ksvc","workload":"web","container":"app","mode":"Off"}`, 400, "unsupported_kind"},
		{"group kind, workload level", `{"namespace":"portal","kind":"group","workload":"g","mode":"Off"}`, 400, "unsupported_kind"},
		{"unknown container", `{"namespace":"portal","kind":"Deployment","workload":"web","container":"nope","mode":"Off"}`, 404, "container_not_found"},
		{"unknown workload, container", `{"namespace":"portal","kind":"Deployment","workload":"nope","container":"app","mode":"Off"}`, 404, "workload_not_found"},
		{"unknown workload, workload level", `{"namespace":"portal","kind":"Deployment","workload":"nope","mode":"Off"}`, 404, "workload_not_found"},
	}
	for _, c := range cases {
		rr := do(h, "POST", "/api/workload", c.body, "")
		var e struct{ Error, Code string }
		_ = json.Unmarshal(rr.Body.Bytes(), &e)
		if rr.Code != c.status || e.Code != c.code || e.Error == "" {
			t.Errorf("%s: %d %s, want %d code=%s", c.name, rr.Code, rr.Body, c.status, c.code)
		}
	}
	for k := range webAnnotations(t, cs) {
		t.Errorf("a rejected request wrote annotation %s", k)
	}
	// Body limit: shared write guard.
	big := `{"namespace":"` + strings.Repeat("a", maxWriteBody) + `"}`
	if rr := do(h, "POST", "/api/workload", big, ""); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", rr.Code)
	}
	// Legacy workload-level write still answers {"ok":true}.
	rr := do(h, "POST", "/api/workload", `{"namespace":"portal","kind":"Deployment","workload":"web","mode":"Recommend","profile":""}`, "")
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"ok":true}` {
		t.Fatalf("legacy write: %d %s", rr.Code, rr.Body)
	}
	if got := webAnnotations(t, cs)[policy.AnnMode]; got != "Recommend" {
		t.Fatalf("workload mode annotation = %q", got)
	}
}

// /api/recommendations carries per-resource confidence (cpuConfident /
// memConfident) next to the aggregate confident flag.
func TestRecommendationsExposePerResourceConfidence(t *testing.T) {
	srv, eng := newTestServer(t, config.Config{})
	eng.Seed([]recommender.Recommendation{{Namespace: "a", Kind: "Deployment", Workload: "w", Container: "c",
		Confident: true, CPUConfident: false, MemConfident: true}}, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/recommendations", nil))
	var body struct {
		Recommendations []map[string]any `json:"recommendations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body.Recommendations) != 1 {
		t.Fatalf("decode: %v %s", err, rr.Body)
	}
	r := body.Recommendations[0]
	if r["confident"] != true || r["cpuConfident"] != false || r["memConfident"] != true {
		t.Fatalf("confidence fields = confident:%v cpuConfident:%v memConfident:%v", r["confident"], r["cpuConfident"], r["memConfident"])
	}
}
