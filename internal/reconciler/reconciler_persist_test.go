package reconciler

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func withAnnotation(k, v string) podOpt {
	return func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[k] = v
	}
}

// podAnnotationPatches returns the annotation maps of every metadata patch on pod.
func (h *harness) podAnnotationPatches(pod string) []map[string]any {
	var out []map[string]any
	for _, c := range h.calls("") {
		if c.Kind != "pods" || c.Name != pod {
			continue
		}
		if md, ok := c.Body["metadata"].(map[string]any); ok {
			if a, ok := md["annotations"].(map[string]any); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// A rejected resize is persisted on the pod (time + reason), so the failure
// backoff survives a restart / failover.
func TestResizeFailurePersisted(t *testing.T) {
	h := newHarness(t, true, mkPod("bad", "1", "512Mi"))
	h.failPod["bad"] = true
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	var got map[string]k8s.ResizeFailure
	for _, a := range h.podAnnotationPatches("bad") {
		if v, ok := a[k8s.AnnResizeFailed].(string); ok {
			got = k8s.ParseResizeFailed(v)
		}
	}
	f, ok := got["app"]
	if !ok || f.At.IsZero() || f.Reason == "" {
		t.Fatalf("failure not persisted: %v", got)
	}
}

// A restarted engine (fresh reconciler) does not retry a pod whose resize was
// rejected within the backoff; past it, it retries.
func TestResizeFailureBackoffSurvivesRestart(t *testing.T) {
	cases := []struct {
		name string
		ago  time.Duration
		want int
	}{
		{"rejected 1m ago: held", time.Minute, 0},
		{"rejected 6m ago: retried", 6 * time.Minute, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := k8s.FormatResizeFailed(map[string]k8s.ResizeFailure{"app": {At: time.Now().Add(-c.ago), Reason: "forbidden"}})
			h := newHarness(t, true, mkPod("p", "1", "512Mi", withAnnotation(k8s.AnnResizeFailed, v)))
			before := metrics.Skips.Value("failure_backoff")
			h.seed(entry(0.2, 128*mib))
			h.cycle(t)
			if got := len(h.calls("resize")); got != c.want {
				t.Fatalf("resize calls = %d, want %d", got, c.want)
			}
			if c.want == 0 && metrics.Skips.Value("failure_backoff")-before != 1 {
				t.Fatal("persisted backoff not counted as failure_backoff")
			}
		})
	}
}

// A successful resize clears the container's persisted failure.
func TestResizeSuccessClearsPersistedFailure(t *testing.T) {
	v := k8s.FormatResizeFailed(map[string]k8s.ResizeFailure{"app": {At: time.Now().Add(-time.Hour), Reason: "x"}})
	h := newHarness(t, true, mkPod("p", "1", "512Mi", withAnnotation(k8s.AnnResizeFailed, v)))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	cleared := false
	for _, a := range h.podAnnotationPatches("p") {
		if val, ok := a[k8s.AnnResizeFailed]; ok && val == nil {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("persisted failure not cleared after a successful resize: %v", h.podAnnotationPatches("p"))
	}
}

// Each pod restarted by a disruptive rolling is marked (same time as its
// last-applied stamp).
func TestDisruptiveRollingStepPersisted(t *testing.T) {
	h := newHarness(t, true, disruptivePods(2)...)
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	calls := h.calls("resize")
	if len(calls) != 1 {
		t.Fatalf("resizes = %d, want 1", len(calls))
	}
	var la, ro map[string]time.Time
	for _, a := range h.podAnnotationPatches(calls[0].Name) {
		if v, ok := a[k8s.AnnLastApplied].(string); ok {
			la = k8s.ParseLastApplied(v)
		}
		if v, ok := a[k8s.AnnRolling].(string); ok {
			ro = k8s.ParseLastApplied(v)
		}
	}
	if ro["app"].IsZero() || !ro["app"].Equal(la["app"]) {
		t.Fatalf("rolling marker %v, last-applied %v: want the same time", ro, la)
	}
	// A non-disruptive resize writes no rolling marker.
	h2 := newHarness(t, true, mkPod("q", "1", "512Mi"))
	h2.seed(entry(0.2, 128*mib))
	h2.cycle(t)
	for _, a := range h2.podAnnotationPatches("q") {
		if _, ok := a[k8s.AnnRolling]; ok {
			t.Fatalf("non-disruptive resize marked as a rolling step: %v", a)
		}
	}
}

// After a restart mid-rolling (web-a already restarted 1 minute ago, 1h
// scale-down cooldown), the fresh reconciler resumes the rolling — next pod
// without waiting the cooldown — but still only behind the readiness gate.
func TestDisruptiveRollingResumesAfterRestart(t *testing.T) {
	step := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	done := func(ready, marked bool) *corev1.Pod {
		opts := []podOpt{withRestartOnResize(corev1.ResourceMemory), withRestartOnResize(corev1.ResourceCPU),
			withReady(ready), withLabels("app", "web"), withLastApplied("app", step)}
		if marked {
			opts = append(opts, withAnnotation(k8s.AnnRolling, k8s.FormatLastApplied(map[string]time.Time{"app": step})))
		}
		return mkPod("web-a", "200m", "128Mi", opts...)
	}
	cases := []struct {
		name string
		a    *corev1.Pod
		want int
	}{
		{"rolling resumed: next pod at once", done(true, true), 1},
		{"restarted pod not Ready yet: readiness gate holds", done(false, true), 0},
		{"no rolling marker: plain cooldown holds", done(true, false), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pods := append([]*corev1.Pod{c.a}, disruptivePods(3)[1:]...)
			h := newHarness(t, true, pods...)
			h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Replicas = 3 }))
			h.cycle(t)
			got := resizedPods(h.calls("resize"))
			if len(got) != c.want {
				t.Fatalf("restarted %v, want %d", got, c.want)
			}
			for _, n := range got {
				if n == "web-a" {
					t.Fatal("re-restarted the pod already at its target")
				}
			}
		})
	}
}

// A stale marker from a FINISHED rolling does not let a new rolling skip the
// cooldown: every pod wants the new change, none is "already done".
func TestOldRollingMarkerDoesNotBypassCooldown(t *testing.T) {
	step := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	var pods []*corev1.Pod
	for _, n := range []string{"web-a", "web-b"} {
		pods = append(pods, mkPod(n, "1", "512Mi", withRestartOnResize(corev1.ResourceMemory), withRestartOnResize(corev1.ResourceCPU),
			withReady(true), withLabels("app", "web"), withLastApplied("app", step),
			withAnnotation(k8s.AnnRolling, k8s.FormatLastApplied(map[string]time.Time{"app": step}))))
	}
	h := newHarness(t, true, pods...)
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	if got := resizedPods(h.calls("resize")); len(got) != 0 {
		t.Fatalf("restarted %v inside the cooldown on a stale rolling marker", got)
	}
}
