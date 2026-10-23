package backtest

import (
	"errors"
	"fmt"
	"math"

	"github.com/nicacioliveira/k8s-tailor/internal/reconciler"
)

// The disruptive replay. A trace with a Disruption restarts a container on every
// resize, so replicas no longer change together: the replay keeps each pod's own
// resources and readiness, paces the restarts behind the PodDisruptionBudget with
// the reconciler's own RollingGate, and counts the restarts each strategy causes.
//
//   - A PodStrategy (the in-place engine) decides per pod, through the
//     reconciler's PlanInPlace + Cooled + RollingGate, exactly like reconcileNS.
//   - Any other strategy returns ONE target; the replay then rolls the pods to it
//     one PDB-budget at a time (a controller rollout, or the VPA updater's
//     evictions, both honor the PDB). Each pod moved costs one restart.

// ErrNotApplicable is returned by a Factory that does not apply to a trace; Run
// skips the strategy silently.
var ErrNotApplicable = errors.New("strategy not applicable to this trace")

// PodState is one replica as a PodStrategy sees it.
type PodState struct {
	Applied Resources
	Ready   bool
}

// PodAction is what a PodStrategy wants one replica to run with from the next
// tick; Restart says that getting there restarts its container.
type PodAction struct {
	Next    Resources
	Restart bool
}

// PodStrategy decides replica by replica (the in-place engine). Observe is not
// used for it on a disruptive trace.
type PodStrategy interface {
	Strategy
	ObservePods(tk Tick, pods []PodState) ([]PodAction, error)
}

// pdbAllowed is the PDB's disruptionsAllowed: Ready pods above minAvailable.
func pdbAllowed(ready, minAvailable int) int {
	if a := ready - minAvailable; a > 0 {
		return a
	}
	return 0
}

func countReady(pods []PodState) int {
	n := 0
	for _, p := range pods {
		if p.Ready {
			n++
		}
	}
	return n
}

// paced rolls the pods to a target one PDB budget at a time. It is the replay's
// model of a controller/updater doing the restarts for a strategy that only says
// what the resources should be.
type paced struct {
	s      Strategy
	target Resources
}

func (p *paced) observe(tk Tick, pods []PodState, d *Disruption) ([]PodAction, error) {
	next, err := p.s.Observe(tk, p.target)
	if err != nil {
		return nil, err
	}
	if tk.CanAct {
		p.target = next
	}
	out := make([]PodAction, len(pods))
	ready := countReady(pods)
	restarted := 0
	for j, pd := range pods {
		out[j].Next = pd.Applied
		if pd.Applied == p.target {
			continue
		}
		if pd.Ready {
			others := true
			for k, o := range pods {
				if k != j && !o.Ready {
					others = false
				}
			}
			st := reconciler.RollingState{OthersReady: others, ReadyRestarted: restarted, PDBProtected: true, PDBAllowed: pdbAllowed(ready, d.PDBMinAvailable)}
			if reconciler.RollingGate(st) != reconciler.RollingGo {
				continue
			}
			restarted++
		}
		out[j] = PodAction{Next: p.target, Restart: true}
	}
	return out, nil
}

// replayRolling is replay for a trace with a Disruption: the metrics are the
// same, scored per pod on the resources THAT pod runs with, plus restarts and the
// most pods not Ready at once.
func replayRolling(tr, obs *Trace, s Strategy, warm int, opt Options) (Metrics, error) {
	d := tr.Disruption
	if d.ReadySteps < 1 {
		return Metrics{}, fmt.Errorf("%s: disruption.readySteps must be >= 1", tr.Key())
	}
	n, np := tr.Len(), len(tr.Pods)
	stepH := float64(tr.StepSec) / 3600
	pods := make([]PodState, np)
	readyAt := make([]int, np)
	for j := range pods {
		pods[j] = PodState{Applied: tr.Initial, Ready: true}
	}
	var pacer *paced
	ps, isPods := s.(PodStrategy)
	if !isPods {
		pacer = &paced{s: s, target: tr.Initial}
	}
	version := tr.Initial
	inOOM := make([]bool, np)
	m := Metrics{Strategy: s.Name()}
	var samples, over int
	var sumCPUWaste, sumMemWaste, sumCPUReq, sumMemReq float64
	tk := Tick{CPU: make([]float64, np), Mem: make([]float64, np), OOM: make([]bool, np)}
	for i := 0; i < n; i++ {
		eval := i >= warm
		for j := range pods {
			if !pods[j].Ready && i >= readyAt[j] {
				pods[j].Ready = true
			}
		}
		if nr := np - countReady(pods); nr > m.MaxNotReady && eval {
			m.MaxNotReady = nr
		}
		tk.I, tk.T, tk.CanAct = i, tr.TimeAt(i), i+1 >= warm && i+1 < n
		for p := range tr.Pods {
			applied := pods[p].Applied
			ceiling := applied.MemCeiling()
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
		var acts []PodAction
		var err error
		if isPods {
			acts, err = ps.ObservePods(tk, append([]PodState(nil), pods...))
		} else {
			acts, err = pacer.observe(tk, pods, d)
		}
		if err != nil {
			return m, err
		}
		if !tk.CanAct {
			continue
		}
		if len(acts) != np {
			return m, fmt.Errorf("tick %d: strategy returned %d pod actions for %d pods", i, len(acts), np)
		}
		for j, a := range acts {
			if a.Next.CPURequest <= 0 || a.Next.MemRequest <= 0 {
				return m, fmt.Errorf("tick %d: strategy returned non-positive request %+v", i, a.Next)
			}
			if a.Next == pods[j].Applied {
				continue
			}
			// A change is a new version of the resources entering the fleet; the pods
			// that follow it over the next ticks are the same change (restarts count them).
			if a.Next != version {
				m.Changes++
				m.ChangeLog = append(m.ChangeLog, Change{T: tr.TimeAt(i + 1), To: a.Next})
				version = a.Next
			}
			pods[j].Applied = a.Next
			if a.Restart {
				m.Restarts++
				pods[j].Ready = false
				readyAt[j] = i + 1 + d.ReadySteps
			}
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
		m.RestartsPerDay = float64(m.Restarts) / days
	}
	m.Final = version
	return m, nil
}
