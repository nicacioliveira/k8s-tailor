package backtest

import (
	"math"

	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// Tick is what a strategy observes at grid index I: every pod's usage (NaN =
// absent) and which pods STARTED an OOM episode at this tick under the
// resources that were in effect. CanAct is false while the simulation is still
// in warmup — whatever the strategy returns then is discarded, so expensive
// strategies may skip computing.
type Tick struct {
	I      int
	T      int64
	CPU    []float64
	Mem    []float64
	OOM    []bool
	CanAct bool
}

// Strategy is one right-sizing policy under test. Observe ingests tick I (the
// usage measured while `applied` was in effect) and returns the resources it
// wants in effect from tick I+1. Implementations must be deterministic.
type Strategy interface {
	Name() string
	Observe(tk Tick, applied Resources) (Resources, error)
	Close()
}

// Factory builds a fresh strategy instance for one trace.
type Factory func(tr *Trace) (Strategy, error)

// ---- static: the current request, never changed ----

type static struct{}

func (static) Name() string                                         { return "static" }
func (static) Observe(_ Tick, applied Resources) (Resources, error) { return applied, nil }
func (static) Close()                                               {}

// NewStatic is the "do nothing" baseline: the request the workload already has.
func NewStatic(*Trace) (Strategy, error) { return static{}, nil }

// ---- KRR "simple" strategy ----

// KRRConfig mirrors robusta-dev/krr `krr simple` defaults (main @ v1.31.x):
// cpu_percentile=95, memory_buffer_percentage=15, points_required=100,
// history_duration=336h, --cpu-min=10m, --mem-min=100MiB, use_oomkill_data=false.
type KRRConfig struct {
	CPUPercentile  float64 // 95
	MemBufferPct   float64 // 15
	PointsRequired int     // 100
	HistorySec     int64   // 336h
	CPUMinCores    float64 // 0.010
	MemMinBytes    float64 // 100 MiB
	// EverySec: KRR is a report, not a controller. The backtest assumes the
	// report is re-run and applied verbatim every EverySec (default 24h) — a
	// generous best case for KRR (most teams apply it far less often).
	EverySec int64
}

func DefaultKRR() KRRConfig {
	return KRRConfig{CPUPercentile: 95, MemBufferPct: 15, PointsRequired: 100, HistorySec: 336 * 3600,
		CPUMinCores: 0.010, MemMinBytes: 100 * mib, EverySec: 86400}
}

type krr struct {
	cfg   KRRConfig
	tr    *Trace
	last  int64
	acted bool
}

// NewKRR returns a factory for the KRR simple strategy.
func NewKRR(cfg KRRConfig) Factory {
	return func(tr *Trace) (Strategy, error) { return &krr{cfg: cfg, tr: tr}, nil }
}

func (k *krr) Name() string { return "krr" }
func (k *krr) Close()       {}

func (k *krr) Observe(tk Tick, applied Resources) (Resources, error) {
	if !tk.CanAct {
		return applied, nil
	}
	if k.acted && tk.T-k.last < k.cfg.EverySec {
		return applied, nil
	}
	k.acted, k.last = true, tk.T
	cpu, mem, ok := k.recommend(tk.I)
	if !ok {
		return applied, nil // "Not enough data" → KRR prints "?" and nothing changes
	}
	// request=cpu, CPU limit UNSET; memory request = limit.
	return Resources{CPURequest: cpu, CPULimit: 0, MemRequest: mem, MemLimit: mem}, nil
}

// recommend reproduces SimpleStrategy over the window (t-history, t]:
//   - CPU: per pod quantile_over_time(0.95, rate) (Prometheus linear
//     interpolation — recommender.Quantile has the same φ(n−1) semantics), then
//     MAX across pods; limit unset.
//   - Memory: per pod max_over_time, MAX across pods, × (1 + 15%); request = limit.
//   - Each resource needs >= points_required points summed across pods.
//   - Runner rounding: ceil to 1m / 1MiB, floored at --cpu-min / --mem-min.
func (k *krr) recommend(upto int) (cpu, mem float64, ok bool) {
	tr := k.tr
	tEnd := tr.TimeAt(upto)
	from := 0
	for from < upto && tr.TimeAt(from) <= tEnd-k.cfg.HistorySec {
		from++
	}
	nCPU, nMem := 0, 0
	maxCPU, maxMem := math.Inf(-1), math.Inf(-1)
	for _, p := range tr.Pods {
		var vals []float64
		pm := math.Inf(-1)
		for i := from; i <= upto; i++ {
			if v := p.CPU[i]; !math.IsNaN(v) {
				vals = append(vals, v)
			}
			if v := p.Mem[i]; !math.IsNaN(v) {
				pm = math.Max(pm, v)
				nMem++
			}
		}
		nCPU += len(vals)
		if len(vals) > 0 {
			maxCPU = math.Max(maxCPU, recommender.Quantile(vals, k.cfg.CPUPercentile/100))
		}
		maxMem = math.Max(maxMem, pm)
	}
	if nCPU < k.cfg.PointsRequired || nMem < k.cfg.PointsRequired || math.IsInf(maxCPU, -1) || math.IsInf(maxMem, -1) {
		return 0, 0, false
	}
	cpu = math.Max(math.Ceil(maxCPU*1000)/1000, k.cfg.CPUMinCores)
	mem = math.Max(math.Ceil(maxMem*(1+k.cfg.MemBufferPct/100)/mib)*mib, k.cfg.MemMinBytes)
	return cpu, mem, true
}
