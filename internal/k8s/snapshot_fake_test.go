package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var webRS = &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
	OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}

func oomStatus(name string, finished time.Time, current bool) corev1.ContainerStatus {
	term := &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(finished)}
	if current {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: term}}
	}
	return corev1.ContainerStatus{Name: name, LastTerminationState: corev1.ContainerState{Terminated: term}}
}

// OOM observations: the hit limit, else the request (unbounded container), else
// a token 1 byte; the largest value and latest time across a workload's pods;
// current termination counts too; a SIGKILL without the OOMKilled reason and a
// regular init container never count.
func TestSnapshotOOMObservations(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	noLimit := func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Limits = nil
	}
	cs := fake.NewSimpleClientset(webRS,
		// Two pods of web disagree: keep max(512Mi, 1Gi) and the latest time.
		snapPod("web-1", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{oomStatus("app", t0.Add(time.Hour), false)}
		}),
		snapPod("web-2", func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
			p.Status.ContainerStatuses = []corev1.ContainerStatus{oomStatus("app", t0, true)}
		}),
		// Unbounded container: the request is the (lower-bound) value.
		snapPod("unbounded", func(p *corev1.Pod) {
			noLimit(p)
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db"}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{oomStatus("app", t0, false)}
		}),
		// Neither request nor limit: still a signal (1 byte).
		snapPod("bare", func(p *corev1.Pod) {
			p.OwnerReferences = nil
			p.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{oomStatus("app", t0, false)}
		}),
		// A SIGKILL that is not an OOM, and an OOMKilled regular init container.
		snapPod("agent", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
			p.Spec.InitContainers = []corev1.Container{ctr("setup")}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137}}}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{oomStatus("setup", t0, false)}
		}),
	)
	c := NewFromClients(cs, emptyDyn(), nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]OOMObservation{
		"apps/Deployment/web/app": {MemBytes: 1024 * 1024 * 1024, At: t0.Add(time.Hour)},
		"apps/StatefulSet/db/app": {MemBytes: 256 * 1024 * 1024, At: t0},
		"apps/Pod/bare/app":       {MemBytes: 1, At: t0},
	}
	if len(snap.OOM) != len(want) {
		t.Fatalf("OOM = %+v, want keys %v", snap.OOM, sortedKeys(want))
	}
	for k, w := range want {
		g := snap.OOM[k]
		if g.MemBytes != w.MemBytes || !g.At.Equal(w.At) {
			t.Errorf("%s = %+v, want %+v", k, g, w)
		}
	}
}

// An OOMKill without FinishedAt is stamped with the observation time.
func TestSnapshotOOMWithoutFinishedAt(t *testing.T) {
	cs := fake.NewSimpleClientset(webRS, snapPod("web-1", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
	}))
	before := time.Now()
	snap, err := NewFromClients(cs, emptyDyn(), nil).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := snap.OOM["apps/Deployment/web/app"]
	if o.At.Before(before) || o.At.After(time.Now()) {
		t.Fatalf("At = %v, want ~now", o.At)
	}
}

