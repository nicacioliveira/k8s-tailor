package backtest

import (
	"errors"
	"fmt"
	"math"
)

// Options configures one replay.
type Options struct {
	// WarmupSec: history every strategy sees before it may act. Metrics are
	// computed only over [warmup, end).
	WarmupSec int64
	// Prices for the cost metric ($ per core-hour, $ per GiB-hour of REQUEST).
	PriceCPUCoreHr float64
	PriceGBHr      float64
}

// Metrics scores one strategy over the evaluation window. Per-pod quantities
// are summed over pods (pod-hours) or averaged over pod-samples.
type Metrics struct {
	Strategy string `json:"strategy"`
	// CPUOverRequestHours: pod-hours where CPU usage > CPU request (throttling
	// / contention risk: the excess is not guaranteed by the scheduler).
	CPUOverRequestHours float64 `json:"cpuOverRequestHours"`
	// CPUOverRequestPct: share of pod-samples with usage > request (%).
	CPUOverRequestPct float64 `json:"cpuOverRequestPct"`
	// CPUOverLimitHours: pod-hours where the CPU DEMAND exceeds the CPU limit in
	// effect (CFS throttling; the measured usage is capped at the limit then).
	CPUOverLimitHours float64 `json:"cpuOverLimitHours"`
	// OOMEvents: number of OOM episodes (usage crossing above the memory limit,
	// or the request when no limit is set), counted per pod.
	OOMEvents int `json:"oomEvents"`
	// MemOverCeilingHours: pod-hours spent above that ceiling.
	MemOverCeilingHours float64 `json:"memOverCeilingHours"`
	// Mean over pod-samples of max(0, request − usage).
	AvgCPUWasteCores float64 `json:"avgCpuWasteCores"`
	AvgMemWasteGiB   float64 `json:"avgMemWasteGiB"`
	// Mean request in effect.
	AvgCPURequest    float64 `json:"avgCpuRequest"`
	AvgMemRequestGiB float64 `json:"avgMemRequestGiB"`
	// Changes: number of times the applied resources changed (each one is an
	// in-place resize for k8s-tailor, an eviction+restart of every replica for VPA).
	Changes       int     `json:"changes"`
	ChangesPerDay float64 `json:"changesPerDay"`
	// Restarts: container restarts caused by applied changes, summed over pods
	// (only for a trace with a Disruption). MaxNotReady: the most pods not Ready at
	// the same tick (the PDB bounds it to replicas - minAvailable).
	Restarts       int     `json:"restarts,omitempty"`
	RestartsPerDay float64 `json:"restartsPerDay,omitempty"`
	MaxNotReady    int     `json:"maxNotReady,omitempty"`
	// Cost of the REQUESTED resources over the window, and relative to static.
	Cost         float64   `json:"cost"`
	CostRelative float64   `json:"costRelative"`
	Final        Resources `json:"final"`
	// ChangeLog: every applied change (effective from T), for auditing.
	ChangeLog []Change `json:"changeLog"`
}

// Change is one applied resource change, effective from unix time T.
type Change struct {
	T  int64     `json:"t"`
	To Resources `json:"to"`
}

// Result is the outcome of one trace replayed through every strategy.
type Result struct {
	Workload    string    `json:"workload"`
	Description string    `json:"description,omitempty"`
	Pods        int       `json:"pods"`
	StepSec     int64     `json:"stepSec"`
	EvalDays    float64   `json:"evalDays"`
	Initial     Resources `json:"initial"`
	// Disruptive: the trace restarts containers on resize; the report then adds the
	// restart columns.
	Disruptive bool      `json:"disruptive,omitempty"`
	Metrics    []Metrics `json:"metrics"`
}

