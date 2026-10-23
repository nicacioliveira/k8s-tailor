package recommender

import (
	"math"
	"testing"
)

// Bug 2b: PredictLinear projected from samples[len-1] — with per-pod concatenated
// (or otherwise unordered) samples that is not the latest point. The projection
// must start from the fitted line at the LATEST timestamp (Prometheus
// predict_linear semantics) and be independent of input order.
func TestPredictLinearOrderIndependent(t *testing.T) {
	cases := []struct {
		name    string
		in      []Sample
		horizon float64
		want    float64
		ok      bool
	}{
		{"ordered", []Sample{{0, 0}, {10, 10}, {20, 20}}, 5, 25, true},
		{"unordered", []Sample{{20, 20}, {0, 0}, {10, 10}}, 5, 25, true},
		{"last sample is the oldest", []Sample{{10, 10}, {20, 20}, {0, 0}}, 5, 25, true},
		{"noisy flat line projects the fit, not the last point", []Sample{{0, 11}, {10, 9}, {20, 10}, {30, 9}, {40, 11}}, 100, 10, true},
		{"single sample", []Sample{{0, 1}}, 10, 0, false},
		{"same timestamp (vertical)", []Sample{{5, 1}, {5, 2}}, 10, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PredictLinear(c.in, c.horizon)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("PredictLinear = %v, want %v", got, c.want)
			}
		})
	}
}

func TestQuantileTable(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		q    float64
		want float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{7}, 0.99, 7},
		{"min", []float64{3, 1, 2}, 0, 1},
		{"max", []float64{3, 1, 2}, 1, 3},
		{"median odd unsorted", []float64{5, 1, 3}, 0.5, 3},
		{"interpolated p25", []float64{0, 10}, 0.25, 2.5},
		{"does not mutate input order", []float64{9, 8, 7}, 0.5, 8},
	}
	for _, c := range cases {
		in := append([]float64(nil), c.in...)
		if got := Quantile(c.in, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: Quantile=%v want %v", c.name, got, c.want)
		}
		for i := range in {
			if in[i] != c.in[i] {
				t.Errorf("%s: input mutated", c.name)
			}
		}
	}
}

func TestWeightedQuantileTable(t *testing.T) {
	flat := []Sample{{0, 1}, {1, 2}, {2, 3}, {3, 4}}
	cases := []struct {
		name  string
		in    []Sample
		q, hl float64
		now   float64
		want  float64
	}{
		{"empty", nil, 0.9, 10, 0, 0},
		{"single", []Sample{{5, 42}}, 0.9, 10, 0, 42},
		{"no decay = plain cumulative quantile", flat, 0.5, 0, 0, 2},
		{"no decay p100", flat, 1, 0, 0, 4},
		// Very short half-life: the newest sample (v=4) dominates all weight.
		{"strong decay favours newest", flat, 0.5, 0.01, 0, 4},
		// now far in the future underflows every weight to 0 → max value.
		{"all weights underflow", flat, 0.5, 1e-9, 1e12, 4},
	}
	for _, c := range cases {
		if got := WeightedQuantile(c.in, c.q, c.hl, c.now); got != c.want {
			t.Errorf("%s: WeightedQuantile=%v want %v", c.name, got, c.want)
		}
	}
}

func TestLinearSlopeTable(t *testing.T) {
	cases := []struct {
		name  string
		in    []Sample
		slope float64
		ok    bool
	}{
		{"flat", []Sample{{0, 5}, {10, 5}}, 0, true},
		{"down", []Sample{{0, 10}, {10, 0}}, -1, true},
		{"too few", []Sample{{0, 1}}, 0, false},
		{"degenerate x", []Sample{{3, 1}, {3, 9}}, 0, false},
	}
	for _, c := range cases {
		got, ok := LinearSlope(c.in)
		if ok != c.ok || math.Abs(got-c.slope) > 1e-9 {
			t.Errorf("%s: slope=%v ok=%v want %v %v", c.name, got, ok, c.slope, c.ok)
		}
	}
}

