package recommender

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// noisyDiurnal: a daily cycle buried in noise — hourly-profile correlation
// around 0.6-0.8, between the default threshold (0.5) and a strict one.
func noisyDiurnal(days int, noise float64) []Sample {
	rng := rand.New(rand.NewPCG(3, 4))
	out := make([]Sample, days*288)
	for i := range out {
		t := float64(i) * 300
		h := math.Mod(t, day) / 3600
		load := 0.5 - 0.5*math.Cos(2*math.Pi*(h-4)/24)
		out[i] = Sample{T: t, V: 0.3 + 0.2*load + noise*rng.NormFloat64()}
	}
	return out
}

// weeklyOnly: flat weekdays, a Monday peak (no daily cycle on the other days).
func weeklyOnly(days int) []Sample {
	rng := rand.New(rand.NewPCG(5, 6))
	out := make([]Sample, days*288)
	for i := range out {
		t := float64(SyntheticMonday) + float64(i)*300
		v := 0.2
		if int(math.Floor((t-float64(SyntheticMonday))/day))%7 == 0 { // Monday
			h := math.Mod(t, day) / 3600
			if h >= 8 && h < 18 {
				v = 0.8
			}
		}
		out[i] = Sample{T: t, V: v * (1 + 0.03*rng.NormFloat64())}
	}
	return out
}

// SyntheticMonday: 2026-01-05T00:00:00Z.
const SyntheticMonday = 1767571200

func TestSeasonGateTunable(t *testing.T) {
	s := noisyDiurnal(7, 0.2)
	c, ok := seasonLagCorr(meanByTime(s), 24, 0)
	if !ok || c < 0.5 || c > 0.9 {
		t.Fatalf("fixture correlation %v (ok=%v) not between the default and the strict threshold", c, ok)
	}
	if got := effectiveSeasonGate(s, "daily", 0, 0); got != "daily" {
		t.Fatalf("default gate: %q, want daily", got)
	}
	if got := effectiveSeasonGate(s, "daily", 0.9, 0); got != "off" {
		t.Fatalf("minCorrelation 0.9: %q, want off", got)
	}
	// 30 h of a clean cycle: 6 day-lag pairs. Default minPairs (24) → off;
	// minPairs 4 → enough evidence.
	short := diurnalSeries(0, 2)[:360]
	if got := effectiveSeasonGate(short, "daily", 0, 0); got != "off" {
		t.Fatalf("30h with the default 24 pairs: %q, want off", got)
	}
	if got := effectiveSeasonGate(short, "daily", 0, 4); got != "daily" {
		t.Fatalf("30h with minPairs 4: %q, want daily", got)
	}
}

// A weekly-only pattern (Monday peak, flat weekdays) has no daily cycle, so the
// daily-lag proxy rejects it: with a 7-day window (no week-lag pairs) weekly
// seasonality is not detectable. With >= 8 days the week-lag correlation itself
// is measured; with >= 14 days it covers every hour of the week.
func TestWeeklySeasonNeedsMoreThanAWeek(t *testing.T) {
	if got := effectiveSeasonGate(weeklyOnly(7), "weekly", 0, 0); got != "off" {
		t.Fatalf("7d window: %q, want off (no week-lag pairs, daily proxy rejects)", got)
	}
	if got := effectiveSeasonGate(weeklyOnly(14), "weekly", 0, 0); got != "weekly" {
		t.Fatalf("14d window: %q, want weekly (real week-lag correlation)", got)
	}
	if got := effectiveSeasonGate(weeklyOnly(14), "both", 0, 0); got != "weekly" {
		t.Fatalf("14d window, season both: %q, want weekly only", got)
	}
}

// The policy fields reach Recommend.
func TestRecommendUsesPolicySeasonGate(t *testing.T) {
	now := time.Unix(int64(30*3600), 0)
	short := diurnalSeries(0, 2)[:360]
	res := testResolved()
	res.Season = "daily"
	in := RecommendInput{Workload: testWL, Container: k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib},
		CPU: short, Mem: series(0, 300, 360, constant(100*mib)), Policy: res, Now: now, MinSamples: 1}
	if got := Recommend(in).Signals.CPUSeason; got != "off" {
		t.Fatalf("default gate: CPUSeason %q, want off", got)
	}
	in.Policy.SeasonMinPairs = 4
	if got := Recommend(in).Signals.CPUSeason; got != "daily" {
		t.Fatalf("seasonGate.minPairs=4: CPUSeason %q, want daily", got)
	}
}

// memory.enforceLimit: a container WITHOUT a memory limit gets one only when
// the policy opts in (request × limitFactor, never below peak × (1+margin)).
func TestRecommendEnforceMemLimit(t *testing.T) {
	now := time.Unix(int64(200*300), 0)
	mem := series(0, 300, 200, func(i int) float64 { return float64(100+i%10) * mib })
	cpu := series(0, 300, 200, constant(0.1))
	c := k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib} // no limits
	res := testResolved()
	res.Mem.LimitFactor = 1.5
	in := RecommendInput{Workload: testWL, Container: c, CPU: cpu, Mem: mem, Policy: res, Now: now, MinSamples: 1}
	if got := Recommend(in).Recommended.MemLimit; got != 0 {
		t.Fatalf("default: mem limit %v introduced", got)
	}
	in.Policy.Mem.EnforceLimit = true
	rec := Recommend(in)
	if rec.Recommended.MemLimit < rec.Recommended.MemRequest*1.5-mib || rec.Recommended.MemLimit < 109*mib*1.15 {
		t.Fatalf("enforceLimit: limit %v for request %v (peak 109Mi)", rec.Recommended.MemLimit/mib, rec.Recommended.MemRequest/mib)
	}
	// limitFactor 0 (unset) on an enforced limit: request × 1, still >= peak × (1+margin).
	in.Policy.Mem.LimitFactor = 0
	if rec := Recommend(in); rec.Recommended.MemLimit < rec.Recommended.MemRequest || rec.Recommended.MemLimit < 109*mib*1.15-mib {
		t.Fatalf("enforceLimit, factor 0: limit %v request %v", rec.Recommended.MemLimit/mib, rec.Recommended.MemRequest/mib)
	}
}
