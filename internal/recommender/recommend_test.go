package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
)

const mib = 1024 * 1024

func testEngine(minSamples int) *Engine {
	cfg := config.Config{MinSamples: minSamples, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004, PricingMode: "static"}
	return NewEngine(cfg, nil, nil, nil, pricing.New(cfg, nil))
}

func testResolved() policy.Resolved {
	return policy.Resolved{
		Mode:      policy.ModeRecommend,
		CPU:       policy.ResourceProfile{Quantile: 0.9, Margin: 0.15},
		Mem:       policy.ResourceProfile{Quantile: 0.95, Margin: 0.15, LimitFactor: 1.0},
		Season:    "off",
		Direction: policy.DirBoth,
	}
}

// series builds one pod's series: n samples every `step` seconds from t0.
func series(t0, step float64, n int, v func(i int) float64) []Sample {
	out := make([]Sample, n)
	for i := range out {
		out[i] = Sample{T: t0 + float64(i)*step, V: v(i)}
	}
	return out
}

func constant(v float64) func(int) float64 { return func(int) float64 { return v } }

var testWL = k8s.Workload{Namespace: "ns", Kind: "Deployment", Name: "web", Replicas: 2}

// Bug 2: samples arrive concatenated per pod (not time-ordered). With two
// replicas at steady but DIFFERENT levels, the boundary between pod A and pod B
// looks like a level shift, so changePointIndex trimmed the series to pod B only
// and the recommendation ignored the busier replica.
func TestRecommendChangePointAcrossPods(t *testing.T) {
	now := float64(time.Now().Unix())
	t0 := now - 200*300
	cases := []struct {
		name    string
		cpu     []Sample
		minCPU  float64 // recommended CPU request must be >= this
		maxCPU  float64 // ... and <= this
		comment string
	}{
		{
			name:   "two steady replicas at different levels: no false change point",
			cpu:    append(series(t0, 300, 200, constant(1.0)), series(t0, 300, 200, constant(0.3))...),
			minCPU: 1.0, maxCPU: 1.2,
		},
		{
			name:   "same, pod order reversed",
			cpu:    append(series(t0, 300, 200, constant(0.3)), series(t0, 300, 200, constant(1.0))...),
			minCPU: 1.0, maxCPU: 1.2,
		},
		{
			name: "real regime shift (both pods drop at the same time) is still detected",
			cpu: append(
				series(t0, 300, 200, func(i int) float64 { return map[bool]float64{true: 1.0, false: 0.2}[i < 100] }),
				series(t0, 300, 200, func(i int) float64 { return map[bool]float64{true: 1.0, false: 0.2}[i < 100] })...),
			minCPU: 0.2, maxCPU: 0.3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := testEngine(1)
			mem := series(t0, 300, 200, constant(100*mib))
			rec := e.recommend(testWL, k8s.Container{Name: "app", CPURequest: 2, MemRequest: 512 * mib}, c.cpu, mem, 0, testResolved(), 0)
			got := rec.Recommended.CPURequest
			if got < c.minCPU || got > c.maxCPU {
				t.Fatalf("recommended cpu = %v, want in [%v, %v]", got, c.minCPU, c.maxCPU)
			}
		})
	}
}

// Bug 3: Confident counted samples pooled across replicas, so a 40-replica
// workload observed for ONE scrape (40 samples, 1 timestamp) passed a
// MinSamples=30 gate. Confidence must require window coverage (distinct time
// points), not replica count.
func TestRecommendConfidenceNeedsTimeCoverage(t *testing.T) {
	now := float64(time.Now().Unix())
	var manyPodsOneScrape []Sample
	for i := 0; i < 40; i++ {
		manyPodsOneScrape = append(manyPodsOneScrape, Sample{T: now, V: 0.5})
	}
	var fewPodsShort []Sample // 3 pods × 10 scrapes = 30 samples, 10 time points
	for p := 0; p < 3; p++ {
		fewPodsShort = append(fewPodsShort, series(now-3000, 300, 10, constant(0.5))...)
	}
	cases := []struct {
		name    string
		samples []Sample
		want    bool
	}{
		{"40 replicas, single scrape", manyPodsOneScrape, false},
		{"3 replicas × 10 scrapes", fewPodsShort, false},
		{"1 replica × 30 scrapes", series(now-9000, 300, 30, constant(0.5)), true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := testEngine(30)
			rec := e.recommend(testWL, k8s.Container{Name: "app"}, c.samples, c.samples, 0, testResolved(), 0)
			if rec.Confident != c.want {
				t.Fatalf("Confident = %v, want %v", rec.Confident, c.want)
			}
		})
	}
}

