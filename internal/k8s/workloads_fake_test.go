package k8s

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func int32p(v int32) *int32 { return &v }

// emptyDyn is a dynamic fake that knows how to list ksvcs (empty).
func emptyDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}, objs...)
}

func fakeDeployment(ns, name string, replicas *int32, tmplLabels map[string]string, mut func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{Replicas: replicas, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: tmplLabels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{ctr("app")}},
		}},
	}
	if mut != nil {
		mut(d)
	}
	return d
}

func workloadsByKey(wls []Workload) map[string]Workload {
	out := map[string]Workload{}
	for _, w := range wls {
		out[w.Namespace+"/"+w.Kind+"/"+w.Name] = w
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ListWorkloads emits one row per controller and per unmanaged pod, honors the
// excluded namespaces, defaults replicas, and drops pods owned by a controller.
func TestListWorkloadsKindsReplicasAndExclusion(t *testing.T) {
	cs := fake.NewSimpleClientset(
		fakeDeployment("apps", "web", int32p(3), map[string]string{"app": "web"}, nil),
		fakeDeployment("apps", "noreplicas", nil, nil, nil),
		fakeDeployment("kube-system", "coredns", int32p(2), nil, nil),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "apps"},
			Spec: appsv1.StatefulSetSpec{Replicas: int32p(2), Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("pg")}}}}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "nil-replicas", Namespace: "apps"},
			Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("x")}}}}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "hidden", Namespace: "kube-system"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "apps"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("agent")}}}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 5}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unscheduled", Namespace: "apps"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "hidden-ds", Namespace: "kube-system"}},
		// Unmanaged pod: its own workload, init containers filtered out (sizable).
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "apps", Labels: map[string]string{"run": "debug"}},
			Spec: corev1.PodSpec{InitContainers: []corev1.Container{ctr("setup")}, Containers: []corev1.Container{ctr("shell")}}},
		// Owned pod: represented by its controller, never its own row.
		snapPod("web-abc-1", nil),
		// Unmanaged pod in an excluded namespace.
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lost", Namespace: "kube-system"}},
	)
	c := NewFromClients(cs, emptyDyn(), map[string]bool{"kube-system": true})
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := workloadsByKey(wls)
	want := map[string]int{
		"apps/Deployment/web":           3,
		"apps/Deployment/noreplicas":    1,
		"apps/StatefulSet/db":           2,
		"apps/StatefulSet/nil-replicas": 1,
		"apps/DaemonSet/agent":          5,
		"apps/DaemonSet/unscheduled":    1,
		"apps/Pod/debug":                1,
	}
	if len(got) != len(want) {
		t.Fatalf("workloads = %v, want %v", sortedKeys(got), sortedKeys(want))
	}
	for k, r := range want {
		w, ok := got[k]
		if !ok {
			t.Errorf("missing %s", k)
			continue
		}
		if w.Replicas != r {
			t.Errorf("%s replicas = %d, want %d", k, w.Replicas, r)
		}
		if w.Autoscaler.Kind != "" || w.PDBProtected || w.Knative.Service != "" {
			t.Errorf("%s: unexpected meta %+v", k, w)
		}
	}
	dbg := got["apps/Pod/debug"]
	if len(dbg.Containers) != 1 || dbg.Containers[0].Name != "shell" || dbg.Labels["run"] != "debug" {
		t.Errorf("unmanaged pod containers = %+v (init must be filtered)", dbg.Containers)
	}
	if db := got["apps/StatefulSet/db"]; len(db.Containers) != 1 || db.Containers[0].Name != "pg" || !db.Containers[0].InTemplate {
		t.Errorf("sts containers = %+v", db.Containers)
	}
}

