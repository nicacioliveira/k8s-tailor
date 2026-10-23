package reconciler

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func withLastApplied(container string, at time.Time) podOpt {
	return func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[k8s.AnnLastApplied] = k8s.FormatLastApplied(map[string]time.Time{container: at})
	}
}

// events returns the Events the reconciler created (reason -> []Event).
func (h *harness) events() map[string][]corev1.Event {
	out := map[string][]corev1.Event{}
	for _, a := range h.cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "events" {
			continue
		}
		ev := a.(k8stesting.CreateAction).GetObject().(*corev1.Event)
		out[ev.Reason] = append(out[ev.Reason], *ev)
	}
	return out
}

// mutations returns every write the reconciler issued against the API, except
// Event creation.
func (h *harness) mutations() []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range h.cs.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
			continue
		}
		if a.GetResource().Resource == "events" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// The cooldown must survive an engine restart / leader failover: a FRESH
// reconciler (empty in-memory state) reads the last actuation back from the
// pod annotation and holds a scale-down inside the cooldown.
func TestCooldownRebuiltFromAnnotation(t *testing.T) {
	cases := []struct {
		name string
		ago  time.Duration
		want int
	}{
		{"stamped 1m ago, 1h down-cooldown -> hold", time.Minute, 0},
		{"stamped 2h ago -> act", 2 * time.Hour, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, mkPod("p", "1", "512Mi", withLastApplied("app", time.Now().Add(-c.ago))))
			h.seed(entry(0.2, 128*mib))
			h.cycle(t)
			if got := len(h.calls("resize")); got != c.want {
				t.Fatalf("resize calls = %d, want %d", got, c.want)
			}
		})
	}
}

// A rollout's 10-minute floor is also rebuilt from the (template-stamped) pods.
func TestRolloutCooldownRebuiltFromAnnotation(t *testing.T) {
	h := newHarness(t, false, mkPod("a", "1", "512Mi", withLastApplied("app", time.Now().Add(-time.Minute))))
	h.seed(entry(2, 1024*mib, func(e *recommender.Entry) { e.Apply = policy.ApplyRollout }))
	h.cycle(t)
	if got := len(h.calls("")); got != 0 {
		t.Fatalf("template patches = %d, want 0 (persisted cooldown)", got)
	}
}

// After an in-place resize the pod is stamped (merged with existing stamps).
func TestInPlaceResizeStampsPod(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	h := newHarness(t, true, mkPod("p", "1", "512Mi", withLastApplied("sidecar", old)))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	var stamp string
	for _, c := range h.calls("") {
		if c.Kind == "pods" && c.Name == "p" {
			anns := c.Body["metadata"].(map[string]any)["annotations"].(map[string]any)
			stamp, _ = anns[k8s.AnnLastApplied].(string)
		}
	}
	m := k8s.ParseLastApplied(stamp)
	if m["app"].IsZero() || !m["sidecar"].Equal(old) {
		t.Fatalf("stamp = %q (parsed %v), want app=now and sidecar kept", stamp, m)
	}
}

// A rollout stamps the pod TEMPLATE so the rolled pods carry the cooldown.
func TestRolloutStampsTemplate(t *testing.T) {
	h := newHarness(t, false, mkPod("a", "1", "512Mi"))
	h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Apply = policy.ApplyRollout }))
	h.cycle(t)
	calls := h.calls("")
	if len(calls) != 1 {
		t.Fatalf("template patches = %d, want 1", len(calls))
	}
	tmpl := calls[0].Body["spec"].(map[string]any)["template"].(map[string]any)
	anns := tmpl["metadata"].(map[string]any)["annotations"].(map[string]any)
	if m := k8s.ParseLastApplied(anns[k8s.AnnLastApplied].(string)); m["app"].IsZero() {
		t.Fatalf("template not stamped: %v", anns)
	}
}

