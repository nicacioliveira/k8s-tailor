package backtest

import (
	"testing"
)

// retarget changes its target once, at tick `at`, to `to`.
type retarget struct {
	at int
	to Resources
}

func (retarget) Name() string { return "retarget" }
func (retarget) Close()       {}
func (s retarget) Observe(tk Tick, applied Resources) (Resources, error) {
	if tk.I >= s.at {
		return s.to, nil
	}
	return applied, nil
}

// A controller-style strategy that only names the target is rolled replica by
// replica behind the PDB: one restart per tick with a budget of one, never two
// replicas not Ready at once, one restart per replica per change.
func TestRollingPacesRestartsBehindThePDB(t *testing.T) {
	init := Resources{CPURequest: 1, MemRequest: 512 * mib, MemLimit: 512 * mib}
	tr := constTrace(3, 200, 0.1, 100*mib, init)
	tr.Disruption = &Disruption{PDBMinAvailable: 2, ReadySteps: 2}
	to := Resources{CPURequest: 0.5, MemRequest: 256 * mib, MemLimit: 512 * mib}
	r, err := Run(tr, []Factory{NewStatic, func(*Trace) (Strategy, error) { return retarget{at: 50, to: to}, nil }}, Options{WarmupSec: 3600, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004})
	if err != nil {
		t.Fatal(err)
	}
	m := metricsOf(t, r, "retarget")
	if m.Restarts != 3 || m.Changes != 1 || m.Final != to {
		t.Fatalf("restarts=%d changes=%d final=%+v, want 3 restarts for the one change, final %+v", m.Restarts, m.Changes, m.Final, to)
	}
	if m.MaxNotReady != 1 {
		t.Fatalf("max not Ready = %d, want 1 (3 replicas, minAvailable 2)", m.MaxNotReady)
	}
	if st := metricsOf(t, r, "static"); st.Restarts != 0 || st.MaxNotReady != 0 {
		t.Fatalf("static restarted something: %+v", st)
	}
}

// A PDB that keeps every replica Ready leaves no budget: nobody is restarted, and
// the resources stay put (the strategy's wish is never forced through).
func TestRollingNeverViolatesAFullPDB(t *testing.T) {
	init := Resources{CPURequest: 1, MemRequest: 512 * mib, MemLimit: 512 * mib}
	tr := constTrace(3, 200, 0.1, 100*mib, init)
	tr.Disruption = &Disruption{PDBMinAvailable: 3, ReadySteps: 1}
	to := Resources{CPURequest: 0.5, MemRequest: 256 * mib, MemLimit: 512 * mib}
	r, err := Run(tr, []Factory{NewStatic, func(*Trace) (Strategy, error) { return retarget{at: 50, to: to}, nil }}, Options{WarmupSec: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if m := metricsOf(t, r, "retarget"); m.Restarts != 0 || m.MaxNotReady != 0 {
		t.Fatalf("restarted against a PDB with no budget: %+v", m)
	}
}

// On the restart-rolling scenario every strategy honors the PDB, restarts exactly
// once per replica per change (a rolling in flight at the end may be partial), the
// in-place engine restarts less than the reference strategies, and the rollout
// path keeps its 10 minute floor.
func TestRestartRollingScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 14 days through the real engine")
	}
	opts := Options{WarmupSec: 2 * 86400, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
	cfg := EngineConfig{Base: testEngineCfg().Base, RefreshEverySec: 3600}
	for _, seed := range []uint64{42, 1, 2} {
		tr, err := Generate("restart-rolling", seed, 14, 300)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Run(tr, []Factory{NewStatic, NewKRR(DefaultKRR()), NewVPA(DefaultVPA()), NewEngine(cfg), NewEngineRollout(cfg)}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Metrics) != 5 || !r.Disruptive {
			t.Fatalf("seed %d: %d strategies, disruptive=%v, want 5 / true", seed, len(r.Metrics), r.Disruptive)
		}
		for _, m := range r.Metrics {
			if m.MaxNotReady > 1 {
				t.Errorf("seed %d %s: %d replicas not Ready at once, the PDB allows 1", seed, m.Strategy, m.MaxNotReady)
			}
			if m.Restarts > 3*m.Changes || m.Restarts < 3*(m.Changes-1) {
				t.Errorf("seed %d %s: %d restarts for %d changes of 3 replicas", seed, m.Strategy, m.Restarts, m.Changes)
			}
		}
		eng, krr, vpa := metricsOf(t, r, "k8s-tailor"), metricsOf(t, r, "krr"), metricsOf(t, r, "vpa")
		if eng.RestartsPerDay >= krr.RestartsPerDay || eng.RestartsPerDay >= vpa.RestartsPerDay {
			t.Errorf("seed %d: engine %.2f restarts/day, krr %.2f, vpa %.2f: expected the engine to restart least", seed, eng.RestartsPerDay, krr.RestartsPerDay, vpa.RestartsPerDay)
		}
		ro := metricsOf(t, r, "k8s-tailor-rollout")
		for i := 1; i < len(ro.ChangeLog); i++ {
			if gap := ro.ChangeLog[i].T - ro.ChangeLog[i-1].T; gap < 600 {
				t.Errorf("seed %d rollout: two rollouts %ds apart, floor is 600s", seed, gap)
			}
		}
	}
}

// The rollout strategy is skipped (no row) on a trace whose resizes restart nothing,
// so the existing scenarios' tables do not change.
func TestRolloutStrategySkippedWithoutDisruption(t *testing.T) {
	init := Resources{CPURequest: 1, MemRequest: 512 * mib}
	tr := constTrace(2, 100, 0.1, 100*mib, init)
	cfg := EngineConfig{Base: testEngineCfg().Base, RefreshEverySec: 3600}
	r, err := Run(tr, []Factory{NewStatic, NewEngineRollout(cfg)}, Options{WarmupSec: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Metrics) != 1 || r.Disruptive {
		t.Fatalf("metrics %v disruptive=%v, want only static", r.Metrics, r.Disruptive)
	}
}