// HPA and PDB coverage is matched per workload: HPA by scaleTargetRef
// (namespace/kind/name), PDB by the pod-template labels. Excluded namespaces,
// nil selectors and empty selectors never match.
func TestListWorkloadsHPAAndPDB(t *testing.T) {
	cs := fake.NewSimpleClientset(
		fakeDeployment("apps", "web", int32p(2), map[string]string{"app": "web", "tier": "fe"}, nil),
		fakeDeployment("apps", "api", int32p(2), map[string]string{"app": "api"}, nil),
		fakeDeployment("apps", "worker", int32p(1), map[string]string{"app": "worker"}, nil),
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "web-hpa", Namespace: "apps"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
				MinReplicas:    int32p(2), MaxReplicas: 10,
				Metrics: []autoscalingv2.MetricSpec{
					{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU}},
					{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory}},
					{Type: autoscalingv2.PodsMetricSourceType}, // custom metric: no resource
				},
			}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "api-hpa", Namespace: "apps"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"},
				MaxReplicas:    4, // MinReplicas unset -> 1
				Metrics: []autoscalingv2.MetricSpec{
					{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory}},
				},
			}},
		// An HPA in an excluded namespace with an identical target is ignored.
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: "kube-system"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "worker"}, MaxReplicas: 3}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "web-pdb", Namespace: "apps"},
			Spec:   policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "fe"}}},
			Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 1}},
		// nil selector and an empty selector must never cover anything.
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "nil-sel", Namespace: "apps"}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "empty-sel", Namespace: "apps"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{}}},
		// An invalid selector is skipped, not fatal.
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "bad-sel", Namespace: "apps"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: "Bogus", Values: []string{"api"}}}}}},
		// Same labels, other namespace: does not apply to apps/api.
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "other-ns", Namespace: "other"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "excluded", Namespace: "kube-system"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}}}},
	)
	c := NewFromClients(cs, emptyDyn(), map[string]bool{"kube-system": true})
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := workloadsByKey(wls)

	web := got["apps/Deployment/web"]
	wantWeb := Autoscaler{Kind: "HPA", Name: "web-hpa", ManagesCPU: true, ManagesMem: true, Min: 2, Max: 10}
	if web.Autoscaler != wantWeb {
		t.Errorf("web autoscaler = %+v, want %+v", web.Autoscaler, wantWeb)
	}
	if web.PDBName != "web-pdb" || !web.PDBProtected || web.PDBAllowed != 1 {
		t.Errorf("web pdb = %q/%v/%d", web.PDBName, web.PDBProtected, web.PDBAllowed)
	}

	api := got["apps/Deployment/api"]
	wantAPI := Autoscaler{Kind: "HPA", Name: "api-hpa", ManagesMem: true, Min: 1, Max: 4}
	if api.Autoscaler != wantAPI {
		t.Errorf("api autoscaler = %+v, want %+v", api.Autoscaler, wantAPI)
	}
	if api.PDBProtected {
		t.Errorf("api must not be covered (only nil/empty/invalid/other-ns PDBs): %q", api.PDBName)
	}

	worker := got["apps/Deployment/worker"]
	if worker.Autoscaler.Kind != "" || worker.PDBProtected {
		t.Errorf("worker picked up excluded-namespace HPA/PDB: %+v", worker)
	}
}

// HPA/PDB lookups are advisory: a list error just means "none", never a failure.
func TestListWorkloadsAdvisoryListErrors(t *testing.T) {
	cs := fake.NewSimpleClientset(fakeDeployment("apps", "web", int32p(1), map[string]string{"app": "web"}, nil))
	for _, res := range []string{"horizontalpodautoscalers", "poddisruptionbudgets"} {
		cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
	}
	c := NewFromClients(cs, emptyDyn(), nil)
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatalf("advisory list errors must not fail ListWorkloads: %v", err)
	}
	if len(wls) != 1 || wls[0].Autoscaler.Kind != "" || wls[0].PDBProtected {
		t.Fatalf("workloads = %+v", wls)
	}
}

// A list error on a primary resource is returned (no partial inventory).
func TestListWorkloadsPrimaryListErrors(t *testing.T) {
	for _, res := range []string{"deployments", "statefulsets", "daemonsets", "pods"} {
		t.Run(res, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("boom")
			})
			c := NewFromClients(cs, emptyDyn(), nil)
			if wls, err := c.ListWorkloads(context.Background()); err == nil {
				t.Fatalf("want error, got %+v", wls)
			}
		})
	}
}

