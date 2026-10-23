package reconciler

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// stuckSiblingPods: three Ready replicas that want the (disruptive) resize and a
// fourth that is permanently NotReady and already at the target (crash-looping
// after its own restart), so it never takes part in the rolling.
func stuckSiblingPods() []*corev1.Pod {
	pods := disruptivePods(3)
	d := mkPod("web-d", "200m", "128Mi", withRestartOnResize(corev1.ResourceMemory), withRestartOnResize(corev1.ResourceCPU),
		withReady(false), withLabels("app", "web"))
	return append(pods, d)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// disruptive.maxWait: a permanently NotReady sibling holds the rolling only for
// maxWait; after that the rolling proceeds when the live PDB and replica reads
// allow it, never otherwise.
func TestDisruptiveMaxWait(t *testing.T) {
	const wait = 15 * time.Minute
	run := func(t *testing.T, maxWait time.Duration, mut func(h *harness)) (h *harness, ck *clock) {
		t.Helper()
		h = newHarness(t, true, stuckSiblingPods()...)
		h.setDeployment(t, 4, 3)
		ck = &clock{t: time.Unix(1_800_000_000, 0)}
		h.r.now = ck.now
		h.r.cfg.DisruptiveMaxWait = maxWait
		if mut != nil {
			mut(h)
		}
		h.seed(entry(0.2, 128*mib))
		return h, ck
	}
	restarted := func(h *harness) int { return len(h.calls("resize")) }

	t.Run("held inside the wait, proceeds once it expires", func(t *testing.T) {
		h, ck := run(t, wait, nil)
		bypass := metrics.DisruptiveWaitExpired.Value("proceeded")
		h.cycle(t)
		if n := restarted(h); n != 0 {
			t.Fatalf("restarted %d pods while the sibling was just seen NotReady", n)
		}
		ck.t = ck.t.Add(wait - time.Minute)
		h.cycle(t)
		if n := restarted(h); n != 0 {
			t.Fatalf("restarted %d pods inside maxWait", n)
		}
		ck.t = ck.t.Add(2 * time.Minute)
		h.cycle(t)
		if n := restarted(h); n != 1 {
			t.Fatalf("restarted %d pods after maxWait, want exactly 1 (budget)", n)
		}
		if d := metrics.DisruptiveWaitExpired.Value("proceeded") - bypass; d != 1 {
			t.Fatalf("proceeded counter delta = %v, want 1", d)
		}
	})
	t.Run("maxWait 0 waits forever", func(t *testing.T) {
		h, ck := run(t, 0, nil)
		h.cycle(t)
		ck.t = ck.t.Add(48 * time.Hour)
		h.cycle(t)
		if n := restarted(h); n != 0 {
			t.Fatalf("restarted %d pods with maxWait=0", n)
		}
	})
	t.Run("a PDB with no budget still blocks after the wait", func(t *testing.T) {
		h, ck := run(t, wait, func(h *harness) { h.setPDB(t, 0) })
		h.cycle(t)
		ck.t = ck.t.Add(wait + time.Minute)
		h.cycle(t)
		if n := restarted(h); n != 0 {
			t.Fatalf("restarted %d pods against a PDB allowing 0 disruptions", n)
		}
	})
	t.Run("a PDB with budget lets it proceed", func(t *testing.T) {
		h, ck := run(t, wait, func(h *harness) { h.setPDB(t, 1) })
		h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.PDBProtected, e.Rec.PDBName, e.Rec.PDBAllowed = true, "web", 1 }))
		h.cycle(t)
		ck.t = ck.t.Add(wait + time.Minute)
		h.cycle(t)
		if n := restarted(h); n != 1 {
			t.Fatalf("restarted %d pods with a PDB allowing 1, want 1", n)
		}
	})
	t.Run("never restarts the last Ready replica", func(t *testing.T) {
		pods := []*corev1.Pod{
			mkPod("web-a", "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withReady(true), withLabels("app", "web")),
			mkPod("web-d", "200m", "128Mi", withRestartOnResize(corev1.ResourceMemory), withReady(false), withLabels("app", "web")),
		}
		h := newHarness(t, true, pods...)
		h.setDeployment(t, 2, 1)
		ck := &clock{t: time.Unix(1_800_000_000, 0)}
		h.r.now, h.r.cfg.DisruptiveMaxWait = ck.now, wait
		h.seed(entry(0.2, 128*mib))
		before := metrics.Skips.Value("disruptive_wait_expired_held")
		h.cycle(t)
		ck.t = ck.t.Add(wait + time.Minute)
		h.cycle(t)
		if n := restarted(h); n != 0 {
			t.Fatalf("restarted the only Ready replica (%d resizes)", n)
		}
		if d := metrics.Skips.Value("disruptive_wait_expired_held") - before; d == 0 {
			t.Fatal("expired-but-held not counted")
		}
	})
	t.Run("a sibling that only just went NotReady is not stuck", func(t *testing.T) {
		h, ck := run(t, wait, nil)
		h.cycle(t)
		ck.t = ck.t.Add(wait + time.Minute)
		// web-b flips NotReady only now (it was Ready until this cycle). It is itself
		// restarted at once (a NotReady target costs no availability), but the Ready
		// web-a and web-c must keep waiting: web-b has been NotReady for 0s.
		h.markResized(t, "web-b", "1", "512Mi", false)
		h.cycle(t)
		for _, name := range resizedPods(h.calls("resize")) {
			if name != "web-b" {
				t.Fatalf("restarted Ready %s although web-b has been NotReady for 0s", name)
			}
		}
	})
}
