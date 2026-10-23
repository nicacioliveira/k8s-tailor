package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// greedyInput: one replica whose template CPU limit is 50m and whose live limit
// is `live`; CPU usage is `usage` for the whole last hour.
func greedyInput(live, usage float64, mut func(*RecommendInput)) RecommendInput {
	now := time.Unix(1_800_000_000, 0)
	var cpu, mem []Sample
	for i := 0; i < 24; i++ {
		ts := float64(now.Unix() - int64((23-i)*300))
		cpu = append(cpu, Sample{T: ts, V: usage})
		mem = append(mem, Sample{T: ts, V: 44 * mib})
	}
	res := testResolved()
	res.CPU.Quantile, res.CPU.Margin = 0.95, 0.25
	in := RecommendInput{
		Workload:  k8s.Workload{Namespace: "ns", Kind: "Deployment", Name: "busy", Replicas: 1},
		Container: k8s.Container{Name: "app", CPURequest: 0.02, CPULimit: 0.05, MemRequest: 64 * mib, MemLimit: 256 * mib},
		CPU:       cpu, Mem: mem, Policy: res, HalfLifeSec: 600, Now: now, MinSamples: 2,
		LiveCPULimit: live, CPULimitGrowth: 1.5, CPUCensorMaxFactor: 8,
		CPUCensorAnchor: 0.05, // the live limit when the engine first saw the container censored
	}
	if mut != nil {
		mut(&in)
	}
	return in
}

// A container that always uses whatever limit it has can never be measured: each
// raise is eaten at once. After CPUCensorMaxSteps raises with usage still pinned at
// the new limit the limit stops growing and the container is marked greedy.
func TestCPUGreedyStopsAfterMaxSteps(t *testing.T) {
	// Live limit after k raises of the 50m template limit at x1.5, rounded up to 1m.
	steps := []float64{0.05, 0.075, 0.113, 0.170}
	for k, live := range steps {
		r := Recommend(greedyInput(live, live*1.02, nil))
		if !r.Signals.CPUCensored {
			t.Fatalf("step %d: not censored: %+v", k, r.Signals)
		}
		raised := r.Recommended.CPULimit > live*1.01
		if k < 3 && (!raised || r.Signals.CPUGreedy) {
			t.Fatalf("step %d (live %v): limit %v, greedy=%v; want a further raise", k, live, r.Recommended.CPULimit, r.Signals.CPUGreedy)
		}
		if k == 3 {
			if raised || !r.Signals.CPUGreedy || r.Signals.CPUCensorSteps != 3 {
				t.Fatalf("step 3 (live %v): limit %v greedy=%v steps=%d; want parked at the live limit", live, r.Recommended.CPULimit, r.Signals.CPUGreedy, r.Signals.CPUCensorSteps)
			}
			if r.Recommended.CPURequest < live-0.001 {
				t.Fatalf("request %v dropped below the live limit %v", r.Recommended.CPURequest, live)
			}
		}
	}
}

func TestCPUGreedyMaxStepsIsConfigurable(t *testing.T) {
	live := 0.170 // three raises in
	if r := Recommend(greedyInput(live, live*1.02, func(in *RecommendInput) { in.CPUCensorMaxSteps = 5 })); r.Signals.CPUGreedy || r.Recommended.CPULimit <= live*1.01 {
		t.Fatalf("maxSteps=5 parked after 3 raises: %+v limit %v", r.Signals, r.Recommended.CPULimit)
	}
	if r := Recommend(greedyInput(0.075, 0.0765, func(in *RecommendInput) { in.CPUCensorMaxSteps = 1 })); !r.Signals.CPUGreedy {
		t.Fatalf("maxSteps=1 did not park after one raise: %+v", r.Signals)
	}
}

// The step count is measured from CPUCensorAnchor when the engine supplies one,
// so a Rollout flow (the template follows the live limit, hiding the raises) is
// counted too.
func TestCPUGreedyAnchor(t *testing.T) {
	live := 0.170
	in := greedyInput(live, live*1.02, func(in *RecommendInput) {
		in.Container.CPULimit = live // template follows the live limit
		in.CPUCensorAnchor = 0
	})
	if r := Recommend(in); r.Signals.CPUGreedy || r.Signals.CPUCensorSteps != 0 {
		t.Fatalf("no anchor: no run of raises is being counted, must not park: %+v", r.Signals)
	}
	in.CPUCensorAnchor = 0.05
	if r := Recommend(in); !r.Signals.CPUGreedy || r.Recommended.CPULimit > live*1.01 {
		t.Fatalf("anchored at 50m: want parked, got %+v limit %v", r.Signals, r.Recommended.CPULimit)
	}
}

// Once usage falls below 80% of the limit the container is released: the signal
// says so (the engine then re-anchors, allowing new raises later).
func TestCPUGreedyRelease(t *testing.T) {
	live := 0.170
	for _, c := range []struct {
		usage       float64
		wantRelease bool
		wantGreedy  bool
	}{{live * 0.5, true, false}, {live * 0.85, false, false}, {live * 1.02, false, true}} {
		r := Recommend(greedyInput(live, c.usage, nil))
		if r.Signals.CPUGreedyRelease != c.wantRelease || r.Signals.CPUGreedy != c.wantGreedy {
			t.Errorf("usage %.0f%% of the limit: release=%v greedy=%v, want release=%v greedy=%v", 100*c.usage/live, r.Signals.CPUGreedyRelease, r.Signals.CPUGreedy, c.wantRelease, c.wantGreedy)
		}
	}
}

// The engine anchors a run of raises at the live limit when it first sees the
// container censored, and forgets the anchor once the container is released.
func TestEngineTracksCensorAnchor(t *testing.T) {
	e := &Engine{}
	const key = "ns/Deployment/busy/app"
	censored := Recommendation{Signals: Signals{CPUCensored: true}}
	e.trackCensor(key, censored, 0.05)
	if got := e.censorAnchorOf(key); got != 0.05 {
		t.Fatalf("anchor = %v, want the live limit 0.05", got)
	}
	e.trackCensor(key, censored, 0.075) // later raises do not move it
	if got := e.censorAnchorOf(key); got != 0.05 {
		t.Fatalf("anchor moved to %v", got)
	}
	e.trackCensor(key, Recommendation{}, 0.17) // not censored, not released: kept
	if got := e.censorAnchorOf(key); got != 0.05 {
		t.Fatalf("anchor lost without a release: %v", got)
	}
	e.trackCensor(key, Recommendation{Signals: Signals{CPUGreedyRelease: true}}, 0.17)
	if got := e.censorAnchorOf(key); got != 0 {
		t.Fatalf("anchor kept after the release: %v", got)
	}
	e.trackCensor(key, censored, 0.17) // a new episode starts from where the limit is now
	if got := e.censorAnchorOf(key); got != 0.17 {
		t.Fatalf("new episode anchor = %v, want 0.17", got)
	}
}
