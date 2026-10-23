package recommender

import (
	"math"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// burstSeries: a 0.1-core baseline with a 0.8-core burst in `burstPct` percent
// of the samples, one replica, 7 days of 5-minute steps.
func burstSeries(burstPct int) []Sample {
	n := 7 * 288
	out := make([]Sample, n)
	for i := range out {
		v := 0.1
		if i%100 < burstPct {
			v = 0.8
		}
		out[i] = Sample{T: float64(1_800_000_000 - (n-1-i)*300), V: v}
	}
	return out
}

func TestBurstinessIndex(t *testing.T) {
	if idx, ok := burstiness(series(0, 300, 500, constant(0.3))); !ok || math.Abs(idx-1) > 1e-9 {
		t.Fatalf("flat series: %v %v, want 1", idx, ok)
	}
	if idx, ok := burstiness(burstSeries(7)); !ok || idx < 6 || idx > 9 {
		t.Fatalf("7%% bursts of 8x: %v %v, want about 8 (p99 / p90)", idx, ok)
	}
	// A burst share above 10% moves p90 itself into the burst: the tail is then
	// the sizing level, not a spike above it.
	if idx, _ := burstiness(burstSeries(30)); idx > 1.5 {
		t.Fatalf("30%% bursts: %v, want ~1 (p90 is already in the burst)", idx)
	}
	if _, ok := burstiness(series(0, 300, 50, constant(0.3))); ok {
		t.Fatal("50 samples (~4 h) must not define an index")
	}
	// An idle container: the denominator is floored, so a few 10m blips on a ~0 baseline are not "bursty".
	idle := series(0, 300, 600, func(i int) float64 {
		if i%50 == 0 {
			return 0.01
		}
		return 0.0005
	})
	if idx, ok := burstiness(idle); !ok || idx > 3 {
		t.Fatalf("idle container with blips: %v %v, want <= 3", idx, ok)
	}
}

// autoQuantile (opt-in): when the burstiness index reaches the threshold the
// CPU quantile is raised to 0.95; the index and the reason are always exposed.
func TestAutoQuantile(t *testing.T) {
	cpu := burstSeries(7)
	mem := series(float64(1_800_000_000-7*288*300), 300, 7*288, constant(100*mib))
	run := func(auto bool, thr float64, q float64, c []Sample) Recommendation {
		res := testResolved()
		res.CPU.Quantile = q
		res.AutoQuantile, res.BurstThreshold = auto, thr
		return Recommend(RecommendInput{
			Workload: testWL, Container: k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib},
			CPU: c, Mem: mem, Policy: res, HalfLifeSec: 86400, Now: time.Unix(1_800_000_000, 0), MinSamples: 1,
		})
	}

	off := run(false, 0, 0.90, cpu)
	on := run(true, 0, 0.90, cpu)
	if off.Signals.Burstiness == nil || *off.Signals.Burstiness < 6 {
		t.Fatalf("the index must be exposed even with the opt-in off: %+v", off.Signals.Burstiness)
	}
	if off.Signals.CPUQuantileUsed != 0.90 || off.Signals.BurstReason == "" {
		t.Fatalf("off: quantile used %v, reason %q", off.Signals.CPUQuantileUsed, off.Signals.BurstReason)
	}
	if on.Signals.CPUQuantileUsed != 0.95 || !on.Signals.BurstQuantileRaised {
		t.Fatalf("on: quantile used %v raised=%v, want 0.95/true (%s)", on.Signals.CPUQuantileUsed, on.Signals.BurstQuantileRaised, on.Signals.BurstReason)
	}
	if on.Recommended.CPURequest <= off.Recommended.CPURequest {
		t.Fatalf("raising the quantile into the burst range must raise the request: %v vs %v", on.Recommended.CPURequest, off.Recommended.CPURequest)
	}

	if r := run(true, 0, 0.90, series(float64(1_800_000_000-7*288*300), 300, 7*288, constant(0.3))); r.Signals.BurstQuantileRaised || r.Signals.CPUQuantileUsed != 0.90 {
		t.Fatalf("a flat series was raised: %+v", r.Signals)
	}
	if r := run(true, 20, 0.90, cpu); r.Signals.BurstQuantileRaised {
		t.Fatalf("threshold 20 above the index %v still raised", *r.Signals.Burstiness)
	}
	if r := run(true, 0, 0.97, cpu); r.Signals.BurstQuantileRaised || r.Signals.CPUQuantileUsed != 0.97 {
		t.Fatalf("a quantile already above 0.95 must be kept: %+v", r.Signals)
	}
}
