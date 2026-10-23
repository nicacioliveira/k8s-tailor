package reconciler

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

type censorCase struct {
	usage      float64 // per-sample CPU usage (cores), 1 replica, 24 x 300 s
	tmpl       float64 // template CPU limit (what the recommender reads as current)
	live       k8s.Container
	throttled  []float64 // optional cfs throttled-period ratio per sample (nil = series absent)
	dir        policy.Direction
	maxCPU     float64
	allowExced bool
}

// censoredPlan runs the real Recommend and the real Decide for a container
// whose CPU usage is measured under a CPU limit (the Istio optin proxy: request
// 20m, limit 50m, usage pinned at ~50.8m).
func censoredPlan(t *testing.T, cc censorCase) (recommender.Recommendation, Plan, bool) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	var cpu, mem, thr []recommender.Sample
	for i := 0; i < 24; i++ {
		ts := float64(now.Unix() - int64((23-i)*300))
		cpu = append(cpu, recommender.Sample{T: ts, V: cc.usage})
		mem = append(mem, recommender.Sample{T: ts, V: 44 * mib})
		if cc.throttled != nil {
			thr = append(thr, recommender.Sample{T: ts, V: cc.throttled[i%len(cc.throttled)]})
		}
	}
	dir := cc.dir
	if dir == "" {
		dir = policy.DirBoth
	}
	res := policy.Resolved{
		Mode: policy.ModeInPlace, Direction: dir, Season: "off", AllowExceedLimit: cc.allowExced,
		CPU: policy.ResourceProfile{Quantile: 0.95, Margin: 0.25, Min: 0.01, Max: cc.maxCPU},
		Mem: policy.ResourceProfile{Quantile: 0.99, Margin: 0.20, Min: 32 * mib, LimitFactor: 1},
	}
	tmpl := k8s.Container{Name: "istio-proxy", CPURequest: cc.live.CPURequest, CPULimit: cc.tmpl, MemRequest: 64 * mib, MemLimit: 256 * mib}
	rec := recommender.Recommend(recommender.RecommendInput{
		Workload:  k8s.Workload{Namespace: "mesh-demo", Kind: "Deployment", Name: "optin", Replicas: 1},
		Container: tmpl, CPU: cpu, Mem: mem, CPUThrottled: thr, LiveCPULimit: cc.live.CPULimit,
		Policy: res, HalfLifeSec: 600, Now: now, MinSamples: 2, CPULimitGrowth: 1.5, CPUCensorMaxFactor: 8,
	})
	entry := recommender.Entry{Rec: rec, Mode: policy.ModeInPlace, Direction: dir,
		ScaleUpCooldown: 30 * time.Second, ScaleDownCooldown: time.Hour}
	p, ok := Decide(entry, cc.live, now, time.Time{})
	return rec, p, ok
}

func proxyLive(req, lim float64) k8s.Container {
	return k8s.Container{Name: "istio-proxy", CPURequest: req, CPULimit: lim, MemRequest: 64 * mib, MemLimit: 256 * mib}
}

// Istio finding 1: usage pinned at the CPU limit is censored data — the real
// demand is unknown but >= the limit. The engine must raise the LIMIT (the
// quota that throttles) by a growth step and lift the request to at least the
// limit, past the absolute drift gate, instead of recommending ~the limit
// forever.
func TestCensoredCPURaisesLimit(t *testing.T) {
	cases := []struct {
		name            string
		cc              censorCase
		wantReq, wantLm float64 // patched request / CPU limit (0 = limit untouched)
	}{
		{"request already at the limit (stuck case)", censorCase{usage: 0.0508, tmpl: 0.05, live: proxyLive(0.05, 0.05)}, 0.05, 0.075},
		{"request below the limit", censorCase{usage: 0.0508, tmpl: 0.05, live: proxyLive(0.02, 0.05)}, 0.05, 0.075},
		// The template still says 50m after an in-place raise: the LIVE limit is
		// the one usage is capped at, and the next step grows from it.
		{"next step from the live limit", censorCase{usage: 0.0752, tmpl: 0.05, live: proxyLive(0.05, 0.075)}, 0.075, 0.113},
		// Average rate below the limit, but the kernel throttles most periods.
		{"cAdvisor throttling signal", censorCase{usage: 0.03, tmpl: 0.05, live: proxyLive(0.05, 0.05), throttled: []float64{0.6}}, 0.05, 0.075},
		{"policy max bounds the step", censorCase{usage: 0.0508, tmpl: 0.05, live: proxyLive(0.05, 0.05), maxCPU: 0.06}, 0.05, 0.06},
		// An explicit policy cpu.max is the operator's bound: it replaces the
		// template × CPU_CENSOR_MAX_FACTOR brake (8 × 50m = 400m here).
		{"policy max above the brake", censorCase{usage: 0.4, tmpl: 0.05, live: proxyLive(0.4, 0.4), maxCPU: 1}, 0.4, 0.6},
		{"allowExceedLimit", censorCase{usage: 0.0508, tmpl: 0.05, live: proxyLive(0.05, 0.05), allowExced: true}, 0.064, 0.075},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, p, ok := censoredPlan(t, tc.cc)
			if !rec.Signals.CPUCensored {
				t.Fatalf("not detected as censored: signals %+v", rec.Signals)
			}
			if !ok || !p.Wants() || !p.Up(tc.cc.live) {
				t.Fatalf("no scale-up planned: ok=%v plan=%+v rec=%+v", ok, p, rec.Recommended)
			}
			cpuLim, _ := p.Limits(rec.Recommended, tc.cc.live)
			if QuantizeCPU(p.TargetCPU) != tc.wantReq || QuantizeCPU(cpuLim) != tc.wantLm {
				t.Fatalf("patch cpu request=%v limit=%v, want %v / %v", p.TargetCPU, cpuLim, tc.wantReq, tc.wantLm)
			}
			if p.TargetCPU > cpuLim {
				t.Fatalf("request %v above limit %v", p.TargetCPU, cpuLim)
			}
		})
	}
}

