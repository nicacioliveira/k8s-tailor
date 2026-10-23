package backtest

import (
	"math"
	"testing"
	"time"
)

// Parity with upstream util/histogram_options_test.go (TestExponentialHistogramOptions).
func TestVPAExponentialOptionsParity(t *testing.T) {
	o := newExpOptions(500, 40, 1.5, vpaEpsilon)
	if o.numBuckets != 6 {
		t.Fatalf("numBuckets = %d, want 6", o.numBuckets)
	}
	for b, want := range []float64{0, 40, 100, 190, 325, 527.5} {
		if got := o.bucketStart(b); !near(got, want, 1e-9) {
			t.Errorf("bucketStart(%d) = %v, want %v", b, got, want)
		}
	}
	for v, want := range map[float64]int{-1: 0, 39.99: 0, 40: 1, 100: 2, 900: 5} {
		if got := o.findBucket(v); got != want {
			t.Errorf("findBucket(%v) = %d, want %d", v, got, want)
		}
	}
	// The production CPU/memory options.
	if n := newExpOptions(1000, 0.01, 1.05, vpaEpsilon).numBuckets; n != 176 {
		t.Errorf("CPU buckets = %d, want 176", n)
	}
}

// Percentile returns the END of the bucket that crosses the threshold.
func TestVPAPercentileIsBucketEnd(t *testing.T) {
	o := newExpOptions(500, 40, 1.5, vpaEpsilon)
	h := newHistogram(o)
	h.add(50, 1) // bucket 1 = [40,100)
	if got := h.percentile(0.5); got != 100 {
		t.Fatalf("percentile = %v, want bucket end 100", got)
	}
	h.add(200, 3) // bucket 3 = [190,325)
	if got := h.percentile(0.25); got != 100 {
		t.Fatalf("p25 = %v, want 100", got)
	}
	if got := h.percentile(0.26); got != 325 {
		t.Fatalf("p26 = %v, want 325", got)
	}
	h.subtract(200, 3)
	if got := h.percentile(0.99); got != 100 {
		t.Fatalf("after subtract p99 = %v, want 100", got)
	}
}

// A sample one half-life newer weighs exactly twice as much.
func TestVPADecayHalfLife(t *testing.T) {
	o := newExpOptions(500, 40, 1.5, vpaEpsilon)
	h := newDecaying(o, 24*time.Hour)
	t0 := time.Unix(SyntheticEpoch, 0).UTC()
	h.addSample(50, 1, t0)                    // bucket end 100
	h.addSample(200, 1, t0.Add(24*time.Hour)) // bucket end 325, weight 2
	// total 3: p(1/3) still in the old bucket, just above it falls in the new one.
	if got := h.percentile(1.0 / 3); got != 100 {
		t.Fatalf("p33 = %v, want 100", got)
	}
	if got := h.percentile(0.34); got != 325 {
		t.Fatalf("p34 = %v, want 325", got)
	}
	// Renormalisation past 100 half-lives must preserve the ratios.
	h.addSample(200, 1, t0.Add(150*24*time.Hour))
	if got := h.percentile(0.5); got != 325 {
		t.Fatalf("after renormalisation p50 = %v, want 325", got)
	}
}

func newTestVPA(t *testing.T, tr *Trace) *vpa {
	t.Helper()
	s, err := NewVPA(DefaultVPA())(tr)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*vpa)
}

func feed(v *vpa, tr *Trace, from, to int, applied Resources, canAct bool) Resources {
	np := len(tr.Pods)
	for i := from; i < to; i++ {
		tk := Tick{I: i, T: tr.TimeAt(i), CPU: make([]float64, np), Mem: make([]float64, np), OOM: make([]bool, np), CanAct: canAct}
		for p := range tr.Pods {
			tk.CPU[p], tk.Mem[p] = tr.Pods[p].CPU[i], tr.Pods[p].Mem[i]
		}
		applied, _ = v.Observe(tk, applied)
	}
	return applied
}

// Constant usage: target = bucketEnd(usage)·1.15; confidence after 24h of
// one-minute-equivalent samples is 1 → upper multiplier ×2, lower ≈ ×0.998.
func TestVPAEstimateConstantUsage(t *testing.T) {
	n := 24*12 + 1 // exactly 24h span
	tr := constTrace(2, n, 0.3, 400*mib, Resources{CPURequest: 1, MemRequest: gib})
	v := newTestVPA(t, tr)
	feed(v, tr, 0, n, tr.Initial, false)
	if c := v.confidence(); !near(c, 1, 1e-9) {
		t.Fatalf("confidence = %v, want 1 (24h span, 5 samples/min-equivalent per tick)", c)
	}
	rec, ok := v.estimate()
	if !ok {
		t.Fatal("no estimate")
	}
	cpuOpt := v.agg.cpu.opt
	end := cpuOpt.bucketStart(cpuOpt.findBucket(0.3) + 1)
	wantTarget := cpuAmount(end) + cpuAmount(cpuAmount(end)*0.15)
	if !near(rec.target.CPURequest, wantTarget, 1e-9) {
		t.Fatalf("cpu target = %v, want %v", rec.target.CPURequest, wantTarget)
	}
	if rec.target.CPURequest < 0.3*1.15 || rec.target.CPURequest > 0.3*1.05*1.15+0.002 {
		t.Fatalf("cpu target %v outside [usage·1.15, usage·1.05·1.15]", rec.target.CPURequest)
	}
	if !near(rec.upper.CPURequest, cpuAmount(wantTarget*2), 0.0011) {
		t.Fatalf("cpu upper = %v, want ≈ 2×target", rec.upper.CPURequest)
	}
	if rec.lower.CPURequest > rec.target.CPURequest || rec.lower.CPURequest < 0.99*rec.target.CPURequest {
		t.Fatalf("cpu lower = %v, want just under target %v", rec.lower.CPURequest, rec.target.CPURequest)
	}
	// Memory: one daily peak per pod; 250Mi floor does not bind at 400Mi.
	memOpt := v.agg.mem.opt
	mend := memAmount(memOpt.bucketStart(memOpt.findBucket(400*mib) + 1))
	if want := mend + memAmount(mend*0.15); rec.target.MemRequest != want {
		t.Fatalf("mem target = %vMi, want %vMi", rec.target.MemRequest/mib, want/mib)
	}
}