// Bug 4: MemLimitFactor (default 1.0) imposed a memory limit on containers that
// had none; and a derived limit sat exactly at the observed peak (no margin).
func TestRecommendMemLimit(t *testing.T) {
	now := float64(time.Now().Unix())
	// p95 ~100Mi, a rare 1Gi peak (fewer than 5% of samples).
	mem := series(now-100*300, 300, 100, func(i int) float64 {
		if i == 50 {
			return 1024 * mib
		}
		return 100 * mib
	})
	cpu := series(now-100*300, 300, 100, constant(0.1))
	cases := []struct {
		name       string
		curLimit   float64
		oomFloor   float64
		allowExc   bool
		wantZero   bool
		wantAtLeas float64
		wantAtMost float64
	}{
		{name: "no current limit -> no limit recommended", curLimit: 0, wantZero: true},
		{name: "current limit -> derived limit clears peak with margin", curLimit: 4096 * mib, wantAtLeas: 1024 * mib * 1.15, wantAtMost: 4096 * mib},
		{name: "derived limit still capped at current limit", curLimit: 1024 * mib, wantAtLeas: 1024 * mib, wantAtMost: 1024 * mib},
		// P0-2: an OOM floor never introduces a limit on an unbounded container (it
		// used to: a limit derived from the request could sit below real usage).
		{name: "OOM floor never introduces a limit", curLimit: 0, oomFloor: 2048 * mib, wantZero: true},
		{name: "OOM floor raises an existing limit", curLimit: 1024 * mib, oomFloor: 2048 * mib, wantAtLeas: 2048 * mib, wantAtMost: 2048 * mib},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := testEngine(1)
			res := testResolved()
			res.AllowExceedLimit = c.allowExc
			rec := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 512 * mib, MemLimit: c.curLimit}, cpu, mem, 0, res, c.oomFloor)
			got := rec.Recommended.MemLimit
			if c.wantZero {
				if got != 0 {
					t.Fatalf("MemLimit = %v Mi, want 0 (none imposed)", got/mib)
				}
				return
			}
			if got < c.wantAtLeas || got > c.wantAtMost {
				t.Fatalf("MemLimit = %v Mi, want in [%v, %v] Mi", got/mib, c.wantAtLeas/mib, c.wantAtMost/mib)
			}
			if rec.Recommended.MemRequest > got {
				t.Fatalf("request %v > limit %v", rec.Recommended.MemRequest, got)
			}
		})
	}
}

func TestRecommendClampsAndCaps(t *testing.T) {
	now := float64(time.Now().Unix())
	cpu := series(now-100*300, 300, 100, constant(3))
	mem := series(now-100*300, 300, 100, constant(100*mib))
	cases := []struct {
		name    string
		mut     func(*policy.Resolved)
		c       k8s.Container
		wantCPU float64
	}{
		{"max clamps", func(r *policy.Resolved) { r.CPU.Max = 1 }, k8s.Container{Name: "app"}, 1},
		{"min floors", func(r *policy.Resolved) { r.CPU.Min = 5 }, k8s.Container{Name: "app"}, 5},
		{"capped at current cpu limit", nil, k8s.Container{Name: "app", CPULimit: 2}, 2},
		{"allowExceedLimit lifts the cap", func(r *policy.Resolved) { r.AllowExceedLimit = true }, k8s.Container{Name: "app", CPULimit: 2}, 3.45},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := testResolved()
			if c.mut != nil {
				c.mut(&res)
			}
			rec := testEngine(1).recommend(testWL, c.c, cpu, mem, 0, res, 0)
			if !approx(rec.Recommended.CPURequest, c.wantCPU, 1e-3) {
				t.Fatalf("cpu = %v, want %v", rec.Recommended.CPURequest, c.wantCPU)
			}
		})
	}
}

func TestRecommendHeadroomGuardAndWaste(t *testing.T) {
	now := float64(time.Now().Unix())
	// 90% at 0.1, 10% at 1.0 — p90*1.15 misses the tail; the guard covers it.
	cpu := series(now-100*300, 300, 100, func(i int) float64 {
		if i%10 == 0 {
			return 1.0
		}
		return 0.1
	})
	mem := series(now-100*300, 300, 100, constant(100*mib))
	res := testResolved()
	res.CPU.Quantile = 0.5
	plain := testEngine(1).recommend(testWL, k8s.Container{Name: "app", CPURequest: 2}, cpu, mem, 0, res, 0)
	res.HeadroomGuard = true
	res.HeadroomThreshold = 0.05
	guarded := testEngine(1).recommend(testWL, k8s.Container{Name: "app", CPURequest: 2}, cpu, mem, 0, res, 0)
	if guarded.Recommended.CPURequest < 1.0 || guarded.Signals.HeadroomBreachCPU > 0.05 {
		t.Fatalf("guard: cpu=%v breach=%v, want >=1.0 and <=0.05", guarded.Recommended.CPURequest, guarded.Signals.HeadroomBreachCPU)
	}
	if plain.Signals.HeadroomBreachCPU < 0.09 {
		t.Fatalf("unguarded breach = %v, want ~0.1", plain.Signals.HeadroomBreachCPU)
	}
	// Waste is positive when over-provisioned: (2 - rec) cores × price × 168h × replicas.
	want := (2 - plain.Recommended.CPURequest + (0-plain.Recommended.MemRequest)/gb*0.004/0.031) * 0.031 * 168 * 2
	if !approx(plain.WasteUSDWeek, want, 0.02) {
		t.Fatalf("waste = %v, want ~%v", plain.WasteUSDWeek, want)
	}
}

// Perf: the change-point scan was O(n²) (a fresh mean over both halves per
// candidate split). A 7d window at 5m for a 50-replica group is ~100k samples.
func TestChangePointLinear(t *testing.T) {
	n := 100_000
	s := series(0, 1, n, func(i int) float64 { return float64(i%7) + 1 })
	done := make(chan struct{})
	go func() { _ = currentRegime(s, 0.5); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("currentRegime on 100k samples took >5s (quadratic)")
	}
}

func BenchmarkCurrentRegime(b *testing.B) {
	s := series(0, 1, 20_000, func(i int) float64 { return float64(i%7) + 1 })
	for i := 0; i < b.N; i++ {
		_ = currentRegime(s, 0.5)
	}
}