// Snapshot scope: excluded namespaces, stale cohorts and pods without a
// resolvable owner are dropped; grouped pods collapse (even under a custom
// controller); terminal pods are owned but neither running nor NotRunning; a
// deleting Pending pod is not counted as NotRunning.
func TestSnapshotScopeAndPhases(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	deleting := metav1.NewTime(base)
	cs := fake.NewSimpleClientset(webRS,
		snapPod("web-ready", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}),
		snapPod("web-unready", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				{Type: "PodResizeInProgress", Status: corev1.ConditionTrue}}
		}),
		snapPod("web-pending-noreason", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: "PodResizePending", Status: corev1.ConditionTrue}}
		}),
		snapPod("web-succeeded", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		snapPod("web-failed", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }),
		snapPod("web-unknown", func(p *corev1.Pod) { p.Status.Phase = corev1.PodUnknown }),
		snapPod("web-deleting", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.DeletionTimestamp = &deleting
			p.Finalizers = []string{"x"} // the fake tracker requires one for a set deletionTimestamp
		}),
		snapPod("job-1", func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "j"}} }),
		snapPod("orphan-rs", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "unknown-rs"}}
		}),
		snapPod("sys", func(p *corev1.Pod) { p.Namespace = "kube-system"; p.OwnerReferences = nil }),
		snapPod("sbx-new", func(p *corev1.Pod) {
			p.Labels = map[string]string{"pool": "py", "gen": "2"}
			p.CreationTimestamp = metav1.NewTime(base.Add(time.Minute))
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Sandbox", Name: "s2"}}
		}),
		snapPod("sbx-old", func(p *corev1.Pod) {
			p.Labels = map[string]string{"pool": "py", "gen": "1"}
			p.CreationTimestamp = metav1.NewTime(base)
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Sandbox", Name: "s1"}}
		}),
	)
	c := NewFromClients(cs, emptyDyn(), map[string]bool{"kube-system": true})
	c.SetGroupKeys([]GroupKey{{Label: "pool", CohortLabel: "gen", CohortSelect: "latest"}})
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	web := Owner{Kind: "Deployment", Name: "web"}
	wantOwn := map[string]Owner{
		"apps/web-ready": web, "apps/web-unready": web, "apps/web-pending-noreason": web,
		"apps/web-succeeded": web, "apps/web-failed": web, "apps/web-unknown": web, "apps/web-deleting": web,
		"apps/sbx-new": {Kind: "group", Name: "py"},
	}
	if len(snap.Ownership) != len(wantOwn) {
		t.Fatalf("ownership = %v, want %v", sortedKeys(snap.Ownership), sortedKeys(wantOwn))
	}
	for k, o := range wantOwn {
		if snap.Ownership[k] != o {
			t.Errorf("%s -> %+v, want %+v", k, snap.Ownership[k], o)
		}
	}
	running := map[string]RunningPod{}
	for _, p := range snap.Running["apps"] {
		running[p.Name] = p
	}
	if len(running) != 4 {
		t.Fatalf("running = %v", sortedKeys(running))
	}
	if !running["web-ready"].Ready || running["web-unready"].Ready {
		t.Errorf("Ready: ready=%v unready=%v", running["web-ready"].Ready, running["web-unready"].Ready)
	}
	if s := running["web-unready"].ResizeState; s != "InProgress" {
		t.Errorf("in-progress resize state = %q", s)
	}
	if s := running["web-pending-noreason"].ResizeState; s != "Pending" {
		t.Errorf("pending (no reason) resize state = %q", s)
	}
	if running["sbx-new"].Owner != (Owner{Kind: "group", Name: "py"}) {
		t.Errorf("group pod owner = %+v", running["sbx-new"].Owner)
	}
	if len(snap.Running["kube-system"]) != 0 {
		t.Error("excluded namespace leaked into Running")
	}
	// Only web-unknown: terminal pods don't count, the deleting one is replaced.
	if len(snap.NotRunning) != 1 || snap.NotRunning["apps/Deployment/web"] != 1 {
		t.Errorf("NotRunning = %v, want web:1", snap.NotRunning)
	}
}

// ResizePolicy RestartContainer marks a resize as disruptive per resource.
func TestSnapshotResizePolicy(t *testing.T) {
	cs := fake.NewSimpleClientset(webRS, snapPod("web-1", func(p *corev1.Pod) {
		p.Spec.Containers[0].ResizePolicy = []corev1.ContainerResizePolicy{
			{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer},
			{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
		}
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "worker",
			ResizePolicy: []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.RestartContainer}}})
	}))
	snap, err := NewFromClients(cs, emptyDyn(), nil).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := classesOf(snap.Running["apps"][0].Containers)
	if !got["app"].MemRestartOnResize || got["app"].CPURestartOnResize {
		t.Errorf("app = %+v, want mem restart only", got["app"])
	}
	if got["worker"].MemRestartOnResize || !got["worker"].CPURestartOnResize {
		t.Errorf("worker = %+v, want cpu restart only", got["worker"])
	}
}

func TestSnapshotListErrors(t *testing.T) {
	for _, res := range []string{"pods", "replicasets"} {
		t.Run(res, func(t *testing.T) {
			cs := fake.NewSimpleClientset(webRS, snapPod("web-1", nil))
			cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("boom")
			})
			if snap, err := NewFromClients(cs, emptyDyn(), nil).Snapshot(context.Background()); err == nil {
				t.Fatalf("want error, got %+v", snap)
			}
		})
	}
}