func TestVPAMinimums(t *testing.T) {
	tr := constTrace(2, 300, 0.001, 10*mib, Resources{CPURequest: 1, MemRequest: gib})
	v := newTestVPA(t, tr)
	feed(v, tr, 0, 300, tr.Initial, false)
	rec, _ := v.estimate()
	if rec.target.CPURequest != 0.025 || rec.target.MemRequest != 250*mib {
		t.Fatalf("minimums not applied: %+v", rec.target)
	}
}

// RecordOOM: memory need = max(request+100MiB, request×1.2).
func TestVPARecordOOMBump(t *testing.T) {
	tr := constTrace(2, 3, 0.1, 100*mib, Resources{CPURequest: 1, MemRequest: gib})
	v := newTestVPA(t, tr)
	feed(v, tr, 0, 1, tr.Initial, false)
	v.recordOOM(0, time.Unix(tr.TimeAt(1), 0), 300*mib) // 300+100=400Mi beats 300×1.2=360Mi
	if got := v.pods[0].oomPeak; got != 400*mib {
		t.Fatalf("oomPeak = %v Mi, want 400", got/mib)
	}
	v.recordOOM(1, time.Unix(tr.TimeAt(1), 0), 1000*mib) // ×1.2 = 1200Mi beats +100
	if got := v.pods[1].oomPeak; got != 1200*mib {
		t.Fatalf("oomPeak = %v Mi, want 1200", got/mib)
	}
}

// Updater: within bounds a young pod is left alone; outside bounds it is
// evicted at once; below --min-replicas nothing is ever evicted.
func TestVPAUpdaterRules(t *testing.T) {
	n := 3 * 24 * 12
	tr := constTrace(2, n, 0.3, 400*mib, Resources{CPURequest: 0.4, MemRequest: 500 * mib, MemLimit: 1000 * mib})
	v := newTestVPA(t, tr)
	applied := feed(v, tr, 0, 24*12, tr.Initial, false)
	rec, _ := v.estimate()
	// Put the pod exactly at target: in range, diff 0 → no update forever.
	at := Resources{CPURequest: rec.target.CPURequest, MemRequest: rec.target.MemRequest, MemLimit: rec.target.MemRequest * 2}
	if got := feed(v, tr, 24*12, 24*12+3, at, true); got != at {
		t.Fatalf("at-target pod updated: %+v", got)
	}
	// Request far above upper bound → evicted immediately, limit keeps the 2× template ratio.
	big := Resources{CPURequest: 10, MemRequest: 10 * gib, MemLimit: 20 * gib}
	got := feed(v, tr, 24*12+3, 24*12+4, big, true)
	if got == big || got.CPURequest != rec.target.CPURequest || !near(got.MemLimit, memAmount(got.MemRequest*2), 1) {
		t.Fatalf("outside-range pod not updated to target with proportional limit: %+v (target %+v)", got, rec.target)
	}
	_ = applied

	one := constTrace(1, n, 0.3, 400*mib, Resources{CPURequest: 10, MemRequest: 10 * gib})
	v1 := newTestVPA(t, one)
	if got := feed(v1, one, 0, n, one.Initial, true); got != one.Initial {
		t.Fatalf("single replica evicted despite --min-replicas=2: %+v", got)
	}
}

func TestVPAUpdatesWithinBoundsAfterLifetime(t *testing.T) {
	// Usage drops slowly from 0.5 to 0.4: the new target stays inside the
	// [lower, upper] band, so only the 12h lifetime + 10% diff rule can fire.
	n := 4 * 24 * 12
	tr := fnTrace(2, n, func(_, i int) float64 {
		if i < n/2 {
			return 0.5
		}
		return 0.4
	}, func(_, _ int) float64 { return 400 * mib }, Resources{CPURequest: 0.6, MemRequest: 540 * mib})
	r, err := Run(tr, []Factory{NewStatic, NewVPA(DefaultVPA())}, Options{WarmupSec: 86400})
	if err != nil {
		t.Fatal(err)
	}
	m := metricsOf(t, r, "vpa")
	if m.Changes == 0 {
		t.Fatal("VPA never updated")
	}
	// 24h half-life: two days after the drop the pre-drop samples still hold
	// ~20% of the decayed weight, so P90 has NOT left the old 0.5 level yet —
	// VPA's documented slowness, reproduced.
	o := newExpOptions(1000, 0.01, 1.05, vpaEpsilon)
	end := cpuAmount(o.bucketStart(o.findBucket(0.5) + 1))
	if want := end + cpuAmount(end*0.15); m.Final.CPURequest != want {
		t.Fatalf("VPA final cpu = %v, want %v (P90 still on the 0.5 regime)", m.Final.CPURequest, want)
	}
	if math.IsNaN(m.AvgCPUWasteCores) {
		t.Fatal("NaN waste")
	}
}