// What must NOT change: usage well under the limit, a missing throttling series
// (absent = no signal, never 0 = "not throttled" either), shrink-only policies,
// the policy max already reached, and containers without a CPU limit.
func TestCensoredCPUNoFalseRaise(t *testing.T) {
	cases := []struct {
		name string
		cc   censorCase
	}{
		{"usage well under the limit", censorCase{usage: 0.02, tmpl: 0.05, live: proxyLive(0.03, 0.05)}},
		{"low throttling ratio", censorCase{usage: 0.02, tmpl: 0.05, live: proxyLive(0.03, 0.05), throttled: []float64{0.02}}},
		{"shrink-only policy", censorCase{usage: 0.0508, tmpl: 0.05, live: proxyLive(0.05, 0.05), dir: policy.DirDown}},
		{"policy max already reached", censorCase{usage: 0.0508, tmpl: 0.05, live: proxyLive(0.05, 0.05), maxCPU: 0.05}},
		// Without a policy max, the live limit already at template × 8: brake.
		{"brake without policy max", censorCase{usage: 0.4, tmpl: 0.05, live: proxyLive(0.4, 0.4)}},
		{"no CPU limit", censorCase{usage: 0.0508, tmpl: 0, live: proxyLive(0.06, 0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, p, ok := censoredPlan(t, tc.cc)
			cpuLim, _ := p.Limits(rec.Recommended, tc.cc.live)
			if ok && (p.CPULimitRaise || (cpuLim > 0 && cpuLim > tc.cc.live.CPULimit)) {
				t.Fatalf("CPU limit raised: plan=%+v lim=%v rec=%+v signals=%+v", p, cpuLim, rec.Recommended, rec.Signals)
			}
			if ok && p.TargetCPU > tc.cc.live.CPURequest && tc.cc.dir == policy.DirDown {
				t.Fatalf("shrink-only policy grew the request: %+v", p)
			}
		})
	}
}

// The live reconciler (in-place and template rollout) applies the censored-CPU
// limit raise that the drift gate used to swallow: request 50m == limit 50m,
// recommendation 50m / limit 75m.
func TestReconcileCensoredCPURaisesLimit(t *testing.T) {
	censored := func(apply policy.Apply) func(*recommender.Entry) {
		return func(e *recommender.Entry) {
			e.Apply = apply
			e.Rec.Current = recommender.Resources{CPURequest: 0.05, CPULimit: 0.05, MemRequest: 64 * mib}
			e.Rec.Recommended = recommender.Resources{CPURequest: 0.05, CPULimit: 0.075, MemRequest: 64 * mib}
			e.Rec.Signals.CPUCensored = true
		}
	}
	t.Run("in-place", func(t *testing.T) {
		h := newHarness(t, true, mkPod("a", "50m", "64Mi", withLimits("50m", "")))
		h.seed(entry(0, 0, censored(policy.ApplyInPlace)))
		h.cycle(t)
		calls := h.calls("resize")
		if len(calls) != 1 {
			t.Fatalf("resize calls = %d, want 1 (censored limit raise)", len(calls))
		}
		res := calls[0].Body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
		if lim := res["limits"].(map[string]any)["cpu"]; lim != "75m" {
			t.Fatalf("cpu limit patched = %v, want 75m (%v)", lim, res)
		}
		if req := res["requests"].(map[string]any)["cpu"]; req != "50m" {
			t.Fatalf("cpu request patched = %v, want 50m", req)
		}
	})
	t.Run("rollout", func(t *testing.T) {
		h := newHarness(t, false, mkPod("a", "50m", "64Mi", withLimits("50m", "")))
		h.seed(entry(0, 0, censored(policy.ApplyRollout)))
		h.cycle(t)
		calls := h.calls("")
		if len(calls) != 1 {
			t.Fatalf("template patches = %d, want 1", len(calls))
		}
		tmpl := calls[0].Body["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		res := tmpl["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
		if lim := res["limits"].(map[string]any)["cpu"]; lim != "75m" {
			t.Fatalf("template cpu limit = %v, want 75m (%v)", lim, res)
		}
	})
	t.Run("not censored: drift gate holds", func(t *testing.T) {
		h := newHarness(t, true, mkPod("a", "50m", "64Mi", withLimits("50m", "")))
		h.seed(entry(0, 0, censored(policy.ApplyInPlace), func(e *recommender.Entry) { e.Rec.Signals.CPUCensored = false }))
		h.cycle(t)
		if n := len(h.calls("resize")); n != 0 {
			t.Fatalf("resize calls = %d, want 0 without censoring", n)
		}
	})
}