// Knative: the per-revision Deployment is folded into the ksvc row (one row per
// Service, read from the dynamic client); the ksvc gets a KPA autoscaler unless
// an explicit HPA targets it. Knative pods are never listed as a group unit.
func TestListWorkloadsKnative(t *testing.T) {
	svc := ksvcWithResources(map[string]any{
		"requests": map[string]any{"cpu": "250m", "memory": "256Mi"},
	})
	svc.SetLabels(map[string]string{"team": "a"})
	noTemplate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.knative.dev/v1", "kind": "Service",
		"metadata": map[string]any{"namespace": "ns", "name": "broken"},
	}}
	excluded := ksvcWith(map[string]any{"name": "app", "image": "x"})
	excluded.SetNamespace("kube-system")
	excluded.SetName("sys")
	revLabels := map[string]string{lblKnativeService: "svc", lblKnativeRevision: "svc-00002"}
	cs := fake.NewSimpleClientset(
		fakeDeployment("ns", "svc-00002-deployment", int32p(1), revLabels, func(d *appsv1.Deployment) { d.Labels = revLabels }),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "svc-00002-pod", Namespace: "ns", Labels: revLabels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("user-container"), ctr("queue-proxy")}}},
	)
	c := NewFromClients(cs, emptyDyn(svc, noTemplate, excluded), map[string]bool{"kube-system": true})
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("workloads = %v, want only the ksvc row", sortedKeys(workloadsByKey(wls)))
	}
	w := wls[0]
	if w.Kind != "ksvc" || w.Name != "svc" || w.Namespace != "ns" || w.Replicas != 1 || w.Labels["team"] != "a" {
		t.Fatalf("ksvc row = %+v", w)
	}
	if w.Knative.Service != "svc" {
		t.Errorf("Knative ref = %+v", w.Knative)
	}
	if w.Autoscaler.Kind != "KPA" || w.Autoscaler.ManagesCPU || w.Autoscaler.ManagesMem {
		t.Errorf("autoscaler = %+v, want KPA that manages no resource", w.Autoscaler)
	}
	if len(w.Containers) != 1 || w.Containers[0].Name != "user-container" || w.Containers[0].CPURequest != 0.25 || w.Containers[0].MemRequest != 256*1024*1024 {
		t.Errorf("containers = %+v", w.Containers)
	}
}

// A plain Deployment carrying Knative labels on the pod template only (not on
// the object) is not grouped, but still reports its Knative ref and a KPA.
func TestListWorkloadsKnativeFromTemplateLabels(t *testing.T) {
	c := NewFromClients(fake.NewSimpleClientset(
		fakeDeployment("ns", "legacy", int32p(1), map[string]string{lblKnativeService: "svc", lblKnativeRevision: "svc-1"}, nil),
	), emptyDyn(), nil)
	// Scope the built-in Knative group to a namespace that is not "ns" so the
	// Deployment itself is listed.
	c.SetGroupKeys([]GroupKey{{Label: lblKnativeService, Namespaces: []string{"elsewhere"}}})
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 || wls[0].Kind != "Deployment" {
		t.Fatalf("workloads = %+v", wls)
	}
	if wls[0].Knative != (KnativeRef{Service: "svc", Revision: "svc-1"}) {
		t.Errorf("knative = %+v", wls[0].Knative)
	}
	if wls[0].Autoscaler != (Autoscaler{Kind: "KPA", Name: "svc-1"}) {
		t.Errorf("autoscaler = %+v", wls[0].Autoscaler)
	}
}

// A TailorGroup collapses pods of a custom controller into ONE synthetic unit
// whose Replicas counts the members; a stale cohort is dropped, and a PDB that
// selects the pooled pods is honored.
func TestListWorkloadsGroupUnit(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	member := func(name, version string, created time.Time) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "sbx",
				Labels:            map[string]string{"pool": "py", "version": version},
				CreationTimestamp: metav1.NewTime(created),
				OwnerReferences:   []metav1.OwnerReference{{Kind: "Sandbox", Name: name}}},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("fetch")},
				Containers:     []corev1.Container{ctr("runtime")},
			},
		}
		return p
	}
	cs := fake.NewSimpleClientset(
		member("a", "v2", base.Add(time.Minute)),
		member("b", "v2", base.Add(2*time.Minute)),
		member("old", "v1", base), // superseded cohort
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "pool-pdb", Namespace: "sbx"},
			Spec:   policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "py"}}},
			Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0}},
	)
	c := NewFromClients(cs, emptyDyn(), nil)
	c.SetGroupKeys([]GroupKey{{Label: "pool", CohortLabel: "version", CohortSelect: "latest"}})
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("workloads = %v, want one group unit", sortedKeys(workloadsByKey(wls)))
	}
	u := wls[0]
	if u.Kind != "group" || u.Name != "py" || u.Namespace != "sbx" {
		t.Fatalf("unit = %+v", u)
	}
	if u.Replicas != 2 {
		t.Errorf("replicas = %d, want 2 (stale cohort excluded)", u.Replicas)
	}
	if len(u.Containers) != 1 || u.Containers[0].Name != "runtime" {
		t.Errorf("containers = %+v, want the sizable runtime only", u.Containers)
	}
	if !u.PDBProtected || u.PDBName != "pool-pdb" || u.PDBAllowed != 0 {
		t.Errorf("pdb = %q/%v/%d", u.PDBName, u.PDBProtected, u.PDBAllowed)
	}
}

