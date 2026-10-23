package recommender

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

const day = 86400.0

// burstySeries: base level with short (≤30 min) random bursts — no periodicity.
func burstySeries(t0 float64, days int, seed uint64) []Sample {
	rng := rand.New(rand.NewPCG(seed, 7))
	n := days * 288
	out := make([]Sample, n)
	left := 0
	amp := 0.0
	for i := range out {
		if left == 0 && rng.Float64() < 6.0/288 { // ~6 bursts/day
			left = 1 + rng.IntN(6)
			amp = 3 + 5*rng.Float64()
		}
		v := 0.12 * (1 + 0.1*rng.NormFloat64())
		if left > 0 {
			v += 0.12 * amp
			left--
		}
		out[i] = Sample{T: t0 + float64(i)*300, V: v}
	}
	return out
}

// diurnalSeries: clean daily cycle (peak 16h UTC) with small noise.
func diurnalSeries(t0 float64, days int) []Sample {
	rng := rand.New(rand.NewPCG(1, 2))
	n := days * 288
	out := make([]Sample, n)
	for i := range out {
		t := t0 + float64(i)*300
		h := math.Mod(t, day) / 3600
		load := 0.5 - 0.5*math.Cos(2*math.Pi*(h-4)/24)
		out[i] = Sample{T: t, V: (0.1 + 0.4*load) * (1 + 0.05*rng.NormFloat64())}
	}
	return out
}

func TestMedianFilterRemovesPulsesKeepsSteps(t *testing.T) {
	s := series(0, 300, 100, func(i int) float64 {
		switch {
		case i >= 20 && i < 24: // 20-min pulse
			return 5
		case i >= 60: // sustained step
			return 2
		}
		return 1
	})
	f := medianFilter(s, cpFilterHalfWidth)
	for i := 15; i < 30; i++ {
		if f[i].V != 1 {
			t.Fatalf("pulse survived the filter at %d: %v", i, f[i].V)
		}
	}
	if f[50].V != 1 || f[70].V != 2 || f[99].V != 2 {
		t.Fatalf("step not preserved: f[50]=%v f[70]=%v f[99]=%v", f[50].V, f[70].V, f[99].V)
	}
	if medianFilter(nil, 3) != nil || len(medianFilter(s, 0)) != len(s) {
		t.Fatal("edge cases")
	}
}

// The bug the backtest found: a burst raised the mean of the recent segment by
// >50% and the window collapsed onto it (quantile 0.15 → 0.57).
func TestCurrentRegimeIgnoresBursts(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		s := burstySeries(0, 3, seed)
		if got := currentRegime(s, 0.5); len(got) != len(s) {
			t.Fatalf("seed %d: bursts trimmed the window to %d of %d samples", seed, len(got), len(s))
		}
	}
	// A real, sustained shift is still cut.
	step := series(0, 300, 576, func(i int) float64 {
		if i < 400 {
			return 0.2
		}
		return 0.6
	})
	got := currentRegime(step, 0.5)
	if len(got) == len(step) || got[0].T < step[400].T-3600 {
		t.Fatalf("sustained step not detected: kept %d of %d (from t=%v)", len(got), len(step), got[0].T)
	}
}

func TestEffectiveSeason(t *testing.T) {
	diurnal := diurnalSeries(0, 7)
	if got := effectiveSeason(diurnal, "daily"); got != "daily" {
		t.Fatalf("clean daily cycle: %q, want daily", got)
	}
	if got := effectiveSeason(diurnal, ""); got != "daily" {
		t.Fatalf("default season on a daily cycle: %q", got)
	}
	// 7d window: no week-lag pairs → falls back to the daily-lag test.
	if got := effectiveSeason(diurnal, "weekly"); got != "weekly" {
		t.Fatalf("weekly on a regular 7d series: %q", got)
	}
	if got := effectiveSeason(diurnal, "both"); got != "both" {
		t.Fatalf("both: %q", got)
	}
	if got := effectiveSeason(diurnal, "off"); got != "off" {
		t.Fatalf("off must stay off: %q", got)
	}
	for seed := uint64(1); seed <= 20; seed++ {
		if got := effectiveSeason(burstySeries(0, 7, seed), "daily"); got != "off" {
			t.Fatalf("seed %d: random bursts accepted as daily seasonality (%q)", seed, got)
		}
	}
	// Less than a day of lag pairs: not enough evidence.
	if got := effectiveSeason(diurnalSeries(0, 1), "daily"); got != "off" {
		t.Fatalf("one day of history: %q, want off", got)
	}
	// Flat series: nothing to forecast.
	if got := effectiveSeason(series(0, 300, 3*288, constant(1)), "daily"); got != "off" {
		t.Fatalf("flat: %q", got)
	}
}

func TestStabilizer(t *testing.T) {
	var s stabilizer
	t0 := time.Unix(1_000_000, 0)
	w := time.Hour
	s.record("k", t0, w, 1.0, 100)
	s.record("k", t0.Add(20*time.Minute), w, 0.4, 300)
	if c, m := s.floor("k", t0.Add(30*time.Minute), w); c != 1.0 || m != 300 {
		t.Fatalf("floor = %v/%v, want max of the window 1.0/300", c, m)
	}
	// The 1.0 point ages out after w.
	if c, _ := s.floor("k", t0.Add(61*time.Minute), w); c != 0.4 {
		t.Fatalf("floor after window = %v, want 0.4", c)
	}
	if c, m := s.floor("k", t0, 0); c != 0 || m != 0 {
		t.Fatal("zero window must disable stabilization")
	}
	if c, _ := s.floor("other", t0, w); c != 0 {
		t.Fatal("unknown key")
	}
	s.record("gone", t0, w, 5, 5)
	s.retain(map[string]bool{"k": true})
	if c, _ := s.floor("gone", t0, w); c != 0 {
		t.Fatal("retain must drop containers no longer in scope")
	}
	s.record("k", t0.Add(2*time.Hour), w, 0.2, 50) // prunes the old points
	if n := len(s.hist["k"]); n != 1 {
		t.Fatalf("history not pruned: %d points", n)
	}
}

