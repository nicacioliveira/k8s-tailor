package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func withLabels(kv ...string) podOpt {
	return func(p *corev1.Pod) {
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		for i := 0; i < len(kv); i += 2 {
			p.Labels[kv[i]] = kv[i+1]
		}
	}
}

// setDeployment rewrites the live Deployment (spec.replicas, status.readyReplicas).
func (h *harness) setDeployment(t *testing.T, replicas, ready int32) {
	t.Helper()
	ctx := context.Background()
	d, err := h.cs.AppsV1().Deployments(testNS).Get(ctx, testDep, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Spec.Replicas, d.Status.ReadyReplicas = &replicas, ready
	if _, err := h.cs.AppsV1().Deployments(testNS).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// setPDB creates or updates a PDB selecting app=web with the given budget.
func (h *harness) setPDB(t *testing.T, allowed int32) {
	t.Helper()
	ctx := context.Background()
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: testNS},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: allowed},
	}
	if _, err := h.cs.PolicyV1().PodDisruptionBudgets(testNS).Get(ctx, "web", metav1.GetOptions{}); err == nil {
		_, err = h.cs.PolicyV1().PodDisruptionBudgets(testNS).Update(ctx, pdb, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := h.cs.PolicyV1().PodDisruptionBudgets(testNS).Create(ctx, pdb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func disruptivePods(n int) []*corev1.Pod {
	var out []*corev1.Pod
	for i := 0; i < n; i++ {
		out = append(out, mkPod("web-"+string(rune('a'+i)), "1", "512Mi",
			withRestartOnResize(corev1.ResourceMemory), withRestartOnResize(corev1.ResourceCPU),
			withReady(true), withLabels("app", "web")))
	}
	return out
}

// The disruptive rolling reads the PDB budget LIVE right before restarting a
// Ready pod: the refresh's value can be up to REFRESH_EVERY old.
func TestDisruptiveUsesLivePDBBudget(t *testing.T) {
	t.Run("stale budget 2, live 0: hold", func(t *testing.T) {
		h := newHarness(t, true, disruptivePods(3)...)
		h.setPDB(t, 0)
		h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) {
			e.Rec.PDBProtected, e.Rec.PDBName, e.Rec.PDBAllowed = true, "web", 2
		}))
		h.cycle(t)
		if got := resizedPods(h.calls("resize")); len(got) != 0 {
			t.Fatalf("restarted %v with a live PDB allowing no disruption", got)
		}
	})
	t.Run("stale unprotected, live budget 2: two per cycle", func(t *testing.T) {
		h := newHarness(t, true, disruptivePods(3)...)
		h.setPDB(t, 2)
		h.seed(entry(0.2, 128*mib))
		h.cycle(t)
		if got := resizedPods(h.calls("resize")); len(got) != 2 {
			t.Fatalf("resized %v, want 2 (live disruptionsAllowed=2)", got)
		}
	})
	t.Run("budget drops between cycles", func(t *testing.T) {
		h := newHarness(t, true, disruptivePods(3)...)
		h.setPDB(t, 1)
		h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) {
			e.Rec.PDBProtected, e.Rec.PDBName, e.Rec.PDBAllowed = true, "web", 1
		}))
		h.cycle(t)
		calls := h.calls("resize")
		if len(calls) != 1 {
			t.Fatalf("cycle 1: resized %v, want 1", resizedPods(calls))
		}
		h.markResized(t, calls[0].Name, "200m", "128Mi", true)
		h.setPDB(t, 0) // e.g. another eviction consumed the budget; the entry still says 1
		h.cycle(t)
		if got := len(h.calls("resize")); got != 1 {
			t.Fatalf("cycle 2 restarted another pod with a live budget of 0 (%d resizes)", got)
		}
	})
}

// The desired replica count and the controller's ready count are read live as
// well: an HPA scale-up (or a replica that went NotReady) after the refresh holds
// the rolling at once.
func TestDisruptiveUsesLiveReplicas(t *testing.T) {
	cases := []struct {
		name           string
		replicas, rdy  int32
		wantRestarted  int
		staleReplicasE int
	}{
		{"live matches", 3, 3, 1, 3},
		{"HPA scaled 3 -> 5 after the refresh", 5, 3, 0, 3},
		{"controller reports a replica not ready", 3, 2, 0, 3},
		{"scaled down 5 -> 3 after the refresh (stale desired 5)", 3, 3, 1, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, disruptivePods(3)...)
			h.setDeployment(t, c.replicas, c.rdy)
			h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Replicas = c.staleReplicasE }))
			h.cycle(t)
			if got := resizedPods(h.calls("resize")); len(got) != c.wantRestarted {
				t.Fatalf("restarted %v, want %d", got, c.wantRestarted)
			}
		})
	}
}

// A failed live read never turns into a restart: hold and retry next tick.
func TestDisruptiveHoldsWhenLiveReadFails(t *testing.T) {
	for _, res := range []string{"poddisruptionbudgets", "deployments"} {
		h := newHarness(t, true, disruptivePods(2)...)
		h.cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fakeErr("apiserver unavailable")
		})
		before := metrics.Skips.Value("disruptive_live_unknown")
		h.seed(entry(0.2, 128*mib))
		h.cycle(t)
		if got := resizedPods(h.calls("resize")); len(got) != 0 {
			t.Fatalf("%s list failing: restarted %v", res, got)
		}
		if d := metrics.Skips.Value("disruptive_live_unknown") - before; d == 0 {
			t.Fatalf("%s list failing: skip not counted", res)
		}
	}
	// Workload deleted between the snapshot and the live read: hold too.
	h := newHarness(t, true, disruptivePods(2)...)
	if err := h.cs.AppsV1().Deployments(testNS).Delete(context.Background(), testDep, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	if got := resizedPods(h.calls("resize")); len(got) != 0 {
		t.Fatalf("workload gone: restarted %v", got)
	}
}

// A non-disruptive resize never pays for the live read.
func TestNonDisruptiveSkipsLiveRead(t *testing.T) {
	h := newHarness(t, true, mkPod("web-a", "1", "512Mi", withReady(true)), mkPod("web-b", "1", "512Mi", withReady(true)))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	for _, a := range h.cs.Actions() {
		if a.GetVerb() == "list" && (a.GetResource().Resource == "poddisruptionbudgets" || a.GetResource().Resource == "deployments") {
			t.Fatalf("non-disruptive resize issued a live read: %v", a)
		}
	}
}

// Live budget 0 on a Ready pod: InPlacePreferred falls back to the template
// rollout; InPlace applies only the part that does not restart the container.
func TestLivePDBBlockFallbacks(t *testing.T) {
	memOnly := func() []*corev1.Pod {
		return []*corev1.Pod{mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true), withLabels("app", "web"))}
	}
	h := newHarness(t, true, memOnly()...)
	h.setPDB(t, 0)
	h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Apply = "InPlacePreferred" }))
	h.cycle(t)
	if r, tpl := len(h.calls("resize")), len(h.calls("")); r != 0 || tpl == 0 {
		t.Fatalf("InPlacePreferred: resizes=%d template patches=%d, want 0 / >0", r, tpl)
	}

	h = newHarness(t, true, memOnly()...)
	h.setPDB(t, 0)
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	calls := h.calls("resize")
	if len(calls) != 1 {
		t.Fatalf("InPlace: resizes = %d, want 1 (CPU part only)", len(calls))
	}
	req := calls[0].Body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
	if req["memory"] != "512Mi" || req["cpu"] != "200m" {
		t.Fatalf("InPlace under a live PDB block: requests %v, want cpu 200m and memory untouched", req)
	}
}
