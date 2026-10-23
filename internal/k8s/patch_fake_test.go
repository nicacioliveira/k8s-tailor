package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func strp(s string) *string { return &s }

// patches returns every patch action recorded by the fake, in order.
func patches(cs *fake.Clientset) []k8stesting.PatchAction {
	var out []k8stesting.PatchAction
	for _, a := range cs.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetVerb() == "patch" {
			out = append(out, p)
		}
	}
	return out
}

func onlyPatch(t *testing.T, cs *fake.Clientset) k8stesting.PatchAction {
	t.Helper()
	ps := patches(cs)
	if len(ps) != 1 {
		t.Fatalf("patch actions = %d, want 1", len(ps))
	}
	return ps[0]
}

func patchBody(t *testing.T, p k8stesting.PatchAction) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(p.GetPatch(), &m); err != nil {
		t.Fatalf("patch body %q: %v", p.GetPatch(), err)
	}
	return m
}

// dig walks nested maps (and the first element of a list) of a parsed patch body.
func dig(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, k := range path {
		if l, ok := v.([]any); ok {
			if len(l) != 1 {
				t.Fatalf("at %q: list of %d, want 1", k, len(l))
			}
			v = l[0]
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %q: %T is not an object", k, v)
		}
		v, ok = m[k]
		if !ok {
			t.Fatalf("missing key %q in %v", k, m)
		}
	}
	return v
}

func podWith(ns, name string, spec corev1.PodSpec) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
}

// ResizePodContainer sends a STRATEGIC merge patch to the pods/resize
// subresource with requests always, limits only when given, targeting
// spec.containers or (native sidecar) spec.initContainers.
func TestResizePodContainer(t *testing.T) {
	cases := []struct {
		name               string
		native             bool
		cpuLimit, memLimit string
		wantList           string
		wantLimits         map[string]any
	}{
		{"app, requests only", false, "", "", "containers", nil},
		{"app, mem limit", false, "", "1Gi", "containers", map[string]any{"memory": "1Gi"}},
		{"native sidecar, both limits", true, "1", "2Gi", "initContainers", map[string]any{"cpu": "1", "memory": "2Gi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(podWith("apps", "p", corev1.PodSpec{
				InitContainers: []corev1.Container{nativeCtr("proxy")},
				Containers:     []corev1.Container{ctr("app")},
			}))
			c := NewFromClients(cs, emptyDyn(), nil)
			target := "app"
			if tc.native {
				target = "proxy"
			}
			if err := c.ResizePodContainer(context.Background(), "apps", "p", target, tc.native, "250m", "300Mi", tc.cpuLimit, tc.memLimit); err != nil {
				t.Fatal(err)
			}
			p := onlyPatch(t, cs)
			if p.GetSubresource() != "resize" || p.GetPatchType() != types.StrategicMergePatchType || p.GetName() != "p" || p.GetNamespace() != "apps" {
				t.Fatalf("patch = sub %q type %q %s/%s", p.GetSubresource(), p.GetPatchType(), p.GetNamespace(), p.GetName())
			}
			body := patchBody(t, p)
			spec := dig(t, body, "spec").(map[string]any)
			if len(spec) != 1 {
				t.Fatalf("spec keys = %v, want only %s", spec, tc.wantList)
			}
			if n := dig(t, spec, tc.wantList, "name"); n != target {
				t.Fatalf("container name = %v", n)
			}
			res := dig(t, spec, tc.wantList, "resources").(map[string]any)
			if !reflect.DeepEqual(res["requests"], map[string]any{"cpu": "250m", "memory": "300Mi"}) {
				t.Errorf("requests = %v", res["requests"])
			}
			lim, has := res["limits"]
			if tc.wantLimits == nil {
				if has {
					t.Errorf("limits = %v, want absent (untouched)", lim)
				}
			} else if !reflect.DeepEqual(lim, tc.wantLimits) {
				t.Errorf("limits = %v, want %v", lim, tc.wantLimits)
			}
		})
	}
}

func TestResizePodContainerError(t *testing.T) {
	c := NewFromClients(fake.NewSimpleClientset(), emptyDyn(), nil)
	if err := c.ResizePodContainer(context.Background(), "apps", "missing", "app", false, "1", "1Gi", "", ""); err == nil {
		t.Fatal("resizing a missing pod must return the API error")
	}
}