func TestRecommendStabilizationFloor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cpu := series(float64(now.Unix())-100*300, 300, 100, constant(0.2))
	mem := series(float64(now.Unix())-100*300, 300, 100, constant(100*mib))
	in := RecommendInput{
		Workload: testWL, Container: k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib, MemLimit: 600 * mib},
		CPU: cpu, Mem: mem, Policy: testResolved(), Now: now, MinSamples: 1,
	}
	raw := Recommend(in)
	if raw.Signals.Stabilized || raw.Recommended.CPURequest != RoundCPUCores(raw.Signals.RawCPURequest) {
		t.Fatalf("no floor: must not be stabilized: %+v", raw.Signals)
	}
	in.StabilizeCPU, in.StabilizeMem = 0.5, 400*mib
	got := Recommend(in)
	if !got.Signals.Stabilized || got.Recommended.CPURequest != 0.5 || got.Recommended.MemRequest != 400*mib {
		t.Fatalf("floor not applied: %+v", got.Recommended)
	}
	if got.Signals.RawCPURequest != raw.Signals.RawCPURequest {
		t.Fatal("raw value must not include the stabilization floor (else the window never decays)")
	}
	// The health cap (current limit) still wins over the floor.
	in.StabilizeMem = 900 * mib
	if got := Recommend(in); got.Recommended.MemRequest > 600*mib {
		t.Fatalf("stabilization exceeded the current limit: %v", got.Recommended.MemRequest/mib)
	}
	// A floor below the raw value is a no-op (scale-up passes at once).
	in.StabilizeCPU, in.StabilizeMem = 0.01, 0
	if got := Recommend(in); got.Signals.Stabilized || got.Recommended.CPURequest != raw.Recommended.CPURequest {
		t.Fatal("a lower floor must not change the recommendation")
	}
}

// Recommend is a pure function of its input — in particular of Now (the
// seasonal-forecast target), not the wall clock.
func TestRecommendUsesInputClock(t *testing.T) {
	s := diurnalSeries(0, 7)
	res := testResolved()
	res.Season = "daily"
	in := RecommendInput{Workload: testWL, Container: k8s.Container{Name: "app"}, CPU: s, Mem: s, Policy: res, MinSamples: 1}
	in.Now = time.Unix(int64(7*day+14*3600), 0) // forecast for 15h: near the peak
	peak := Recommend(in)
	in.Now = time.Unix(int64(7*day+3*3600), 0) // forecast for 04h: the trough
	trough := Recommend(in)
	if peak.Signals.CPUForecast == nil || trough.Signals.CPUForecast == nil || *peak.Signals.CPUForecast <= *trough.Signals.CPUForecast {
		t.Fatalf("forecast must follow in.Now: peak=%v trough=%v", peak.Signals.CPUForecast, trough.Signals.CPUForecast)
	}
	if peak.Signals.CPUSeason != "daily" {
		t.Fatalf("CPUSeason = %q", peak.Signals.CPUSeason)
	}
	again := Recommend(in)
	if again.Recommended != trough.Recommended {
		t.Fatal("same input, different output")
	}
}

// With random bursts the forecast is gated off, so the recommendation does not
// swing with the hour of day (the bursty flapping found by the backtest).
func TestRecommendBurstyIgnoresForecast(t *testing.T) {
	s := burstySeries(0, 7, 3)
	res := testResolved()
	res.Season = "daily"
	var lo, hi float64 = math.Inf(1), 0
	for h := 0; h < 24; h++ {
		in := RecommendInput{Workload: testWL, Container: k8s.Container{Name: "app"}, CPU: s, Mem: s, Policy: res, MinSamples: 1,
			Now: time.Unix(int64(7*day)+int64(h)*3600, 0)}
		r := Recommend(in)
		if r.Signals.CPUForecast != nil {
			t.Fatalf("h=%d: forecast used on non-periodic bursts", h)
		}
		lo, hi = math.Min(lo, r.Recommended.CPURequest), math.Max(hi, r.Recommended.CPURequest)
	}
	if hi != lo {
		t.Fatalf("recommendation swings with the hour of day: %v..%v", lo, hi)
	}
}

// The engine clock drives the OOM-floor TTL (replay in simulated time).
func TestEngineClockDrivesOOMTTL(t *testing.T) {
	e := testEngine(1)
	e.cfg.OOMGrowthFactor, e.cfg.OOMFloorTTL = 1.5, 24*time.Hour
	sim := time.Unix(1_000_000, 0)
	e.SetClock(func() time.Time { return sim })
	e.learnOOM(map[string]k8s.OOMObservation{"k": {MemBytes: 1000, At: sim}})
	if got := e.oomFloorFor("k"); got != 1500 {
		t.Fatalf("floor = %v", got)
	}
	sim = sim.Add(25 * time.Hour)
	if got := e.oomFloorFor("k"); got != 0 {
		t.Fatalf("floor must expire by the ENGINE clock, got %v", got)
	}
	e.SetClock(nil)
	if d := time.Since(e.clock()); d < 0 || d > time.Minute {
		t.Fatal("nil clock must fall back to time.Now")
	}
}