// GroupMembers lists the raw workloads a rule folds, with the unit each belongs
// to, for both a label key and the "namespace" sentinel.
func TestGroupMembers(t *testing.T) {
	cs := fake.NewSimpleClientset(
		fakeDeployment("site-a", "web", int32p(3), nil, func(d *appsv1.Deployment) { d.Labels = map[string]string{"site": "alpha"} }),
		fakeDeployment("site-a", "nil-rep", nil, nil, nil),
		fakeDeployment("kube-system", "dns", int32p(2), nil, func(d *appsv1.Deployment) { d.Labels = map[string]string{"site": "alpha"} }),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "site-a", Labels: map[string]string{"site": "alpha"}},
			Spec: appsv1.StatefulSetSpec{Replicas: int32p(2)}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "site-b"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sys", Namespace: "kube-system"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "site-b", Labels: map[string]string{"site": "beta"}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 4}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "sys-ds", Namespace: "kube-system"}},
	)
	c := NewFromClients(cs, emptyDyn(), map[string]bool{"kube-system": true})

	byLabel := c.GroupMembers(context.Background(), "site", nil)
	gotLabel := map[string]WorkloadRef{}
	for _, r := range byLabel {
		gotLabel[r.Namespace+"/"+r.Kind+"/"+r.Name] = r
	}
	wantLabel := map[string]WorkloadRef{
		"site-a/Deployment/web":  {Namespace: "site-a", Kind: "Deployment", Name: "web", Replicas: 3, Unit: "alpha"},
		"site-a/StatefulSet/db":  {Namespace: "site-a", Kind: "StatefulSet", Name: "db", Replicas: 2, Unit: "alpha"},
		"site-b/DaemonSet/agent": {Namespace: "site-b", Kind: "DaemonSet", Name: "agent", Replicas: 4, Unit: "beta"},
	}
	if len(gotLabel) != len(wantLabel) {
		t.Fatalf("by label = %v, want %v", sortedKeys(gotLabel), sortedKeys(wantLabel))
	}
	for k, w := range wantLabel {
		if gotLabel[k] != w {
			t.Errorf("%s = %+v, want %+v", k, gotLabel[k], w)
		}
	}

	byNS := c.GroupMembers(context.Background(), "namespace", []string{"site-*"})
	gotNS := map[string]WorkloadRef{}
	for _, r := range byNS {
		gotNS[r.Namespace+"/"+r.Kind+"/"+r.Name] = r
	}
	wantNS := map[string]WorkloadRef{
		"site-a/Deployment/web":     {Namespace: "site-a", Kind: "Deployment", Name: "web", Replicas: 3, Unit: "site-a"},
		"site-a/Deployment/nil-rep": {Namespace: "site-a", Kind: "Deployment", Name: "nil-rep", Replicas: 1, Unit: "site-a"},
		"site-a/StatefulSet/db":     {Namespace: "site-a", Kind: "StatefulSet", Name: "db", Replicas: 2, Unit: "site-a"},
		"site-b/StatefulSet/cache":  {Namespace: "site-b", Kind: "StatefulSet", Name: "cache", Replicas: 1, Unit: "site-b"},
		"site-b/DaemonSet/agent":    {Namespace: "site-b", Kind: "DaemonSet", Name: "agent", Replicas: 4, Unit: "site-b"},
	}
	if len(gotNS) != len(wantNS) {
		t.Fatalf("by namespace = %v, want %v", sortedKeys(gotNS), sortedKeys(wantNS))
	}
	for k, w := range wantNS {
		if gotNS[k] != w {
			t.Errorf("%s = %+v, want %+v", k, gotNS[k], w)
		}
	}

	if got := c.GroupMembers(context.Background(), "namespace", []string{"nomatch"}); len(got) != 0 {
		t.Errorf("out-of-scope namespaces = %+v, want none", got)
	}
	// Never nil: the API serializes it as [] rather than null.
	if got := c.GroupMembers(context.Background(), "absent-label", nil); got == nil || len(got) != 0 {
		t.Errorf("no members = %#v, want empty non-nil slice", got)
	}
}

