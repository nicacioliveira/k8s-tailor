package recommender

import "testing"

// bimodal builds `lowN` samples at `low` and `highN` samples at `high`.
func bimodal(lowN int, low float64, highN int, high float64) []Sample {
	s := make([]Sample, 0, lowN+highN)
	for i := 0; i < lowN; i++ {
		s = append(s, Sample{T: float64(i), V: low})
	}
	for i := 0; i < highN; i++ {
		s = append(s, Sample{T: float64(lowN + i), V: high})
	}
	return s
}

// TestShareGtVsQuantile is the headroom-breach signal's core: shareGt(s, gt) is the
// fraction of samples above gt (the MetricsQL share_gt_over_time).
func TestShareGtVsQuantile(t *testing.T) {
	// 90 idle (0.1) + 10 busy (1.0): a rec at the idle level breaches ~10% of the time.
	s := bimodal(90, 0.1, 10, 1.0)
	if got := shareGt(s, 0.1); got < 0.09 || got > 0.11 {
		t.Fatalf("breach at idle rec: want ~0.10, got %v", got)
	}
	// A rec at/above the busy level is never breached.
	if got := shareGt(s, 1.0); got != 0 {
		t.Fatalf("breach at busy rec: want 0, got %v", got)
	}
	if got := shareGt(nil, 5); got != 0 {
		t.Fatalf("empty series: want 0, got %v", got)
	}
}

// TestHeadroomGuardBounds proves the guard's contract: flooring the request at
// quantileOf(s, 1-t) guarantees the headroom breach shareGt(s, request) <= t —
// which is exactly what the recommender does when headroomGuard is enabled. Also
// shows the guard RAISES the request relative to a p90 that misses the tail.
func TestHeadroomGuardBounds(t *testing.T) {
	series := []([]Sample){
		bimodal(90, 0.1, 10, 1.0), // 10% busy
		bimodal(95, 0.2, 5, 2.0),  // 5% busy
		bimodal(50, 1.0, 50, 1.0), // flat (guard is a no-op)
		bimodal(98, 0.05, 2, 3.0), // 2% rare spike
	}
	for _, thr := range []float64{0.02, 0.05, 0.1} {
		for i, s := range series {
			guarded := quantileOf(s, 1-thr)
			breach := shareGt(s, guarded)
			if breach > thr+1e-9 {
				t.Errorf("series %d thr=%v: guard did not bound breach: %v > %v", i, thr, breach, thr)
			}
		}
	}

	// Concrete raise: a p90 request misses the 10% tail (breaches ~10%); the guard at
	// t=0.05 floors to the p95 (=1.0 here), dropping the breach to 0.
	s := bimodal(90, 0.1, 10, 1.0)
	p90 := quantileOf(s, 0.90)
	if breach := shareGt(s, p90); breach < 0.05 {
		t.Fatalf("p90 rec should breach the tail, got %v", breach)
	}
	guarded := quantileOf(s, 0.95)
	if guarded <= p90 {
		t.Fatalf("guard should raise the request above p90: guarded=%v p90=%v", guarded, p90)
	}
	if breach := shareGt(s, guarded); breach > 0.05 {
		t.Fatalf("guard should bound breach <= 5%%, got %v", breach)
	}
}
