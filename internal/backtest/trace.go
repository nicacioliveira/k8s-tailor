// Package backtest replays a usage time series (CPU + memory per pod of one
// workload container) through several right-sizing strategies — the real
// k8s-tailor engine, faithful ports of the KRR "simple" strategy and the VPA
// recommender+updater, and the static current request — and scores each on the
// same offline, deterministic simulation: throttling-risk hours, simulated OOM
// events, mean waste, stability (changes/day) and relative cost.
//
// It is a SIMULATION. Usage is an exogenous trace: changing a request never
// changes the usage that follows (no feedback from throttling, OOM restarts,
// scheduling or HPA). See docs/BACKTEST.md for the honest list of limitations.
package backtest

import (
	"fmt"
	"math"
)

const (
	mib = 1024 * 1024
	gib = 1024 * 1024 * 1024
)

// Resources is the per-container request/limit set a strategy applies. CPU in
// cores, memory in bytes; a zero limit means "unset".
type Resources struct {
	CPURequest float64 `json:"cpuRequest"`
	CPULimit   float64 `json:"cpuLimit"`
	MemRequest float64 `json:"memRequest"`
	MemLimit   float64 `json:"memLimit"`
}

// MemCeiling is the memory value above which the simulation counts an OOM: the
// limit when set, else the request (an unbounded container above its request is
// the first candidate for node-pressure eviction / OOM-kill).
func (r Resources) MemCeiling() float64 {
	if r.MemLimit > 0 {
		return r.MemLimit
	}
	return r.MemRequest
}

// PodTrace is one replica's usage on the trace grid. NaN = no sample (pod absent).
type PodTrace struct {
	Name string    `json:"name"`
	CPU  []float64 `json:"-"` // cores
	Mem  []float64 `json:"-"` // bytes
}

// Trace is the usage history of ONE workload container across its replicas,
// sampled on a fixed grid: index i is at Start + i*StepSec (unix seconds).
type Trace struct {
	Namespace   string     `json:"namespace"`
	Workload    string     `json:"workload"`
	Container   string     `json:"container"`
	Start       int64      `json:"start"`
	StepSec     int64      `json:"stepSec"`
	Pods        []PodTrace `json:"pods"`
	Initial     Resources  `json:"initial"` // what the workload runs with before any strategy acts
	Description string     `json:"description,omitempty"`
	// Disruption (nil = a resize restarts nothing: the default, every scenario but
	// restart-rolling) makes every resize restart the container, as resizePolicy
	// RestartContainer does, and replays it replica by replica behind a
	// PodDisruptionBudget. See Disruption.
	Disruption *Disruption `json:"disruption,omitempty"`
}

// Disruption describes a workload whose containers restart on every resize
// (resizePolicy RestartContainer for CPU and memory, or any resize by a rollout,
// an eviction or a new template), behind a PodDisruptionBudget.
type Disruption struct {
	// PDBMinAvailable: pods that must stay Ready; the budget at any moment is
	// ready - PDBMinAvailable (the PDB's disruptionsAllowed).
	PDBMinAvailable int `json:"pdbMinAvailable"`
	// ReadySteps: grid steps a restarted container takes to be Ready again (>= 1).
	ReadySteps int `json:"readySteps"`
}

// Len is the number of grid points.
func (t *Trace) Len() int {
	if len(t.Pods) == 0 {
		return 0
	}
	return len(t.Pods[0].CPU)
}

// TimeAt is the unix timestamp of grid index i.
func (t *Trace) TimeAt(i int) int64 { return t.Start + int64(i)*t.StepSec }

// Key identifies the workload container.
func (t *Trace) Key() string { return t.Namespace + "/" + t.Workload + "/" + t.Container }

// Validate checks the structural invariants every consumer relies on.
func (t *Trace) Validate() error {
	if t.StepSec <= 0 {
		return fmt.Errorf("%s: step must be > 0", t.Key())
	}
	if len(t.Pods) == 0 {
		return fmt.Errorf("%s: no pods", t.Key())
	}
	n := len(t.Pods[0].CPU)
	if n == 0 {
		return fmt.Errorf("%s: empty series", t.Key())
	}
	seen := map[string]bool{}
	for _, p := range t.Pods {
		if p.Name == "" || seen[p.Name] {
			return fmt.Errorf("%s: empty or duplicate pod name %q", t.Key(), p.Name)
		}
		seen[p.Name] = true
		if len(p.CPU) != n || len(p.Mem) != n {
			return fmt.Errorf("%s: pod %s series length mismatch", t.Key(), p.Name)
		}
		for i := 0; i < n; i++ {
			if v := p.CPU[i]; v < 0 || math.IsInf(v, 0) {
				return fmt.Errorf("%s: pod %s invalid cpu %v at %d", t.Key(), p.Name, v, i)
			}
			if v := p.Mem[i]; v < 0 || math.IsInf(v, 0) {
				return fmt.Errorf("%s: pod %s invalid mem %v at %d", t.Key(), p.Name, v, i)
			}
		}
	}
	if t.Initial.CPURequest <= 0 || t.Initial.MemRequest <= 0 {
		return fmt.Errorf("%s: initial cpu/mem request must be > 0 (set them in the input or via flags)", t.Key())
	}
	return nil
}

func nanSlice(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = math.NaN()
	}
	return s
}

// observedCopy returns a copy of t whose CPU series are private (replay
// overwrites them with the limit-capped usage); memory is shared read-only.
func (t *Trace) observedCopy() *Trace {
	c := *t
	c.Pods = make([]PodTrace, len(t.Pods))
	for i, p := range t.Pods {
		c.Pods[i] = PodTrace{Name: p.Name, CPU: append([]float64(nil), p.CPU...), Mem: p.Mem}
	}
	return &c
}
