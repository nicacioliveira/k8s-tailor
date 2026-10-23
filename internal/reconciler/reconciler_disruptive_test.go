package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// withRestartOnResize sets resizePolicy RestartContainer on the app container
// for the given resource (a resize of it restarts the container: disruptive).
func withRestartOnResize(res corev1.ResourceName) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].ResizePolicy = append(p.Spec.Containers[0].ResizePolicy,
			corev1.ContainerResizePolicy{ResourceName: res, RestartPolicy: corev1.RestartContainer})
	}
}

func withReady(ready bool) podOpt {
	return func(p *corev1.Pod) {
		st := corev1.ConditionFalse
		if ready {
			st = corev1.ConditionTrue
		}
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: st})
	}
}

func resizedPods(calls []resizeCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

// markResized simulates the kubelet having applied (and the container having
// restarted with) the resize: the pod spec now carries the target requests.
func (h *harness) markResized(t *testing.T, pod, cpu, mem string, ready bool) {
	t.Helper()
	p, err := h.cs.CoreV1().Pods(testNS).Get(context.Background(), pod, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
	}
	p.Status.Conditions = nil
	withReady(ready)(p)
	if _, err := h.cs.CoreV1().Pods(testNS).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// P0-1: a resize that restarts the container (resizePolicy RestartContainer, for
// memory OR cpu) is a disruption. It must go one pod per cycle (or up to the PDB's
// disruptionsAllowed), and only advance once every replica is Ready again — never
// restart all replicas in the same cycle.
func TestDisruptiveResizeRollsOnePodAtATime(t *testing.T) {
	for _, rn := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
		t.Run(string(rn), func(t *testing.T) {
			h := newHarness(t, true,
				mkPod("web-a", "1", "512Mi", withRestartOnResize(rn), withReady(true)),
				mkPod("web-b", "1", "512Mi", withRestartOnResize(rn), withReady(true)),
				mkPod("web-c", "1", "512Mi", withRestartOnResize(rn), withReady(true)),
			)
			h.seed(entry(0.2, 128*mib))
			h.cycle(t)
			if got := resizedPods(h.calls("resize")); len(got) != 1 {
				t.Fatalf("cycle 1 resized %v, want exactly 1 pod (disruptive)", got)
			}
			first := h.calls("resize")[0].Name

			// The restarted pod is not Ready yet: the rolling must wait.
			h.markResized(t, first, "200m", "128Mi", false)
			h.cycle(t)
			if got := len(h.calls("resize")); got != 1 {
				t.Fatalf("advanced while %s not Ready: %d resizes", first, got)
			}

			// Ready again → the next pod goes, without waiting the (1h) cooldown.
			h.markResized(t, first, "200m", "128Mi", true)
			h.cycle(t)
			calls := h.calls("resize")
			if len(calls) != 2 || calls[1].Name == first {
				t.Fatalf("after %s Ready: resizes %v, want a second, different pod", first, resizedPods(calls))
			}
		})
	}
}

// A PDB allowing N disruptions lets the rolling go N pods per cycle (the budget
// is read live: the PDB object, not the refresh's copy).
func TestDisruptiveResizeHonorsPDBBudget(t *testing.T) {
	h := newHarness(t, true,
		mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true), withLabels("app", "web")),
		mkPod("web-b", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true), withLabels("app", "web")),
		mkPod("web-c", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true), withLabels("app", "web")),
	)
	h.setPDB(t, 2)
	h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) {
		e.Rec.PDBProtected, e.Rec.PDBName, e.Rec.PDBAllowed = true, "web", 2
	}))
	h.cycle(t)
	if got := resizedPods(h.calls("resize")); len(got) != 2 {
		t.Fatalf("resized %v, want 2 (PDB disruptionsAllowed=2)", got)
	}
}

// Non-disruptive (NotRequired) resizes still converge every replica at once.
func TestNonDisruptiveResizeStillConvergesAtOnce(t *testing.T) {
	h := newHarness(t, true, mkPod("web-a", "1", "512Mi"), mkPod("web-b", "1", "512Mi"))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	if got := len(h.calls("resize")); got != 2 {
		t.Fatalf("resized %d, want 2", got)
	}
}