func workloadObjects() []runtime.Object {
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"keep": "me", "drop": "x"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{nativeCtr("proxy")},
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi"),
					"nvidia.com/gpu": resource.MustParse("1")},
			}}},
		},
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "apps", Name: name, Annotations: map[string]string{"owner": "team-a", "stale": "y"}}
	}
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: meta("dep"), Spec: appsv1.DeploymentSpec{Template: tmpl}},
		&appsv1.StatefulSet{ObjectMeta: meta("sts"), Spec: appsv1.StatefulSetSpec{Template: tmpl}},
		&appsv1.DaemonSet{ObjectMeta: meta("ds"), Spec: appsv1.DaemonSetSpec{Template: tmpl}},
		&corev1.Pod{ObjectMeta: meta("pod"), Spec: tmpl.Spec},
	}
}

// templateOf reads back the pod template of a workload from the fake tracker.
func templateOf(t *testing.T, cs *fake.Clientset, kind, name string) corev1.PodTemplateSpec {
	t.Helper()
	ctx := context.Background()
	switch kind {
	case "Deployment":
		d, err := cs.AppsV1().Deployments("apps").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template
	case "StatefulSet":
		s, err := cs.AppsV1().StatefulSets("apps").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return s.Spec.Template
	case "DaemonSet":
		d, err := cs.AppsV1().DaemonSets("apps").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template
	}
	t.Fatalf("unknown kind %s", kind)
	return corev1.PodTemplateSpec{}
}

func objectAnnotations(t *testing.T, cs *fake.Clientset, kind, name string) map[string]string {
	t.Helper()
	ctx := context.Background()
	var m metav1.Object
	var err error
	switch kind {
	case "Deployment":
		m, err = cs.AppsV1().Deployments("apps").Get(ctx, name, metav1.GetOptions{})
	case "StatefulSet":
		m, err = cs.AppsV1().StatefulSets("apps").Get(ctx, name, metav1.GetOptions{})
	case "DaemonSet":
		m, err = cs.AppsV1().DaemonSets("apps").Get(ctx, name, metav1.GetOptions{})
	case "Pod":
		m, err = cs.CoreV1().Pods("apps").Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		t.Fatal(err)
	}
	return m.GetAnnotations()
}

var workloadKinds = map[string]string{"Deployment": "dep", "StatefulSet": "sts", "DaemonSet": "ds"}

// PatchWorkloadResources strategic-merges one container's resources into the
// pod TEMPLATE: requests change, a passed limit changes, other limits and
// extended resources survive, pod annotations merge, and native sidecars are
// addressed in initContainers.
func TestPatchWorkloadResources(t *testing.T) {
	for kind, name := range workloadKinds {
		t.Run(kind, func(t *testing.T) {
			cs := fake.NewSimpleClientset(workloadObjects()...)
			c := NewFromClients(cs, emptyDyn(), nil)
			err := c.PatchWorkloadResources(context.Background(), kind, "apps", name, "app", false,
				"500m", "768Mi", "2", "", map[string]string{AnnLastApplied: `{"app":"2026-01-02T03:04:05Z"}`})
			if err != nil {
				t.Fatal(err)
			}
			p := onlyPatch(t, cs)
			if p.GetPatchType() != types.StrategicMergePatchType || p.GetSubresource() != "" || p.GetName() != name {
				t.Fatalf("patch type %q sub %q name %q", p.GetPatchType(), p.GetSubresource(), p.GetName())
			}
			body := patchBody(t, p)
			if got := dig(t, body, "spec", "template", "metadata", "annotations", AnnLastApplied); got != `{"app":"2026-01-02T03:04:05Z"}` {
				t.Errorf("template annotation in patch = %v", got)
			}
			if lim := dig(t, body, "spec", "template", "spec", "containers", "resources", "limits"); !reflect.DeepEqual(lim, map[string]any{"cpu": "2"}) {
				t.Errorf("limits in patch = %v, want only cpu", lim)
			}

			tmpl := templateOf(t, cs, kind, name)
			if len(tmpl.Spec.Containers) != 1 {
				t.Fatalf("containers = %d (strategic merge must not append)", len(tmpl.Spec.Containers))
			}
			r := tmpl.Spec.Containers[0].Resources
			checks := map[string][2]resource.Quantity{
				"req cpu": {r.Requests[corev1.ResourceCPU], resource.MustParse("500m")},
				"req mem": {r.Requests[corev1.ResourceMemory], resource.MustParse("768Mi")},
				"lim cpu": {r.Limits[corev1.ResourceCPU], resource.MustParse("2")},
				"lim mem": {r.Limits[corev1.ResourceMemory], resource.MustParse("2Gi")},
				"gpu":     {r.Limits["nvidia.com/gpu"], resource.MustParse("1")},
			}
			for k, q := range checks {
				if q[0].Cmp(q[1]) != 0 {
					t.Errorf("%s = %s, want %s", k, q[0].String(), q[1].String())
				}
			}
			if tmpl.Annotations["keep"] != "me" || tmpl.Annotations[AnnLastApplied] == "" {
				t.Errorf("template annotations = %v", tmpl.Annotations)
			}
			if init := tmpl.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU]; init.Cmp(resource.MustParse("100m")) != 0 {
				t.Errorf("native sidecar touched: %s", init.String())
			}
		})
	}
}

