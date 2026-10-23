package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func snapPod(name string, mut func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	if mut != nil {
		mut(p)
	}
	return p
}

func TestSnapshotRunningPodState(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-time.Minute))
	cs := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}},
		snapPod("plain", nil),
		snapPod("pending", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Infeasible"}}
		}),
		snapPod("pending-false", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: "PodResizePending", Status: corev1.ConditionFalse, Reason: "Deferred"}}
		}),
		snapPod("legacy", func(p *corev1.Pod) { p.Status.Resize = corev1.PodResizeStatusDeferred }),
		snapPod("guaranteed", func(p *corev1.Pod) { p.Status.QOSClass = corev1.PodQOSGuaranteed }),
		snapPod("oom", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: finished}}}}
		}),
		snapPod("not-running", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
	)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RunningPod{}
	for _, p := range snap.Running["apps"] {
		got[p.Name] = p
	}
	cases := []struct {
		pod    string
		resize string
		qos    corev1.PodQOSClass
	}{
		{"plain", "", corev1.PodQOSBurstable},
		{"pending", "Pending/Infeasible", corev1.PodQOSBurstable},
		{"pending-false", "", corev1.PodQOSBurstable},
		{"legacy", "Deferred", corev1.PodQOSBurstable},
		{"guaranteed", "", corev1.PodQOSGuaranteed},
	}
	for _, c := range cases {
		p, ok := got[c.pod]
		if !ok {
			t.Errorf("%s: missing from Running", c.pod)
			continue
		}
		if p.ResizeState != c.resize || p.QOSClass != c.qos || p.Owner != (Owner{Kind: "Deployment", Name: "web"}) {
			t.Errorf("%s: %+v", c.pod, p)
		}
	}
	if _, ok := got["not-running"]; ok {
		t.Error("non-running pod listed as running")
	}
	if _, ok := snap.Ownership["apps/not-running"]; !ok {
		t.Error("ownership must include non-running pods")
	}
	if n := snap.NotRunning["apps/Deployment/web"]; n != 1 {
		t.Errorf("NotRunning[web] = %d, want 1 (the Pending replica)", n)
	}
	o, ok := snap.OOM["apps/Deployment/web/app"]
	if !ok || o.MemBytes != 512*1024*1024 || !o.At.Equal(finished.Time) {
		t.Errorf("OOM observation = %+v ok=%v", o, ok)
	}
}

// The kubelet-effected resources (status.containerStatuses[].resources) win over
// the spec: with a resize Pending/Infeasible the spec already shows the DESIRED
// value, so waste/"Applied" must read what the container actually runs with.
func TestSnapshotPrefersStatusResources(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}},
		snapPod("resized", func(p *corev1.Pod) {
			p.UID = "uid-1"
			p.Annotations = map[string]string{AnnLastApplied: `{"app":"2026-01-02T03:04:05Z"}`}
			p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			}}}
		}),
		snapPod("no-status", nil),
	)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RunningPod{}
	for _, p := range snap.Running["apps"] {
		got[p.Name] = p
	}
	r := got["resized"].Containers[0]
	if r.CPURequest != 1 || r.MemRequest != 1024*1024*1024 || r.MemLimit != 2*1024*1024*1024 {
		t.Errorf("status resources not preferred: %+v", r)
	}
	if r.SpecCPURequest != 0.1 || r.SpecMemRequest != 256*1024*1024 {
		t.Errorf("spec requests not kept: %+v", r)
	}
	if got["resized"].UID != "uid-1" {
		t.Errorf("uid = %q", got["resized"].UID)
	}
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if la := got["resized"].LastApplied["app"]; !la.Equal(want) {
		t.Errorf("LastApplied = %v want %v", la, want)
	}
	n := got["no-status"].Containers[0]
	if n.MemRequest != 256*1024*1024 || n.MemLimit != 512*1024*1024 {
		t.Errorf("spec fallback wrong: %+v", n)
	}
}

func TestParseLastApplied(t *testing.T) {
	if m := ParseLastApplied(""); len(m) != 0 {
		t.Fatalf("empty -> %v", m)
	}
	if m := ParseLastApplied("garbage"); len(m) != 0 {
		t.Fatalf("garbage -> %v", m)
	}
	m := ParseLastApplied(`{"a":"2026-01-02T03:04:05Z","b":"nope"}`)
	if len(m) != 1 || m["a"].IsZero() {
		t.Fatalf("got %v", m)
	}
	s := FormatLastApplied(map[string]time.Time{"a": m["a"]})
	if back := ParseLastApplied(s); !back["a"].Equal(m["a"]) {
		t.Fatalf("roundtrip %q -> %v", s, back)
	}
}

// Pods carrying the webhook's admission marker (any phase) are listed in
// Snapshot.Admitted with their spec requests; a malformed marker is ignored.
func TestSnapshotAdmitted(t *testing.T) {
	marked := snapPod("marked", func(p *corev1.Pod) {
		p.UID = "u1"
		p.Status.Phase = corev1.PodPending
		p.Annotations = map[string]string{AnnAdmissionSized: `{"app":{"fromCpu":1,"toCpu":0.2,"fromMem":1,"toMem":268435456}}`}
	})
	bad := snapPod("bad", func(p *corev1.Pod) { p.Annotations = map[string]string{AnnAdmissionSized: `nope`} })
	plain := snapPod("plain", nil)
	cs := fake.NewSimpleClientset(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}, marked, bad, plain)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Admitted) != 1 {
		t.Fatalf("admitted = %+v, want only the well-formed marker", snap.Admitted)
	}
	a := snap.Admitted[0]
	if a.UID != "u1" || a.Owner.Name != "web" || a.Decisions["app"].ToCPU != 0.2 || len(a.Containers) != 1 || a.Containers[0].MemRequest != 256*1024*1024 {
		t.Fatalf("admitted pod = %+v", a)
	}
}