func TestEventsAndMetricsOnResize(t *testing.T) {
	applied := metrics.Resizes.Value("inplace", "applied")
	failed := metrics.Resizes.Value("inplace", "failed")

	h := newHarness(t, true, mkPod("ok", "1", "512Mi", func(p *corev1.Pod) { p.UID = "u-ok" }), mkPod("bad", "1", "512Mi"))
	h.failPod["bad"] = true
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)

	ev := h.events()
	var podEv, wlEv bool
	for _, e := range ev["Rightsized"] {
		if e.InvolvedObject.Kind == "Pod" && e.InvolvedObject.Name == "ok" && string(e.InvolvedObject.UID) == "u-ok" {
			podEv = true
		}
		if e.InvolvedObject.Kind == "Deployment" && e.InvolvedObject.Name == testDep {
			wlEv = true
		}
	}
	if !podEv || !wlEv {
		t.Fatalf("Rightsized events pod=%v workload=%v (%v)", podEv, wlEv, ev)
	}
	if len(ev["ResizeFailed"]) != 1 || ev["ResizeFailed"][0].Type != corev1.EventTypeWarning {
		t.Fatalf("ResizeFailed events = %v", ev["ResizeFailed"])
	}
	if d := metrics.Resizes.Value("inplace", "applied") - applied; d != 1 {
		t.Fatalf("applied counter delta = %v, want 1", d)
	}
	if d := metrics.Resizes.Value("inplace", "failed") - failed; d != 1 {
		t.Fatalf("failed counter delta = %v, want 1", d)
	}
}

// An Infeasible resize is surfaced as ONE Warning Event (deduplicated across
// ticks) and counted as a skip every tick.
func TestInfeasibleSkipEventDeduplicated(t *testing.T) {
	before := metrics.Skips.Value("resize_pending")
	h := newHarness(t, true, mkPod("p", "1", "512Mi", withResizeCondition("PodResizePending", "Infeasible")))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	h.cycle(t)
	h.cycle(t)
	if n := len(h.events()["ResizeInfeasible"]); n != 1 {
		t.Fatalf("ResizeInfeasible events = %d, want 1 (dedupe)", n)
	}
	if d := metrics.Skips.Value("resize_pending") - before; d != 3 {
		t.Fatalf("skip counter delta = %v, want 3", d)
	}
	// After the dedupe window the Event is re-emitted.
	h.r.now = func() time.Time { return time.Now().Add(2 * skipEventEvery) }
	h.cycle(t)
	if n := len(h.events()["ResizeInfeasible"]); n != 2 {
		t.Fatalf("ResizeInfeasible events after window = %d, want 2", n)
	}
}

// A Deferred resize is transient — counted, but no Event noise.
func TestDeferredSkipNoEvent(t *testing.T) {
	h := newHarness(t, true, mkPod("p", "1", "512Mi", withResizeCondition("PodResizePending", "Deferred")))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	if n := len(h.events()["ResizeInfeasible"]); n != 0 {
		t.Fatalf("events for Deferred = %d, want 0", n)
	}
}

// Recommend / Off must NEVER mutate anything, whatever the apply method, QoS,
// OOM state or in-place support — the shadow-mode guarantee.
func TestNonActuatingModesNeverMutate(t *testing.T) {
	for _, mode := range []policy.Mode{policy.ModeRecommend, policy.ModeOff, policy.ModeWebhook} {
		for _, apply := range []policy.Apply{policy.ApplyInPlace, policy.ApplyRollout, policy.ApplyInPlacePreferred} {
			for _, inPlace := range []bool{true, false} {
				for _, qos := range []corev1.PodQOSClass{corev1.PodQOSBurstable, corev1.PodQOSGuaranteed} {
					h := newHarness(t, inPlace, mkPod("p", "1", "512Mi", withQoS(qos)), mkPod("q", "1", "512Mi",
						withResizeCondition("PodResizePending", "Infeasible")))
					h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) {
						e.Mode, e.Apply = mode, apply
						e.Rec.OOMRisk = true
						e.Rec.Recommended.MemLimit = 2048 * mib
					}))
					h.cycle(t)
					h.cycle(t)
					if m := h.mutations(); len(m) != 0 {
						t.Fatalf("mode=%s apply=%s inPlace=%v qos=%s: %d mutations (%v)", mode, apply, inPlace, qos, len(m), m)
					}
					if ev := h.events(); len(ev) != 0 {
						t.Fatalf("mode=%s apply=%s: events emitted %v", mode, apply, ev)
					}
				}
			}
		}
	}
}
