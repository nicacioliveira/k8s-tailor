package reconciler

import (
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func resizeLimits(t *testing.T, c resizeCall) map[string]any {
	t.Helper()
	ctr := c.Body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	lim, _ := ctr["resources"].(map[string]any)["limits"].(map[string]any)
	return lim
}

// memory.enforceLimit (opt-in): a container WITHOUT a memory limit gets the
// recommended one, in place — even when its request has not drifted (otherwise
// the limit would never land). Without the opt-in nothing is introduced.
func TestEnforceMemLimitInPlace(t *testing.T) {
	cases := []struct {
		name    string
		enforce bool
		recMem  float64 // request recommendation (pod runs 512Mi)
		want    any     // memory limit patched (nil = none, no resize at all when !resize)
		resize  bool
	}{
		{"opt-in, request drifted", true, 256 * mib, "384Mi", true},
		// The request stays at 512Mi, so the 384Mi limit is lifted to it.
		{"opt-in, request within tolerance: limit only", true, 500 * mib, "512Mi", true},
		{"default: never introduced", false, 256 * mib, nil, true},
		{"default, no drift: nothing to do", false, 500 * mib, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, mkPod("p", "1", "512Mi"))
			h.seed(entry(1, c.recMem, func(e *recommender.Entry) {
				e.Rec.Recommended.MemLimit = 384 * mib
				e.EnforceMemLimit = c.enforce
			}))
			h.cycle(t)
			calls := h.calls("resize")
			if (len(calls) == 1) != c.resize {
				t.Fatalf("resize calls = %d, want resize=%v", len(calls), c.resize)
			}
			if !c.resize {
				return
			}
			lim := resizeLimits(t, calls[0])
			if got, ok := lim["memory"]; (c.want == nil && ok) || (c.want != nil && got != c.want) {
				t.Fatalf("memory limit = %v, want %v", got, c.want)
			}
			if c.name == "opt-in, request within tolerance: limit only" {
				req := calls[0].Body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
				if req["memory"] != "512Mi" {
					t.Fatalf("pure limit add moved the request: %v", req)
				}
			}
		})
	}
}

// The limit added is never below the patched request.
func TestEnforceMemLimitNotBelowRequest(t *testing.T) {
	p := PlanInPlace(entry(1, 1024*mib, func(e *recommender.Entry) {
		e.Rec.Recommended.MemLimit = 512 * mib
		e.EnforceMemLimit = true
	}), k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib})
	_, m := p.Limits(recommender.Resources{CPURequest: 1, MemRequest: 1024 * mib, MemLimit: 512 * mib}, k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib})
	if m < 1024*mib {
		t.Fatalf("limit %v below request 1024Mi", m/mib)
	}
}

// A memory-held (HPA on memory) container never gets a limit added.
func TestEnforceMemLimitRespectsHold(t *testing.T) {
	p := PlanInPlace(entry(1, 512*mib, func(e *recommender.Entry) {
		e.Rec.Recommended.MemLimit = 768 * mib
		e.Rec.MemHeldByHPA = true
		e.EnforceMemLimit = true
	}), k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib})
	if p.Wants() {
		t.Fatalf("memory held by an HPA but the plan wants a change: %+v", p)
	}
}

// The rollout path adds the limit to the template too, only with the opt-in.
func TestEnforceMemLimitRollout(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		h := newHarness(t, false, mkPod("a", "1", "512Mi"))
		h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) {
			e.Apply = policy.ApplyRollout
			e.Rec.Recommended.MemLimit = 192 * mib
			e.EnforceMemLimit = enforce
		}))
		h.cycle(t)
		calls := h.calls("")
		if len(calls) != 1 {
			t.Fatalf("enforce=%v: template patches = %d, want 1", enforce, len(calls))
		}
		tmpl := calls[0].Body["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		res := tmpl["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
		lim, _ := res["limits"].(map[string]any)
		got, has := lim["memory"]
		if has != enforce || (enforce && got != "192Mi") {
			t.Fatalf("enforce=%v: memory limit %v (has=%v)", enforce, got, has)
		}
	}
}
