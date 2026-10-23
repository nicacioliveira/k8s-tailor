package backtest

import (
	"testing"
)

// Regression guard for the flapping the backtest exposed (docs/BACKTEST.md,
// "bursty"): with random bursts the engine used to resize CPU between ~160m
// and ~1 core almost every hour (11–14 changes/day). With the seasonal gate,
// the burst-robust change-point and scale-down stabilization it must stay
// within the 3 changes/day target, while the periodic scenario keeps its
// forecast advantage (risk well below the reference ~2.3% of VPA).
func TestEngineDoesNotFlapOnBursts(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 14 days through the real engine")
	}
	cfg := EngineConfig{Base: testEngineCfg().Base, RefreshEverySec: 3600}
	opts := Options{WarmupSec: 2 * 86400, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
	for _, c := range []struct {
		scenario   string
		maxChanges float64
		maxRiskPct float64
	}{
		{"bursty", 3, 100},
		{"diurnal-weekly", 1.5, 1.5},
	} {
		tr, err := Generate(c.scenario, 1, 14, 300)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Run(tr, []Factory{NewStatic, NewEngine(cfg)}, opts)
		if err != nil {
			t.Fatal(err)
		}
		m := metricsOf(t, r, "k8s-tailor")
		if m.ChangesPerDay > c.maxChanges {
			t.Errorf("%s: %.2f changes/day, want <= %v", c.scenario, m.ChangesPerDay, c.maxChanges)
		}
		if m.CPUOverRequestPct > c.maxRiskPct {
			t.Errorf("%s: CPU>request %.2f%%, want <= %v%%", c.scenario, m.CPUOverRequestPct, c.maxRiskPct)
		}
	}
}

// Censored CPU (Istio finding 1, docs/TECHNICAL.md "Censored CPU"): a proxy
// throttled at a 50m CPU limit used to be measured at ~50m forever — the engine
// recommended ~the limit and the drift gate swallowed the rest, so it never
// escaped (576 of 576 pod-hours over the limit with seed 42). It must now raise
// the limit by steps until the demand fits, and the replay must cap observed
// usage at the applied CPU limit for every strategy.
func TestEngineEscapesThrottledCap(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 14 days through the real engine")
	}
	cfg := EngineConfig{Base: testEngineCfg().Base, RefreshEverySec: 3600}
	opts := Options{WarmupSec: 2 * 86400, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
	tr, err := Generate("throttled-cap", 42, 14, 300)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Run(tr, []Factory{NewStatic, NewEngine(cfg)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	st, m := metricsOf(t, r, "static"), metricsOf(t, r, "k8s-tailor")
	if st.CPUOverLimitHours < 500 {
		t.Fatalf("static over-limit = %.1f pod·h: the replay no longer models the cap", st.CPUOverLimitHours)
	}
	if m.CPUOverLimitHours > 0.05*st.CPUOverLimitHours {
		t.Errorf("engine over-limit = %.1f pod·h (static %.1f): still stuck under the CPU limit", m.CPUOverLimitHours, st.CPUOverLimitHours)
	}
	if m.Final.CPULimit <= 0.28 || m.Final.CPURequest > m.Final.CPULimit {
		t.Errorf("final = %+v: want a CPU limit above the 280m peak demand and request <= limit", m.Final)
	}
	if m.ChangesPerDay > 3 {
		t.Errorf("%.2f changes/day, want <= 3", m.ChangesPerDay)
	}
}

// A CPU-greedy container (a busy loop: it uses every core it is given) eats each
// limit raise at once. Before the convergence rule the engine kept growing the
// limit to its brake (template limit x CPU_CENSOR_MAX_FACTOR = 1600m with seed 42);
// it must now park at the third step (200m x 1.5^3 = 675m) and stay there, while
// throttled-cap, whose demand really ends, keeps escaping (TestEngineEscapesThrottledCap).
func TestEngineParksCPUGreedy(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 14 days through the real engine")
	}
	cfg := EngineConfig{Base: testEngineCfg().Base, RefreshEverySec: 3600}
	opts := Options{WarmupSec: 2 * 86400, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
	tr, err := Generate("cpu-greedy", 42, 14, 300)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Run(tr, []Factory{NewStatic, NewEngine(cfg)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	m := metricsOf(t, r, "k8s-tailor")
	if m.Final.CPULimit < 0.6 || m.Final.CPULimit > 0.7 {
		t.Errorf("final CPU limit = %v, want parked at the third step (~675m)", m.Final.CPULimit)
	}
	for _, c := range m.ChangeLog {
		if c.To.CPULimit > 0.7 {
			t.Errorf("limit raised to %v at %d: past the third step", c.To.CPULimit, c.T)
		}
	}
}

// autoQuantile (opt-in, AUTO_QUANTILE): on the bursty scenario the engine raises
// the CPU quantile to 0.95 once the burstiness index (p99 / p90) passes the
// threshold. Seed 42, 14 days: CPU>request 7.20% -> 4.78% against KRR's 3.41%
// (docs/BACKTEST.md), at a cost close to KRR's. The default (off) must reproduce
// the run without the feature exactly.
func TestEngineAutoQuantileOnBursty(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 14 days through the real engine")
	}
	opts := Options{WarmupSec: 2 * 86400, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
	tr, err := Generate("bursty", 42, 14, 300)
	if err != nil {
		t.Fatal(err)
	}
	run := func(auto bool) (eng, krr Metrics) {
		cfg := EngineConfig{Base: testEngineCfg().Base, RefreshEverySec: 3600}
		cfg.Base.AutoQuantile = auto
		r, err := Run(tr, []Factory{NewStatic, NewKRR(DefaultKRR()), NewEngine(cfg)}, opts)
		if err != nil {
			t.Fatal(err)
		}
		return metricsOf(t, r, "k8s-tailor"), metricsOf(t, r, "krr")
	}
	off, krr := run(false)
	on, _ := run(true)
	if on.CPUOverRequestPct > krr.CPUOverRequestPct+2 {
		t.Errorf("autoQuantile on: CPU>request %.2f%%, want <= KRR %.2f%% + 2pp", on.CPUOverRequestPct, krr.CPUOverRequestPct)
	}
	if on.CPUOverRequestPct >= off.CPUOverRequestPct {
		t.Errorf("autoQuantile did not lower the risk: %.2f%% vs %.2f%% off", on.CPUOverRequestPct, off.CPUOverRequestPct)
	}
	if on.CostRelative > krr.CostRelative*1.05 {
		t.Errorf("autoQuantile cost %.3f above KRR's %.3f", on.CostRelative, krr.CostRelative)
	}
	if off.CPUOverRequestPct < 7 {
		t.Errorf("default run changed: CPU>request %.2f%%, want the 7.20%% of the baseline", off.CPUOverRequestPct)
	}
}