// Run replays tr through every factory and scores each.
func Run(tr *Trace, factories []Factory, opt Options) (*Result, error) {
	if err := tr.Validate(); err != nil {
		return nil, err
	}
	n := tr.Len()
	warm := int((opt.WarmupSec + tr.StepSec - 1) / tr.StepSec)
	if warm < 0 {
		warm = 0
	}
	if warm >= n {
		return nil, fmt.Errorf("%s: warmup (%ds) leaves no evaluation window (trace has %d points of %ds)", tr.Key(), opt.WarmupSec, n, tr.StepSec)
	}
	res := &Result{
		Workload: tr.Key(), Description: tr.Description, Pods: len(tr.Pods), StepSec: tr.StepSec,
		EvalDays: float64(int64(n-warm)*tr.StepSec) / 86400, Initial: tr.Initial, Disruptive: tr.Disruption != nil,
	}
	for _, f := range factories {
		// Each strategy reads its own OBSERVED copy of the trace: the CPU a
		// container uses is capped at its CPU limit (CFS quota), so the usage a
		// strategy measures depends on the limit IT applied. replay fills the
		// observed CPU tick by tick; nothing ever reads ahead of the current tick.
		obs := tr.observedCopy()
		s, err := f(obs)
		if errors.Is(err, ErrNotApplicable) {
			continue // e.g. the rollout strategy on a trace whose resizes restart nothing
		}
		if err != nil {
			return nil, err
		}
		replayFn := replay
		if tr.Disruption != nil {
			replayFn = replayRolling
		}
		m, err := replayFn(tr, obs, s, warm, opt)
		s.Close()
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", tr.Key(), s.Name(), err)
		}
		res.Metrics = append(res.Metrics, m)
	}
	var staticCost float64
	for _, m := range res.Metrics {
		if m.Strategy == "static" {
			staticCost = m.Cost
		}
	}
	for i := range res.Metrics {
		if staticCost > 0 {
			res.Metrics[i].CostRelative = res.Metrics[i].Cost / staticCost
		}
	}
	return res, nil
}

// replay scores s on the DEMAND in tr while s observes obs, whose CPU at each
// tick is the demand capped at the CPU limit in effect (no limit = no cap).
func replay(tr, obs *Trace, s Strategy, warm int, opt Options) (Metrics, error) {
	n, np := tr.Len(), len(tr.Pods)
	stepH := float64(tr.StepSec) / 3600
	applied := tr.Initial
	inOOM := make([]bool, np)
	m := Metrics{Strategy: s.Name()}
	var samples, over int
	var sumCPUWaste, sumMemWaste, sumCPUReq, sumMemReq float64
	tk := Tick{CPU: make([]float64, np), Mem: make([]float64, np), OOM: make([]bool, np)}
	for i := 0; i < n; i++ {
		eval := i >= warm
		ceiling := applied.MemCeiling()
		tk.I, tk.T, tk.CanAct = i, tr.TimeAt(i), i+1 >= warm && i+1 < n
		for p := range tr.Pods {
			c, mem := tr.Pods[p].CPU[i], tr.Pods[p].Mem[i]
			seen := c
			if applied.CPULimit > 0 && c > applied.CPULimit {
				seen = applied.CPULimit
			}
			obs.Pods[p].CPU[i] = seen
			tk.CPU[p], tk.Mem[p] = seen, mem
			tk.OOM[p] = false
			if !math.IsNaN(mem) {
				isOver := mem > ceiling
				tk.OOM[p] = isOver && !inOOM[p]
				inOOM[p] = isOver
			}
			if !eval || math.IsNaN(c) || math.IsNaN(mem) {
				continue
			}
			samples++
			if c > applied.CPURequest {
				over++
				m.CPUOverRequestHours += stepH
			}
			if applied.CPULimit > 0 && c > applied.CPULimit {
				m.CPUOverLimitHours += stepH
			}
			if mem > ceiling {
				m.MemOverCeilingHours += stepH
			}
			if tk.OOM[p] {
				m.OOMEvents++
			}
			sumCPUWaste += math.Max(0, applied.CPURequest-c)
			sumMemWaste += math.Max(0, applied.MemRequest-mem)
			sumCPUReq += applied.CPURequest
			sumMemReq += applied.MemRequest
			m.Cost += (applied.CPURequest*opt.PriceCPUCoreHr + applied.MemRequest/gib*opt.PriceGBHr) * stepH
		}
		next, err := s.Observe(tk, applied)
		if err != nil {
			return m, err
		}
		if !tk.CanAct {
			continue
		}
		if next.CPURequest <= 0 || next.MemRequest <= 0 {
			return m, fmt.Errorf("tick %d: strategy returned non-positive request %+v", i, next)
		}
		if next != applied {
			m.Changes++
			applied = next
			m.ChangeLog = append(m.ChangeLog, Change{T: tr.TimeAt(i + 1), To: next})
		}
	}
	if samples > 0 {
		f := float64(samples)
		m.CPUOverRequestPct = 100 * float64(over) / f
		m.AvgCPUWasteCores = sumCPUWaste / f
		m.AvgMemWasteGiB = sumMemWaste / f / gib
		m.AvgCPURequest = sumCPUReq / f
		m.AvgMemRequestGiB = sumMemReq / f / gib
	}
	days := float64(int64(n-warm)*tr.StepSec) / 86400
	if days > 0 {
		m.ChangesPerDay = float64(m.Changes) / days
	}
	m.Final = applied
	return m, nil
}
