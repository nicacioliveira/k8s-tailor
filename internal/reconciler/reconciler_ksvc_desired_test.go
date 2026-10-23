package reconciler

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// A Knative Service has no replica count of its own; its revisions' Deployments
// carry it. The disruptive rolling must not restart a Ready pod while the
// autoscaler wants more replicas than are Ready (a scale-up the pod snapshot has
// not seen yet).
func TestKsvcRollingUsesRevisionDeploymentCount(t *testing.T) {
	ksvcPods := func() []*corev1.Pod {
		var out []*corev1.Pod
		for _, n := range []string{"a", "b", "c"} {
			out = append(out, mkPod("site-00001-deployment-"+n, "1", "512Mi",
				withRestartOnResize("memory"), withReady(true), withLabels("serving.knative.dev/service", "site", "app", "site")))
		}
		return out
	}
	setRevision := func(t *testing.T, h *harness, desired, ready int32) {
		t.Helper()
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "site-00001-deployment", Namespace: testNS, Labels: map[string]string{
				"serving.knative.dev/service": "site", "serving.knative.dev/revision": "site-00001"}},
			Spec:   appsv1.DeploymentSpec{Replicas: &desired},
			Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
		}
		ctx := context.Background()
		if _, err := h.cs.AppsV1().Deployments(testNS).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
			if _, err = h.cs.AppsV1().Deployments(testNS).Create(ctx, d, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed := func(h *harness) {
		e := entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Kind, e.Rec.Workload = "ksvc", "site" })
		h.eng.Seed([]recommender.Recommendation{e.Rec}, map[string]recommender.Entry{
			recommender.Key(testNS, "ksvc", "site", "app"): e,
		})
	}

	t.Run("the autoscaler wants a 4th replica: hold", func(t *testing.T) {
		h := newHarness(t, true, ksvcPods()...)
		setRevision(t, h, 4, 3)
		seed(h)
		h.cycle(t)
		if n := len(h.calls("resize")); n != 0 {
			t.Fatalf("restarted %d Ready pods while 3 of 4 desired replicas are Ready", n)
		}
	})
	t.Run("all desired replicas Ready: one pod goes", func(t *testing.T) {
		h := newHarness(t, true, ksvcPods()...)
		setRevision(t, h, 3, 3)
		seed(h)
		h.cycle(t)
		if n := len(h.calls("resize")); n != 1 {
			t.Fatalf("restarted %d pods, want 1 (one at a time)", n)
		}
	})
}