func ownershipFixture() *fake.Clientset {
	return fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}},
		// An orphan ReplicaSet (no Deployment owner).
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "orphan-rs", Namespace: "apps"}},
		snapPod("web-1", nil),
		snapPod("db-0", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db"}}
		}),
		snapPod("agent-x", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
		}),
		snapPod("bare", func(p *corev1.Pod) { p.OwnerReferences = nil }),
		snapPod("orphan-1", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "orphan-rs"}}
		}),
		snapPod("job-1", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "migrate"}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
		}),
		snapPod("grouped", func(p *corev1.Pod) {
			p.Labels = map[string]string{"pool": "py"}
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Sandbox", Name: "s1"}}
		}),
		snapPod("web-pending", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
		snapPod("web-oom", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: "app", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
				{Name: "sigkill", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137}}},
			}
		}),
		// Another namespace: never seen by a namespaced call.
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"}},
	)
}

// Ownership maps every pod of a namespace to its workload: RS->Deployment,
// StatefulSet, DaemonSet, bare pod -> itself, grouped pod -> its group unit;
// unknown controllers and orphan ReplicaSets are left out.
func TestOwnership(t *testing.T) {
	c := NewFromClients(ownershipFixture(), emptyDyn(), nil)
	c.SetGroupKeys([]GroupKey{{Label: "pool"}})
	got, err := c.Ownership(context.Background(), "apps")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Owner{
		"web-1":       {Kind: "Deployment", Name: "web"},
		"web-pending": {Kind: "Deployment", Name: "web"},
		"web-oom":     {Kind: "Deployment", Name: "web"},
		"db-0":        {Kind: "StatefulSet", Name: "db"},
		"agent-x":     {Kind: "DaemonSet", Name: "agent"},
		"bare":        {Kind: "Pod", Name: "bare"},
		"grouped":     {Kind: "group", Name: "py"},
	}
	if len(got) != len(want) {
		t.Fatalf("ownership = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %+v, want %+v", k, got[k], v)
		}
	}
}

func TestOwnershipListErrors(t *testing.T) {
	for _, res := range []string{"pods", "replicasets"} {
		t.Run(res, func(t *testing.T) {
			cs := ownershipFixture()
			cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("boom")
			})
			c := NewFromClients(cs, emptyDyn(), nil)
			if _, err := c.Ownership(context.Background(), "apps"); err == nil {
				t.Fatal("want error")
			}
			if _, err := c.ListRunningPods(context.Background(), "apps"); err == nil {
				t.Fatal("ListRunningPods: want error")
			}
			if _, err := c.OOMKilled(context.Background(), "apps"); err == nil {
				t.Fatal("OOMKilled: want error")
			}
		})
	}
}

// ListRunningPods keeps only Running pods with a resolvable owner.
func TestListRunningPods(t *testing.T) {
	c := NewFromClients(ownershipFixture(), emptyDyn(), nil)
	pods, err := c.ListRunningPods(context.Background(), "apps")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RunningPod{}
	for _, p := range pods {
		got[p.Name] = p
	}
	// web-pending: not Running. orphan-1/job-1: no resolvable owner. grouped:
	// no "pool" group key here, so its custom (Sandbox) owner is unresolvable.
	want := map[string]Owner{
		"web-1":   {Kind: "Deployment", Name: "web"},
		"web-oom": {Kind: "Deployment", Name: "web"},
		"db-0":    {Kind: "StatefulSet", Name: "db"},
		"agent-x": {Kind: "DaemonSet", Name: "agent"},
		"bare":    {Kind: "Pod", Name: "bare"},
	}
	if len(got) != len(want) {
		t.Fatalf("running = %v, want %v", sortedKeys(got), sortedKeys(want))
	}
	for k, o := range want {
		if got[k].Owner != o {
			t.Errorf("%s owner = %+v, want %+v", k, got[k].Owner, o)
		}
		if len(got[k].Containers) != 1 || got[k].Containers[0].MemLimit != 512*1024*1024 {
			t.Errorf("%s containers = %+v", k, got[k].Containers)
		}
	}
}

// OOMKilled keys containers by "kind/workload/container" from the current or
// the last termination, and only on the explicit OOMKilled reason (a bare
// exit 137 is any SIGKILL). Pods without a resolvable owner are skipped.
func TestOOMKilled(t *testing.T) {
	c := NewFromClients(ownershipFixture(), emptyDyn(), nil)
	got, err := c.OOMKilled(context.Background(), "apps")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"DaemonSet/agent/app": true, "Deployment/web/app": true}
	if len(got) != len(want) {
		t.Fatalf("oom = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %s", k)
		}
	}
}
