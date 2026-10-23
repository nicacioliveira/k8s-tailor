package recommender

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestQuantile(t *testing.T) {
	if got := Quantile([]float64{1, 2, 3, 4, 5}, 0.5); got != 3 {
		t.Fatalf("median = %v, want 3", got)
	}
	if got := Quantile([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.9); !approx(got, 9.1, 1e-9) {
		t.Fatalf("p90 = %v, want 9.1", got)
	}
	if got := Quantile(nil, 0.95); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
	if got := Quantile([]float64{42}, 0.95); got != 42 {
		t.Fatalf("single = %v, want 42", got)
	}
}

func TestWeightedQuantileDecay(t *testing.T) {
	half := 10.0
	oldSpike := []Sample{{0, 100}}
	for i := 0; i < 10; i++ {
		oldSpike = append(oldSpike, Sample{float64(10 + i*9), 1})
	}
	qOld := WeightedQuantile(oldSpike, 0.9, half, 100)

	recentSpike := []Sample{}
	for i := 0; i < 10; i++ {
		recentSpike = append(recentSpike, Sample{float64(i * 9), 1})
	}
	recentSpike = append(recentSpike, Sample{100, 100})
	qRecent := WeightedQuantile(recentSpike, 0.9, half, 100)

	if !(qRecent > qOld) {
		t.Fatalf("recent spike (%v) should outweigh old spike (%v)", qRecent, qOld)
	}
}

func TestPeak(t *testing.T) {
	if got := Peak([]Sample{{0, 3}, {1, 9}, {2, 5}}); got != 9 {
		t.Fatalf("peak = %v, want 9", got)
	}
}

func TestLinearPredict(t *testing.T) {
	up := []Sample{{0, 0}, {10, 10}, {20, 20}}
	slope, ok := LinearSlope(up)
	if !ok || !approx(slope, 1, 1e-9) {
		t.Fatalf("slope = %v ok=%v, want 1", slope, ok)
	}
	if p, _ := PredictLinear(up, 5); !approx(p, 25, 1e-9) {
		t.Fatalf("predict = %v, want 25", p)
	}
	if _, ok := PredictLinear([]Sample{{0, 1}}, 10); ok {
		t.Fatal("predict with 1 sample should be ok=false")
	}
}

func TestSeasonalForecast(t *testing.T) {
	// Season of 100s: high (10) in phase [0,10), low (1) elsewhere. Build 5 seasons.
	var s []Sample
	for cycle := 0; cycle < 5; cycle++ {
		base := float64(cycle * 100)
		for ph := 0; ph < 100; ph += 5 {
			v := 1.0
			if ph < 10 {
				v = 10.0
			}
			s = append(s, Sample{T: base + float64(ph), V: v})
		}
	}
	// Forecast at a target inside the peak phase should return the high value...
	peak := SeasonalForecast(s, 0.9, 100, 10, 500+3) // targetT phase 3 => peak bucket
	if peak < 8 {
		t.Fatalf("seasonal peak forecast = %v, want ~10", peak)
	}
	// ...while the target in a trough returns the low value.
	trough := SeasonalForecast(s, 0.9, 100, 10, 500+50) // phase 50 => trough
	if trough > 2 {
		t.Fatalf("seasonal trough forecast = %v, want ~1", trough)
	}
	// Not enough data => 0 (caller falls back to the decay estimate).
	if got := SeasonalForecast(s[:3], 0.9, 100, 10, 3); got != 0 {
		t.Fatalf("insufficient data forecast = %v, want 0", got)
	}
}

func TestRounding(t *testing.T) {
	if got := RoundCPUCores(0.1234); !approx(got, 0.124, 1e-9) {
		t.Fatalf("cpu = %v, want 0.124", got)
	}
	if got := RoundCPUCores(0); !approx(got, 0.001, 1e-9) {
		t.Fatalf("cpu min = %v, want 0.001", got)
	}
	mib := float64(1024 * 1024)
	if got := RoundMemBytes(1.5 * mib); got != 2*mib {
		t.Fatalf("mem = %v, want %v", got, 2*mib)
	}
	if got := RoundMemBytes(0); got != mib {
		t.Fatalf("mem min = %v, want %v", got, mib)
	}
}
