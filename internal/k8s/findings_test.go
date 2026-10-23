package k8s

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A StatefulSet or DaemonSet folded by a TailorGroup is represented by the group
// unit only — exactly like a grouped Deployment. Listing it as well produced a
// second workload for the same pods (with no usage of its own, since Snapshot
// attributes every member pod to the group).
func TestListWorkloadsGroupedStatefulSetAndDaemonSetNotDuplicated(t *testing.T) {
	site := map[string]string{"site": "portal"}
	tmpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: site},
		Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("app")}}}
	cs := fake.NewSimpleClientset(
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "apps", Labels: site},
			Spec: appsv1.StatefulSetSpec{Replicas: int32p(1), Template: tmpl}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "apps", Labels: site},
			Spec: appsv1.DaemonSetSpec{Template: tmpl}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1}},
		// Not grouped: still listed as itself.
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "apps"},
			Spec: appsv1.StatefulSetSpec{Replicas: int32p(1), Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("app")}}}}},
		snapPod("db-0", func(p *corev1.Pod) {
			p.Labels = site
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db"}}
		}),
		snapPod("agent-x", func(p *corev1.Pod) {
			p.Labels = site
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
		}),
	)
	c := NewFromClients(cs, emptyDyn(), nil)
	c.SetGroupKeys([]GroupKey{{Label: "site", Source: "sites"}})
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := workloadsByKey(wls)
	want := []string{"apps/StatefulSet/solo", "apps/group/portal"}
	if len(got) != len(want) {
		t.Fatalf("workloads = %v, want %v", sortedKeys(got), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s (got %v)", k, sortedKeys(got))
		}
	}
	// The unit remembers the TailorGroup CR that folded it (OOM floors persist there).
	if u := got["apps/group/portal"]; u.GroupSource != "sites" {
		t.Errorf("group unit GroupSource = %q, want sites", u.GroupSource)
	}
	if u := got["apps/group/portal"]; u.Replicas != 2 {
		t.Errorf("group replicas = %d, want 2 (db-0 + agent-x)", u.Replicas)
	}
}

// The OOMKilled helper must see a native sidecar's kill (reported in
// initContainerStatuses), but never a regular init container's.
func TestOOMKilledNativeSidecar(t *testing.T) {
	cs := fake.NewSimpleClientset(webRS,
		snapPod("web-1", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{ctr("setup"), nativeCtr("istio-proxy")}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{Name: "setup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
				{Name: "istio-proxy", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
			}
		}))
	got, err := NewFromClients(cs, emptyDyn(), nil).OOMKilled(context.Background(), "apps")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["Deployment/web/istio-proxy"] {
		t.Fatalf("oom = %v, want only Deployment/web/istio-proxy", got)
	}
}

// Workload-level Events must point at an object that exists: a ksvc owner is the
// Knative Service (serving.knative.dev/v1, kind Service); a TailorGroup unit has
// no object at all, so no Event is emitted for it (the pod still gets its own).
func TestRecordEventSyntheticOwners(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewFromClients(cs, emptyDyn(), nil)
	ctx := context.Background()
	created := func() []*corev1.Event {
		var out []*corev1.Event
		for _, a := range cs.Actions() {
			if ca, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "events" {
				out = append(out, ca.GetObject().(*corev1.Event))
			}
		}
		return out
	}

	c.RecordEvent(ctx, "ksvc", "apps", "portal", "Rightsized", "m")
	evs := created()
	if len(evs) != 1 {
		t.Fatalf("ksvc: events = %d, want 1", len(evs))
	}
	if o := evs[0].InvolvedObject; o.Kind != "Service" || o.APIVersion != "serving.knative.dev/v1" || o.Name != "portal" {
		t.Fatalf("ksvc involvedObject = %+v, want serving.knative.dev/v1 Service portal", o)
	}

	for _, kind := range []string{"group", "Unknown", ""} {
		cs.ClearActions()
		c.RecordEventFor(ctx, EventRef{Kind: kind, Namespace: "apps", Name: "py"}, corev1.EventTypeNormal, "Rightsized", "m")
		if evs := created(); len(evs) != 0 {
			t.Fatalf("%q: emitted %+v, want no event (no such object)", kind, evs[0].InvolvedObject)
		}
	}
}

// Truncation to the API server's 1 KiB cap must not split a multi-byte rune.
func TestRecordEventTruncatesOnRuneBoundary(t *testing.T) {
	for _, r := range []string{"é", "→", "😀"} { // 2, 3 and 4 bytes
		cs := fake.NewSimpleClientset()
		c := NewFromClients(cs, emptyDyn(), nil)
		c.RecordEventFor(context.Background(), EventRef{Kind: "Pod", Namespace: "apps", Name: "p"},
			corev1.EventTypeNormal, "R", strings.Repeat(r, 1000)) // 1021 is never a rune boundary here
		ev := cs.Actions()[0].(k8stesting.CreateAction).GetObject().(*corev1.Event)
		m := ev.Message
		if len(m) > 1024 || len(m) < 1024-utf8.UTFMax || !strings.HasSuffix(m, "...") {
			t.Errorf("%q: len = %d, suffix ok %v", r, len(m), strings.HasSuffix(m, "..."))
		}
		if !utf8.ValidString(m) {
			t.Errorf("%q: truncated message is not valid UTF-8 (split rune): %q", r, m[len(m)-8:])
		}
	}
	// Short messages (even multi-byte) are left untouched.
	cs := fake.NewSimpleClientset()
	c := NewFromClients(cs, emptyDyn(), nil)
	short := strings.Repeat("é", 512) // exactly 1024 bytes
	c.RecordEventFor(context.Background(), EventRef{Kind: "Pod", Namespace: "apps", Name: "p"}, corev1.EventTypeNormal, "R", short)
	if m := cs.Actions()[0].(k8stesting.CreateAction).GetObject().(*corev1.Event).Message; m != short {
		t.Errorf("a 1024-byte message was altered (len %d)", len(m))
	}
}

// Ownership honors the client's namespace exclusion like ListWorkloads and
// Snapshot do: an excluded namespace's pods are never attributed to a workload.
func TestOwnershipHonorsExcludedNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "kube-system",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}},
		snapPod("web-1", func(p *corev1.Pod) {
			p.Namespace = "kube-system"
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
		}),
	)
	c := NewFromClients(cs, emptyDyn(), map[string]bool{"kube-system": true})
	own, err := c.Ownership(context.Background(), "kube-system")
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 0 {
		t.Fatalf("ownership of an excluded namespace = %v, want empty", own)
	}
	if pods, err := c.ListRunningPods(context.Background(), "kube-system"); err != nil || len(pods) != 0 {
		t.Fatalf("running pods of an excluded namespace = %v (err %v), want none", pods, err)
	}
}
