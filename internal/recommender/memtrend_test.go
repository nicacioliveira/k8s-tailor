package recommender

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// noisy builds a series around level(i) with deterministic relative noise.
func noisy(t0 float64, n int, seed uint64, rel float64, level func(i int) float64) []Sample {
	rng := rand.New(rand.NewPCG(seed, 1))
	return series(t0, 300, n, func(i int) float64 { return level(i) * (1 + rel*rng.NormFloat64()) })
}

func memTrendOf(t *testing.T, mem []Sample) *float64 {
	t.Helper()
	now := mem[len(mem)-1].T
	rec := Recommend(RecommendInput{
		Workload:  k8s.Workload{Namespace: "ns", Kind: "Deployment", Name: "proxy", Replicas: 1},
		Container: k8s.Container{Name: "istio-proxy", CPURequest: 0.02, MemRequest: 64 * mib, MemLimit: 256 * mib},
		CPU:       series(mem[0].T, 300, len(mem), constant(0.01)), Mem: mem,
		Policy: testResolved(), HalfLifeSec: 600, Now: time.Unix(int64(now), 0), MinSamples: 2,
	})
	return rec.Signals.MemTrendBytes
}

// Istio finding: memTrendBytes read 829 MB for a 44 MB proxy. A 24 h
// extrapolation of a slope fitted on an hour of almost-flat, noisy (or
// start-up ramp) data is meaningless; it must yield no trend instead.
func TestMemTrendRejectsNoiseAndShortHistory(t *testing.T) {
	const base = 44 * 1000 * 1000.0
	t0 := 1_800_000_000.0
	cases := []struct {
		name string
		mem  []Sample
	}{
		// 1 h of history (WINDOW=1h, step 300 s) creeping up 10% with noise: the
		// old code projected it 24 h ahead.
		{"1h start-up creep", noisy(t0, 12, 1, 0.01, func(i int) float64 { return base * (1 + 0.01*float64(i)) })},
		// Two days flat with noise: any fitted slope is noise.
		{"2d flat with noise", noisy(t0, 576, 2, 0.03, func(int) float64 { return base })},
		{"3 points", series(t0, 300, 3, func(i int) float64 { return base + float64(i)*1e6 })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := memTrendOf(t, tc.mem); p != nil {
				t.Fatalf("memTrendBytes = %.0f MB for a ~44 MB container, want no trend", *p/1e6)
			}
		})
	}
}

// A real leak keeps its trend, projected no further than the history it was
// fitted on (here the full 24 h horizon: 2 days of data).
func TestMemTrendKeepsRealLeak(t *testing.T) {
	t0 := 1_800_000_000.0
	const perStep = 110 * mib / 288.0 // 110 MiB/day
	mem := noisy(t0, 576, 3, 0.005, func(i int) float64 { return 300*mib + perStep*float64(i) })
	p := memTrendOf(t, mem)
	if p == nil {
		t.Fatal("leak not reported")
	}
	want := 300*mib + perStep*float64(575+288)
	if math.Abs(*p-want)/want > 0.03 {
		t.Fatalf("leak trend = %.0f MiB, want ~%.0f MiB", *p/mib, want/mib)
	}
	// 6 h of the same leak: the projection horizon is capped at the 6 h observed.
	short := mem[:72]
	p = memTrendOf(t, short)
	if p == nil {
		t.Fatal("6h leak not reported")
	}
	if want := 300*mib + perStep*float64(71+72); math.Abs(*p-want)/want > 0.03 {
		t.Fatalf("6h leak trend = %.0f MiB, want ~%.0f MiB (horizon capped at the observed span)", *p/mib, want/mib)
	}
}
