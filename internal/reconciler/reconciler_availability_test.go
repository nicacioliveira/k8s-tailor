package reconciler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func withPhase(ph corev1.PodPhase) podOpt {
	return func(p *corev1.Pod) { p.Status.Phase = ph }
}

// P1-1 (2nd review): the snapshot only carried Running pods, so a Pending replica
// (no node, image pulling, Init) was invisible to the readiness gate and the
// disruptive rolling restarted a Ready replica with only 1/3 serving.
func TestDisruptiveHoldsWhileAReplicaIsPending(t *testing.T) {
	h := newHarness(t, true,
		mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
		mkPod("web-b", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
		mkPod("web-c", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(false), withPhase(corev1.PodPending)),
	)
	h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Replicas = 3 }))
	h.cycle(t)
	if got := resizedPods(h.calls("resize")); len(got) != 0 {
		t.Fatalf("restarted %v with a Pending replica", got)
	}
}

// P1-1: a missing replica (spec.replicas=3, only 2 pods exist; not yet created
// by the controller) also holds the disruptive rolling.
func TestDisruptiveHoldsWhileAReplicaIsMissing(t *testing.T) {
	h := newHarness(t, true,
		mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
		mkPod("web-b", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
	)
	h.setDeployment(t, 3, 2) // live spec.replicas=3, 2 Ready
	h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Replicas = 3 }))
	h.cycle(t)
	if got := resizedPods(h.calls("resize")); len(got) != 0 {
		t.Fatalf("restarted %v with 2/3 replicas existing", got)
	}
	// With the desired count present and Ready, the rolling proceeds (1 per cycle).
	h2 := newHarness(t, true,
		mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
		mkPod("web-b", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
	)
	h2.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Replicas = 2 }))
	h2.cycle(t)
	if got := resizedPods(h2.calls("resize")); len(got) != 1 {
		t.Fatalf("resized %v, want exactly 1", got)
	}
}

// P1-2 (2nd review): all replicas NotReady (crash-loop from OOM). The othersReady
// gate blocked the emergency increase of any of them: deadlock. Restarting a pod
// that is no longer serving costs no availability, so the gate and the budget
// (which only count Ready pods) do not apply to it.
func TestDisruptiveOOMGrowWhenAllReplicasNotReady(t *testing.T) {
	for _, pdb := range []bool{false, true} {
		h := newHarness(t, true,
			mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(false)),
			mkPod("web-b", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(false)),
		)
		h.seed(entry(1, 1024*mib, func(e *recommender.Entry) {
			e.Rec.OOMRisk = true
			e.Rec.Replicas = 2
			if pdb { // PDB minAvailable with all unhealthy → disruptionsAllowed=0
				e.Rec.PDBProtected, e.Rec.PDBName, e.Rec.PDBAllowed = true, "web", 0
			}
		}))
		h.cycle(t)
		if got := resizedPods(h.calls("resize")); len(got) != 2 {
			t.Fatalf("pdb=%v: resized %v, want both NotReady replicas in the 1st cycle", pdb, got)
		}
	}
}

// A Ready target keeps its normal pacing even when another NotReady target was
// restarted in the same cycle: the NotReady one does not consume the budget, but the
// Ready one keeps waiting until all the other replicas are Ready.
func TestDisruptiveReadyTargetStillWaitsForNotReadySibling(t *testing.T) {
	h := newHarness(t, true,
		mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(false)),
		mkPod("web-b", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true)),
	)
	h.seed(entry(1, 1024*mib, func(e *recommender.Entry) { e.Rec.OOMRisk = true; e.Rec.Replicas = 2 }))
	h.cycle(t)
	got := resizedPods(h.calls("resize"))
	if len(got) != 1 || got[0] != "web-a" {
		t.Fatalf("resized %v, want only web-a (NotReady); web-b waits for web-a to come back", got)
	}
}
