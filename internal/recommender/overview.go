package recommender

import (
	"sort"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

// UsagePoint is cluster-aggregated usage at a timestamp.
type UsagePoint struct {
	T   float64 `json:"t"`
	CPU float64 `json:"cpu"` // cores, summed across all workloads
	Mem float64 `json:"mem"` // bytes, summed
}

// Overview is the cluster summary: totals + aggregate usage trend.
type Overview struct {
	Workloads      int          `json:"workloads"`
	Under          int          `json:"under"`
	Over           int          `json:"over"`
	CostUsdWeek    float64      `json:"costUsdWeek"`
	SavingsUsdWeek float64      `json:"savingsUsdWeek"`
	WastedPct      float64      `json:"wastedPct"`
	CPUReserved    float64      `json:"cpuReserved"`    // cores, total reserved (request*replicas)
	CPURecommended float64      `json:"cpuRecommended"` // cores
	MemReserved    float64      `json:"memReserved"`    // bytes
	MemRecommended float64      `json:"memRecommended"` // bytes
	Usage          []UsagePoint `json:"usage"`
}

func (e *Engine) Overview() Overview {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ov := Overview{Workloads: len(e.list)}
	for _, r := range e.list {
		rep := float64(r.Replicas)
		// Reserved = what the LIVE pods hold (what the node actually reserves and
		// Karpenter schedules against); in-place resizes are reflected here.
		ov.CPUReserved += r.Applied.CPURequest * rep
		ov.CPURecommended += r.Recommended.CPURequest * rep
		ov.MemReserved += r.Applied.MemRequest * rep
		ov.MemRecommended += r.Recommended.MemRequest * rep
		if r.WasteUSDWeek > 0 {
			ov.SavingsUsdWeek += r.WasteUSDWeek
		}
		// one bucket per workload: net more expensive to run right => under; cheaper => over
		curCost := r.Current.CPURequest*e.cfg.PriceCPUCoreHr + r.Current.MemRequest/gb*e.cfg.PriceGBHr
		recCost := r.Recommended.CPURequest*e.cfg.PriceCPUCoreHr + r.Recommended.MemRequest/gb*e.cfg.PriceGBHr
		if recCost > curCost*1.05 {
			ov.Under++
		} else if recCost < curCost*0.95 {
			ov.Over++
		}
	}

	// Aggregate per-workload usage trends into a single cluster series (sum by timestamp).
	cpuByT := map[float64]float64{}
	memByT := map[float64]float64{}
	for _, en := range e.byKey {
		for _, s := range en.CPUTrend {
			cpuByT[s.T] += s.V
		}
		for _, s := range en.MemTrend {
			memByT[s.T] += s.V
		}
	}
	ts := make([]float64, 0, len(cpuByT))
	for t := range cpuByT {
		ts = append(ts, t)
	}
	sort.Float64s(ts)
	for _, t := range ts {
		ov.Usage = append(ov.Usage, UsagePoint{T: t, CPU: cpuByT[t], Mem: memByT[t]})
	}

	ov.CostUsdWeek = round2((ov.CPUReserved*e.cfg.PriceCPUCoreHr + ov.MemReserved/gb*e.cfg.PriceGBHr) * config.HoursPerWeek)
	if ov.CostUsdWeek > 0 {
		ov.WastedPct = round2(ov.SavingsUsdWeek / ov.CostUsdWeek * 100)
	}
	ov.SavingsUsdWeek = round2(ov.SavingsUsdWeek)
	return ov
}