func TestSeasonForecastDispatch(t *testing.T) {
	const day, week = 86400.0, 7 * 86400.0
	// Daily pattern: 10 between 12:00-13:00 UTC, 1 otherwise; 14 days at 15m.
	// Additionally every Monday (weekly phase) 12:00-13:00 is 50.
	var s []Sample
	for t := 0.0; t < 14*day; t += 900 {
		v := 1.0
		h := math.Mod(t, day) / 3600
		if h >= 12 && h < 13 {
			v = 10
			if math.Mod(t, week) < day { // first day of the week bucket
				v = 50
			}
		}
		s = append(s, Sample{T: t, V: v})
	}
	target := 14*day + 12.5*3600 // day 14 ≡ week-phase day 0, 12:30
	cases := []struct {
		season string
		check  func(float64) bool
		desc   string
	}{
		{"off", func(v float64) bool { return v == 0 }, "0"},
		{"none", func(v float64) bool { return v == 0 }, "0"},
		{"", func(v float64) bool { return v >= 10 }, ">=10 (daily)"},
		{"daily", func(v float64) bool { return v >= 10 }, ">=10"},
		{"weekly", func(v float64) bool { return v == 50 }, "50"},
		{"both", func(v float64) bool { return v == 50 }, "max(daily,weekly)=50"},
	}
	for _, c := range cases {
		if got := seasonForecast(s, 0.9, c.season, target); !c.check(got) {
			t.Errorf("season %q: forecast=%v want %s", c.season, got, c.desc)
		}
	}
	if got := SeasonalForecast(s, 0.9, 0, 3600, target); got != 0 {
		t.Errorf("zero season must return 0, got %v", got)
	}
}

func TestPhaseDistWraps(t *testing.T) {
	if d := phaseDist(1, 99, 100); d != 2 {
		t.Fatalf("phaseDist wrap = %v, want 2", d)
	}
	if d := phaseDist(10, 30, 100); d != 20 {
		t.Fatalf("phaseDist = %v, want 20", d)
	}
}

func TestChangePointIndexTable(t *testing.T) {
	steady := series(0, 1, 100, constant(1))
	shift := series(0, 1, 100, func(i int) float64 {
		if i < 60 {
			return 1
		}
		return 3
	})
	// The scan returns the LATEST split that clears the threshold, so for a shift
	// at 60 any index in the new regime [60, n-minSeg] is correct.
	cases := []struct {
		name   string
		in     []Sample
		lo, hi int
	}{
		{"short series", series(0, 1, 10, constant(1)), 0, 0},
		{"steady", steady, 0, 0},
		{"level shift at 60", shift, 60, 75},
	}
	for _, c := range cases {
		if got := changePointIndex(c.in, 0.5); got < c.lo || got > c.hi {
			t.Errorf("%s: changePointIndex=%v want in [%v,%v]", c.name, got, c.lo, c.hi)
		}
	}
}

func TestAggregateByTimeAndHelpers(t *testing.T) {
	got := aggregateByTime([]Sample{{2, 1}, {1, 1}, {2, 3}})
	if len(got) != 2 || got[0] != (Sample{1, 1}) || got[1] != (Sample{2, 4}) {
		t.Fatalf("aggregateByTime = %v", got)
	}
	if aggregateByTime(nil) != nil {
		t.Fatal("aggregateByTime(nil) != nil")
	}
	if clamp(5, 1, 3) != 3 || clamp(0, 1, 3) != 1 || clamp(9, 1, 0) != 9 {
		t.Fatal("clamp")
	}
	if !drifted(1, 1.2) || drifted(1, 1.05) || drifted(0, 0) || !drifted(0, 1) {
		t.Fatal("drifted")
	}
	if round2(1.239) != 1.23 {
		t.Fatal("round2")
	}
	if quantileOf(nil, 0.5) != 0 || quantileOf([]Sample{{0, 1}, {1, 2}}, 0) != 1 || quantileOf([]Sample{{0, 1}, {1, 2}}, 1) != 2 {
		t.Fatal("quantileOf edges")
	}
}