func TestPatchWorkloadResourcesNativeSidecar(t *testing.T) {
	cs := fake.NewSimpleClientset(workloadObjects()...)
	c := NewFromClients(cs, emptyDyn(), nil)
	if err := c.PatchWorkloadResources(context.Background(), "Deployment", "apps", "dep", "proxy", true, "50m", "64Mi", "", "", nil); err != nil {
		t.Fatal(err)
	}
	body := patchBody(t, onlyPatch(t, cs))
	tmplPatch := dig(t, body, "spec", "template").(map[string]any)
	if _, has := tmplPatch["metadata"]; has {
		t.Errorf("no pod annotations given, yet patch carries metadata: %v", tmplPatch)
	}
	spec := tmplPatch["spec"].(map[string]any)
	if _, has := spec["containers"]; has {
		t.Fatalf("native sidecar patch must target initContainers only: %v", spec)
	}
	if n := dig(t, spec, "initContainers", "name"); n != "proxy" {
		t.Fatalf("initContainers name = %v", n)
	}
	tmpl := templateOf(t, cs, "Deployment", "dep")
	if len(tmpl.Spec.InitContainers) != 1 {
		t.Fatalf("initContainers = %d", len(tmpl.Spec.InitContainers))
	}
	if q := tmpl.Spec.InitContainers[0].Resources.Requests[corev1.ResourceMemory]; q.Cmp(resource.MustParse("64Mi")) != 0 {
		t.Errorf("sidecar mem = %s", q.String())
	}
	if q := tmpl.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("app container touched: %s", q.String())
	}
}

func TestPatchWorkloadResourcesUnsupportedKind(t *testing.T) {
	cs := fake.NewSimpleClientset(workloadObjects()...)
	c := NewFromClients(cs, emptyDyn(), nil)
	for _, kind := range []string{"Pod", "ksvc", "group", ""} {
		err := c.PatchWorkloadResources(context.Background(), kind, "apps", "x", "app", false, "1", "1Gi", "", "", nil)
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("kind %q: err = %v, want unsupported", kind, err)
		}
	}
	if n := len(patches(cs)); n != 0 {
		t.Fatalf("unsupported kinds sent %d patches", n)
	}
	if err := c.PatchWorkloadResources(context.Background(), "Deployment", "apps", "missing", "app", false, "1", "1Gi", "", "", nil); err == nil {
		t.Fatal("missing workload must return the API error")
	}
}

