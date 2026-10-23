package reconciler

import (
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func TestRollingGate(t *testing.T) {
	ok := RollingState{OthersReady: true}
	cases := []struct {
		name string
		st   RollingState
		want RollingVerdict
	}{
		{"all others Ready, no PDB: one pod", ok, RollingGo},
		{"another replica not Ready", RollingState{OthersReady: false}, RollingWait},
		{"controller short of its desired count", RollingState{OthersReady: true, ControllerShort: true}, RollingWait},
		{"no PDB: the second restart in a cycle waits for the budget (1)", RollingState{OthersReady: true, ReadyRestarted: 1}, RollingOverBudget},
		{"PDB allowing 2: two restarts go", RollingState{OthersReady: true, PDBProtected: true, PDBAllowed: 2, ReadyRestarted: 1}, RollingGo},
		{"PDB allowing 2: the third waits", RollingState{OthersReady: true, PDBProtected: true, PDBAllowed: 2, ReadyRestarted: 2}, RollingOverBudget},
		{"PDB allowing 0: nothing goes", RollingState{OthersReady: true, PDBProtected: true, PDBAllowed: 0}, RollingOverBudget},
	}
	for _, c := range cases {
		if got := RollingGate(c.st); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if RollingBudget(false, 5) != 1 || RollingBudget(true, 3) != 3 {
		t.Fatal("budget: one without a PDB, disruptionsAllowed with one")
	}
}

// PlanRollout is the template-level decision rollout() applies, extracted so the
// backtest replays it: HPA hold, direction, drift tolerance, never-shrink limits.
func TestPlanRollout(t *testing.T) {
	cur := recommender.Resources{CPURequest: 1, CPULimit: 2, MemRequest: 512 * mib, MemLimit: 1024 * mib}
	mk := func(rec recommender.Resources, mut ...func(*recommender.Entry)) recommender.Entry {
		e := recommender.Entry{
			Rec:       recommender.Recommendation{Confident: true, Current: cur, Recommended: rec},
			Direction: policy.DirBoth,
		}
		for _, m := range mut {
			m(&e)
		}
		return e
	}
	if _, ok := PlanRollout(mk(recommender.Resources{CPURequest: 0.95, MemRequest: 500 * mib})); ok {
		t.Fatal("a 5% drift must not roll")
	}
	p, ok := PlanRollout(mk(recommender.Resources{CPURequest: 0.5, CPULimit: 1, MemRequest: 256 * mib, MemLimit: 512 * mib}))
	if !ok || p.TargetCPU != 0.5 || p.TargetMem != 256*mib || p.Up {
		t.Fatalf("shrink: %+v %v", p, ok)
	}
	if p.CPULimit != 2 || p.MemLimit != 1024*mib {
		t.Fatalf("limits are never shrunk by a rollout: cpu %v mem %v", p.CPULimit, p.MemLimit/mib)
	}
	p, ok = PlanRollout(mk(recommender.Resources{CPURequest: 3, MemRequest: 256 * mib}))
	if !ok || !p.Up || p.CPULimit != 3 {
		t.Fatalf("grow past the limit lifts the limit to the request: %+v %v", p, ok)
	}
	p, _ = PlanRollout(mk(recommender.Resources{CPURequest: 3, MemRequest: 256 * mib}, func(e *recommender.Entry) { e.Rec.CPUHeldByHPA = true }))
	if p.TargetCPU != 1 || !p.CPUHeld {
		t.Fatalf("HPA-held CPU must stay at the template value: %+v", p)
	}
	if _, ok := PlanRollout(mk(recommender.Resources{CPURequest: 3, MemRequest: 700 * mib}, func(e *recommender.Entry) { e.Direction = policy.DirDown })); ok {
		t.Fatal("shrink-only: a growth must not roll")
	}
}
