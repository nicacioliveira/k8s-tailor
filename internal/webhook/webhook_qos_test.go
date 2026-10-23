package webhook

import (
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// A Guaranteed template (every container requests == limits for CPU and
// memory) must keep its QoS class: lowering only the requests would make the
// pod born Burstable, silently changing its eviction class. The webhook admits
// it unmodified and counts the skip, consistent with the reconciler (which never
// resizes a Guaranteed pod in place).
func TestWebhookSkipsGuaranteedTemplate(t *testing.T) {
	cases := []struct {
		name    string
		spec    podSpec
		mutated bool
	}{
		{"guaranteed (req == lim)", podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: rl("cpu", "1", "memory", "512Mi")}, false},
		{"guaranteed (limits only: requests default to limits)", podSpec{owners: rsOwner, lim: rl("cpu", "1", "memory", "512Mi")}, false},
		{"burstable (cpu limit only)", podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: rl("cpu", "1")}, true},
		{"burstable (req < lim)", podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: rl("cpu", "2", "memory", "512Mi")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := metrics.WebhookSkips.Value("guaranteed_qos")
			f := newFixture(t, webEntry(0.2, 128*mib))
			res := call(t, f.wh, review(t, c.spec))
			if got := len(res.ops) > 0; got != c.mutated {
				t.Fatalf("mutated = %v, want %v (ops %v)", got, c.mutated, res.ops)
			}
			d := metrics.WebhookSkips.Value("guaranteed_qos") - before
			if !c.mutated && d != 1 {
				t.Fatalf("guaranteed_qos skips delta = %v, want 1", d)
			}
			if c.mutated && d != 0 {
				t.Fatalf("burstable pod counted as guaranteed_qos skip (%v)", d)
			}
		})
	}
}

// memory.enforceLimit: the webhook adds the recommended memory limit to a
// container created without one — only with the opt-in.
func TestWebhookEnforceMemLimit(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		f := newFixture(t, webEntry(0.2, 128*mib, func(e *recommender.Entry) {
			e.Rec.Recommended.MemLimit = 192 * mib
			e.EnforceMemLimit = enforce
		}))
		res := call(t, f.wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}))
		_, lim := resourcesOf(t, res.ops)
		got, has := lim["memory"]
		if has != enforce || (enforce && got != "192Mi") {
			t.Fatalf("enforce=%v: memory limit %v (has=%v)", enforce, got, has)
		}
	}
}

// guaranteed: resizeBoth — a Guaranteed template is sized with request and limit
// set to the same value (the pod is born Guaranteed, as its owner declared).
// CPU follows the recommendation in both directions; memory only grows.
func TestWebhookGuaranteedResizeBoth(t *testing.T) {
	spec := podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: rl("cpu", "1", "memory", "512Mi")}
	resizeBoth := func(cpu, mem float64) *recommender.Entry {
		return webEntry(cpu, mem, func(e *recommender.Entry) {
			e.GuaranteedResize = true
			e.Rec.Recommended.CPULimit = cpu
			e.Rec.Recommended.MemLimit = mem
		})
	}
	cases := []struct {
		name       string
		entry      *recommender.Entry
		wantCPU    string
		wantMem    string
		memSkipped float64
	}{
		{"grow both", resizeBoth(2, 1024*mib), "2000m", "1024Mi", 0},
		{"shrink cpu, memory kept", resizeBoth(0.5, 128*mib), "500m", "512Mi", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := metrics.WebhookSkips.Value("guaranteed_mem_shrink")
			f := newFixture(t, c.entry)
			res := call(t, f.wh, review(t, spec))
			if len(res.ops) == 0 {
				t.Fatal("not mutated")
			}
			req, lim := resourcesOf(t, res.ops)
			for _, r := range []struct{ name, want string }{{"cpu", c.wantCPU}, {"memory", c.wantMem}} {
				if req[r.name] != r.want || lim[r.name] != r.want {
					t.Fatalf("%s: request %v limit %v, want both %s (pod would stop being Guaranteed)", r.name, req[r.name], lim[r.name], r.want)
				}
			}
			if d := metrics.WebhookSkips.Value("guaranteed_mem_shrink") - before; d != c.memSkipped {
				t.Fatalf("guaranteed_mem_shrink delta = %v, want %v", d, c.memSkipped)
			}
		})
	}

	t.Run("without the opt-in the template is admitted unmodified", func(t *testing.T) {
		before := metrics.WebhookSkips.Value("guaranteed_qos")
		e := resizeBoth(2, 1024*mib)
		e.GuaranteedResize = false
		res := call(t, newFixture(t, e).wh, review(t, spec))
		if len(res.ops) != 0 {
			t.Fatalf("mutated without the opt-in: %v", res.ops)
		}
		if d := metrics.WebhookSkips.Value("guaranteed_qos") - before; d != 1 {
			t.Fatalf("guaranteed_qos delta = %v, want 1", d)
		}
	})
}