// PatchWorkloadPodAnnotations JSON-merges into the template metadata; a nil
// value is sent as null (removing the key).
func TestPatchWorkloadPodAnnotations(t *testing.T) {
	for kind, name := range workloadKinds {
		t.Run(kind, func(t *testing.T) {
			cs := fake.NewSimpleClientset(workloadObjects()...)
			c := NewFromClients(cs, emptyDyn(), nil)
			err := c.PatchWorkloadPodAnnotations(context.Background(), kind, "apps", name, map[string]*string{
				"sidecar.istio.io/proxyCPU": strp("150m"),
				"drop":                      nil,
			})
			if err != nil {
				t.Fatal(err)
			}
			p := onlyPatch(t, cs)
			if p.GetPatchType() != types.MergePatchType {
				t.Fatalf("patch type = %q, want merge", p.GetPatchType())
			}
			anns := dig(t, patchBody(t, p), "spec", "template", "metadata", "annotations").(map[string]any)
			if v, has := anns["drop"]; !has || v != nil {
				t.Errorf("drop = %v (present=%v), want explicit null", v, has)
			}
			got := templateOf(t, cs, kind, name).Annotations
			want := map[string]string{"keep": "me", "sidecar.istio.io/proxyCPU": "150m"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("template annotations = %v, want %v", got, want)
			}
			if obj := objectAnnotations(t, cs, kind, name); obj["owner"] != "team-a" || obj["sidecar.istio.io/proxyCPU"] != "" {
				t.Errorf("object metadata must be untouched: %v", obj)
			}
		})
	}
}

func TestPatchWorkloadPodAnnotationsUnsupportedKind(t *testing.T) {
	cs := fake.NewSimpleClientset(workloadObjects()...)
	c := NewFromClients(cs, emptyDyn(), nil)
	err := c.PatchWorkloadPodAnnotations(context.Background(), "Pod", "apps", "pod", map[string]*string{"a": strp("b")})
	if err == nil || !strings.Contains(err.Error(), `"Pod"`) {
		t.Fatalf("err = %v, want unsupported kind Pod", err)
	}
	if n := len(patches(cs)); n != 0 {
		t.Fatalf("sent %d patches", n)
	}
}

// AnnotateWorkload JSON-merges into the OBJECT metadata (never the template)
// for every supported kind; nil removes a key.
func TestAnnotateWorkload(t *testing.T) {
	kinds := map[string]string{"Deployment": "dep", "StatefulSet": "sts", "DaemonSet": "ds", "Pod": "pod"}
	for kind, name := range kinds {
		t.Run(kind, func(t *testing.T) {
			cs := fake.NewSimpleClientset(workloadObjects()...)
			c := NewFromClients(cs, emptyDyn(), nil)
			err := c.AnnotateWorkload(context.Background(), kind, "apps", name, map[string]*string{
				"tailor.k8s-tailor.dev/policy": strp("conservative"),
				"stale":                        nil,
			})
			if err != nil {
				t.Fatal(err)
			}
			p := onlyPatch(t, cs)
			if p.GetPatchType() != types.MergePatchType || p.GetName() != name {
				t.Fatalf("patch type %q name %q", p.GetPatchType(), p.GetName())
			}
			body := patchBody(t, p)
			if _, has := body["spec"]; has {
				t.Fatalf("AnnotateWorkload must not touch spec: %v", body)
			}
			got := objectAnnotations(t, cs, kind, name)
			want := map[string]string{"owner": "team-a", "tailor.k8s-tailor.dev/policy": "conservative"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("annotations = %v, want %v", got, want)
			}
			if kind != "Pod" {
				if tmpl := templateOf(t, cs, kind, name).Annotations; tmpl["tailor.k8s-tailor.dev/policy"] != "" {
					t.Errorf("template annotations changed: %v", tmpl)
				}
			}
		})
	}
}

func TestAnnotateWorkloadUnsupportedKind(t *testing.T) {
	cs := fake.NewSimpleClientset(workloadObjects()...)
	c := NewFromClients(cs, emptyDyn(), nil)
	err := c.AnnotateWorkload(context.Background(), "ksvc", "apps", "svc", map[string]*string{"a": strp("b")})
	if err == nil || !strings.Contains(err.Error(), `"ksvc"`) {
		t.Fatalf("err = %v", err)
	}
	if n := len(patches(cs)); n != 0 {
		t.Fatalf("sent %d patches", n)
	}
	if err := c.AnnotateWorkload(context.Background(), "Deployment", "apps", "missing", nil); err == nil {
		t.Fatal("missing workload must return the API error")
	}
}

// PatchKnativeService drops the pinned revision name (Knative would reject a
// template change under the same name), merges pod annotations into the
// template, and surfaces Get errors and templates without containers.
func TestPatchKnativeServiceRevisionNameAndAnnotations(t *testing.T) {
	obj := ksvcWith(map[string]any{"name": "app", "image": "x"})
	if err := unstructured.SetNestedField(obj.Object, "svc-00007", "spec", "template", "metadata", "name"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedStringMap(obj.Object, map[string]string{"autoscaling.knative.dev/min-scale": "1"},
		"spec", "template", "metadata", "annotations"); err != nil {
		t.Fatal(err)
	}
	dyn := emptyDyn(obj)
	c := NewFromClients(fake.NewSimpleClientset(), dyn, nil)
	if err := c.PatchKnativeService(context.Background(), "ns", "svc", "app", "100m", "128Mi", "1", "", map[string]string{AnnLastApplied: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := dyn.Resource(ksvcGVR).Namespace("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "spec", "template", "metadata", "name"); found {
		t.Error("pinned revision name must be removed so Knative mints a new revision")
	}
	anns, _, _ := unstructured.NestedStringMap(got.Object, "spec", "template", "metadata", "annotations")
	if anns["autoscaling.knative.dev/min-scale"] != "1" || anns[AnnLastApplied] != "x" {
		t.Errorf("template annotations = %v", anns)
	}
	cs, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	res := cs[0].(map[string]any)["resources"].(map[string]any)
	if !reflect.DeepEqual(res["limits"], map[string]any{"cpu": "1"}) {
		t.Errorf("limits = %v, want only the passed cpu limit", res["limits"])
	}
	if !reflect.DeepEqual(res["requests"], map[string]any{"cpu": "100m", "memory": "128Mi"}) {
		t.Errorf("requests = %v", res["requests"])
	}
}

func TestPatchKnativeServiceErrors(t *testing.T) {
	empty := ksvcWith()
	empty.SetName("empty")
	c := NewFromClients(fake.NewSimpleClientset(), emptyDyn(empty), nil)
	if err := c.PatchKnativeService(context.Background(), "ns", "missing", "app", "1", "1Gi", "", "", nil); err == nil {
		t.Error("missing ksvc must return the Get error")
	}
	err := c.PatchKnativeService(context.Background(), "ns", "empty", "app", "1", "1Gi", "", "", nil)
	if err == nil || !strings.Contains(err.Error(), "no containers") || errors.Is(err, ErrNoTemplateContainer) {
		t.Errorf("empty template err = %v", err)
	}
}

// ResolvePodOwner resolves at admission time: a group key wins, RS -> its
// Deployment via a live Get, StatefulSet/DaemonSet directly; anything else
// (no owners, missing RS, RS without a Deployment, unknown kind) is unresolved.
func TestResolvePodOwner(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "SomethingElse", Name: "x"}, {Kind: "Deployment", Name: "web"}}}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "apps"}},
	)
	c := NewFromClients(cs, emptyDyn(), nil)
	c.SetGroupKeys([]GroupKey{{Label: "pool", Namespaces: []string{"sbx"}}})
	ref := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, Name: name}}
	}
	cases := []struct {
		name   string
		ns     string
		labels map[string]string
		owners []metav1.OwnerReference
		want   Owner
		ok     bool
	}{
		{"rs -> deployment", "apps", nil, ref("ReplicaSet", "web-abc"), Owner{Kind: "Deployment", Name: "web"}, true},
		{"statefulset", "apps", nil, ref("StatefulSet", "db"), Owner{Kind: "StatefulSet", Name: "db"}, true},
		{"daemonset", "apps", nil, ref("DaemonSet", "agent"), Owner{Kind: "DaemonSet", Name: "agent"}, true},
		{"no owners", "apps", nil, nil, Owner{}, false},
		{"missing rs", "apps", nil, ref("ReplicaSet", "gone"), Owner{}, false},
		{"rs without deployment", "apps", nil, ref("ReplicaSet", "orphan"), Owner{}, false},
		{"unknown controller", "apps", nil, ref("Job", "j"), Owner{}, false},
		{"group key", "sbx", map[string]string{"pool": "py"}, ref("Sandbox", "s1"), Owner{Kind: "group", Name: "py"}, true},
		{"group key out of scope", "apps", map[string]string{"pool": "py"}, ref("Sandbox", "s1"), Owner{}, false},
		{"knative built-in", "apps", map[string]string{lblKnativeService: "svc"}, ref("ReplicaSet", "web-abc"), Owner{Kind: "ksvc", Name: "svc"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := c.ResolvePodOwner(context.Background(), tc.ns, tc.labels, tc.owners)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %+v/%v, want %+v/%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// RecordEvent emits a Normal event on the workload with the right apiVersion;
// RecordEventFor carries the type and UID, and truncates messages to the API
// server's 1KiB cap.
func TestRecordEvents(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewFromClients(cs, emptyDyn(), nil)
	ctx := context.Background()

	events := func() []*corev1.Event {
		var out []*corev1.Event
		for _, a := range cs.Actions() {
			if ca, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "events" {
				out = append(out, ca.GetObject().(*corev1.Event))
			}
		}
		return out
	}

	c.RecordEvent(ctx, "StatefulSet", "apps", "db", "Rightsized", "cpu 1→0.5")
	evs := events()
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	e := evs[0]
	if e.Type != corev1.EventTypeNormal || e.Reason != "Rightsized" || e.Message != "cpu 1→0.5" || e.Namespace != "apps" {
		t.Errorf("event = %+v", e)
	}
	if e.InvolvedObject.Kind != "StatefulSet" || e.InvolvedObject.Name != "db" || e.InvolvedObject.APIVersion != "apps/v1" {
		t.Errorf("involved = %+v", e.InvolvedObject)
	}
	if e.Source.Component != "k8s-tailor" || e.ReportingController != "k8s-tailor.dev/engine" || e.Count != 1 || e.GenerateName != "k8s-tailor." {
		t.Errorf("event meta = %+v", e)
	}

	cases := []struct {
		kind, api string
	}{
		{"Pod", "v1"},
		{"Service", "serving.knative.dev/v1"},
		{"Deployment", "apps/v1"},
		{"DaemonSet", "apps/v1"},
		{"ksvc", "serving.knative.dev/v1"}, // the Knative Service (kind Service)
	}
	for _, tc := range cases {
		cs.ClearActions()
		c.RecordEventFor(ctx, EventRef{Kind: tc.kind, Namespace: "apps", Name: "x", UID: "uid-9"}, corev1.EventTypeWarning, "ResizeFailed", "nope")
		evs := events()
		if len(evs) != 1 {
			t.Fatalf("%s: events = %d", tc.kind, len(evs))
		}
		e := evs[0]
		if e.InvolvedObject.APIVersion != tc.api || e.InvolvedObject.UID != "uid-9" || e.Type != corev1.EventTypeWarning || e.Action != "ResizeFailed" {
			t.Errorf("%s: event = %+v", tc.kind, e)
		}
	}

	cs.ClearActions()
	c.RecordEventFor(ctx, EventRef{Kind: "Pod", Namespace: "apps", Name: "p"}, corev1.EventTypeNormal, "R", strings.Repeat("a", 2000))
	evs = events()
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	if m := evs[0].Message; len(m) != 1024 || !strings.HasSuffix(m, "...") {
		t.Errorf("message len = %d suffix %q, want 1024 ending in ...", len(m), m[len(m)-3:])
	}
}

// An event Create failure is swallowed (best-effort; never blocks actuation).
func TestRecordEventFailureIsSilent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	c := NewFromClients(cs, emptyDyn(), nil)
	c.RecordEvent(context.Background(), "Deployment", "apps", "web", "Rightsized", "m") // must not panic
	if n := len(cs.Actions()); n != 1 {
		t.Fatalf("actions = %d, want the single attempted create", n)
	}
}

// InPlaceSupport: a definite "unsupported" (no pods/resize) is cached for the
// TTL and re-checked afterwards, so a control-plane upgrade is picked up; a
// later discovery error keeps the previous definite answer.
func TestInPlaceSupportTTLAndUpgrade(t *testing.T) {
	cs := fake.NewSimpleClientset()
	fd := cs.Discovery().(*fakediscovery.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods"}}}}
	failing := false
	cs.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})
	c := NewFromClients(cs, emptyDyn(), nil)
	now := time.Unix(1_700_000_000, 0)
	c.clock = func() time.Time { return now }
	ctx := context.Background()

	if ok, known := c.InPlaceSupport(ctx); ok || !known {
		t.Fatalf("no pods/resize: ok=%v known=%v, want definite unsupported", ok, known)
	}
	// Control plane upgraded: still cached within the TTL.
	fd.Resources[0].APIResources = append(fd.Resources[0].APIResources, metav1.APIResource{Name: "pods/resize"})
	now = now.Add(inPlaceTTL - time.Second)
	if c.InPlaceSupported(ctx) {
		t.Fatal("re-checked before the TTL expired")
	}
	now = now.Add(time.Second)
	if ok, known := c.InPlaceSupport(ctx); !ok || !known {
		t.Fatalf("after TTL: ok=%v known=%v, want supported", ok, known)
	}
	// A transient error after the TTL keeps the last definite answer.
	failing = true
	now = now.Add(inPlaceTTL)
	if ok, known := c.InPlaceSupport(ctx); !ok || !known {
		t.Fatalf("transient error: ok=%v known=%v, want previous answer kept", ok, known)
	}
}

// The default clock is wall time (no injected clock).
func TestInPlaceSupportDefaultClock(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods/resize"}}}}
	c := NewFromClients(cs, emptyDyn(), nil)
	if !c.InPlaceSupported(context.Background()) {
		t.Fatal("want supported")
	}
	if d := time.Since(c.inPlaceAt); d < 0 || d > time.Minute {
		t.Fatalf("inPlaceAt not stamped with wall time: %v", c.inPlaceAt)
	}
}

func TestOOMFloorsRoundTrip(t *testing.T) {
	if m := ParseOOMFloors(""); m == nil || len(m) != 0 {
		t.Fatalf("empty -> %#v", m)
	}
	if m := ParseOOMFloors("{not json"); m == nil || len(m) != 0 {
		t.Fatalf("malformed -> %#v", m)
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	in := map[string]OOMFloor{"app": {Mem: 512 * 1024 * 1024, At: at}}
	s := FormatOOMFloors(in)
	out := ParseOOMFloors(s)
	if len(out) != 1 || out["app"].Mem != in["app"].Mem || !out["app"].At.Equal(at) {
		t.Fatalf("roundtrip %q -> %+v", s, out)
	}
}

// SetSidecarNames extends the classifier used by every listing.
func TestSetSidecarNames(t *testing.T) {
	cs := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{ctr("app"), ctr("acme-shipper")},
		}}},
	})
	c := NewFromClients(cs, emptyDyn(), nil)
	if c.Clientset() != cs {
		t.Fatal("Clientset must expose the typed client")
	}
	classOf := func() ContainerClass {
		wls, err := c.ListWorkloads(context.Background())
		if err != nil || len(wls) != 1 {
			t.Fatalf("wls=%+v err=%v", wls, err)
		}
		return classesOf(wls[0].Containers)["acme-shipper"].Class
	}
	if got := classOf(); got != ClassApp {
		t.Fatalf("before: %q, want app", got)
	}
	c.SetSidecarNames([]string{"acme-*"})
	if got := classOf(); got != ClassInjectedSidecar {
		t.Fatalf("after: %q, want injected-sidecar", got)
	}
	if !c.Classifier().IsSidecarName("acme-anything") {
		t.Fatal("Classifier() must return the configured classifier")
	}
}

// parseCPU/parseMem tolerate empty and malformed quantities (a hand-edited
// ksvc must not break the listing).
func TestParseQuantitiesTolerant(t *testing.T) {
	if parseCPU("") != 0 || parseCPU("lots") != 0 || parseCPU("1500m") != 1.5 {
		t.Fatal("parseCPU")
	}
	if parseMem("") != 0 || parseMem("huge") != 0 || parseMem("1Ki") != 1024 {
		t.Fatal("parseMem")
	}
}
