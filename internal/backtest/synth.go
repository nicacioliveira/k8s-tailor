package backtest

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
)

// SyntheticEpoch is the fixed start of every synthetic trace: Monday
// 2026-01-05T00:00:00Z, so hour-of-day / day-of-week patterns are reproducible.
const SyntheticEpoch int64 = 1767571200

// Scenario is a named synthetic workload generator.
type Scenario struct {
	Name        string
	Description string
	gen         func(g *gen) *Trace
}

// scenarios is the catalogue. Every generator is a pure function of
// (seed, days, step): same inputs → bit-identical trace.
var scenarios = []Scenario{
	{"diurnal-weekly", "3 replicas; CPU and memory with a daily cycle (peak 16:00 UTC) and weekends at 55%; AR(1) noise. Oversized initial request (1 core / 1Gi).", genDiurnal},
	{"bursty", "3 replicas; low CPU baseline with ~6 bursts/day (5–30 min, 3–8× the baseline) correlated across replicas; memory rises +80Mi during a burst.", genBursty},
	{"step-change", "3 replicas; a deploy halfway through the period shifts the level: CPU 0.20→0.55 core, memory 300Mi→650Mi.", genStep},
	{"memory-leak", "2 replicas; memory leak of 110Mi/day starting at 280Mi, reset by a deploy every 5 days; limit 1Gi.", genLeak},
	{"memory-spikes", "3 replicas; stable memory ~300Mi with short, rare spikes (~1 every 2 days, 10–20 min, 680–820Mi) above the initial 640Mi limit: a real OOMKill. Measures whether the learned OOM floor raises the limit without inflating the request.", genSpikes},
	{"heterogeneous", "4 heterogeneous replicas (hot shard 2.2×, cold 0.4×) over the same daily cycle; the recommendation is one per workload.", genHetero},
	{"restart-rolling", "3 replicas with the diurnal-weekly load whose containers RESTART on every resize (resizePolicy RestartContainer for CPU and memory) behind a PodDisruptionBudget that keeps 2 Ready; a restarted container is Ready again after one step. Initial request 1 core / 1Gi. Measures the restarts per day each strategy causes and that no more than one replica is ever not Ready.", genRestartRolling},
	{"cpu-greedy", "2 replicas of a container that consumes every CPU it is given (a busy loop: demand 10 cores, far above any limit), started with request 100m and CPU limit 200m. The measured usage is always the limit in effect, so every raise is eaten at once. Measures whether a strategy stops raising.", genGreedy},
	{"throttled-cap", "2 replicas of a mesh proxy started with request 20m and CPU limit 50m (the Istio test's optin proxy); demand 100–280m over a daily cycle (280m is what the real proxy used once its limit was lifted), so the measured usage is capped at the CPU limit in effect. Measures whether a strategy escapes censored data.", genThrottled},
}

// ScenarioNames lists the synthetic scenarios, in catalogue order.
func ScenarioNames() []string {
	out := make([]string, len(scenarios))
	for i, s := range scenarios {
		out[i] = s.Name
	}
	return out
}

// Generate builds the named synthetic scenario. days >= 1, stepSec > 0.
func Generate(name string, seed uint64, days int, stepSec int64) (*Trace, error) {
	if days < 1 || stepSec <= 0 {
		return nil, fmt.Errorf("days must be >= 1 and step > 0")
	}
	for _, s := range scenarios {
		if s.Name != name {
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(name))
		g := &gen{
			rng:  rand.New(rand.NewPCG(seed, h.Sum64())),
			n:    int(int64(days) * 86400 / stepSec),
			step: stepSec,
		}
		tr := s.gen(g)
		tr.Namespace, tr.Container, tr.Start, tr.StepSec = "backtest", "app", SyntheticEpoch, stepSec
		if tr.Workload == "" {
			tr.Workload = name
		}
		tr.Description = s.Description
		if err := tr.Validate(); err != nil {
			return nil, err
		}
		return tr, nil
	}
	known := ScenarioNames()
	sort.Strings(known)
	return nil, fmt.Errorf("unknown scenario %q (known: %v)", name, known)
}

type gen struct {
	rng  *rand.Rand
	n    int
	step int64
}

func (g *gen) t(i int) int64 { return SyntheticEpoch + int64(i)*g.step }

// day returns the fractional day index since the epoch.
func (g *gen) day(i int) float64 { return float64(int64(i)*g.step) / 86400 }

// diurnal is 0 at 04:00 UTC and 1 at 16:00 UTC (raised cosine).
func (g *gen) diurnal(i int) float64 {
	h := math.Mod(float64(g.t(i)), 86400) / 3600
	return 0.5 - 0.5*math.Cos(2*math.Pi*(h-4)/24)
}

// weekly is 1 on weekdays, 0.55 on Saturday/Sunday (epoch is a Monday).
func (g *gen) weekly(i int) float64 {
	dow := int(g.day(i)) % 7
	if dow >= 5 {
		return 0.55
	}
	return 1
}

// ar returns an AR(1) noise series (phi=0.8, unit-ish variance), one per pod.
func (g *gen) ar() []float64 {
	out := make([]float64, g.n)
	e := 0.0
	for i := range out {
		e = 0.8*e + 0.6*g.rng.NormFloat64()
		out[i] = e
	}
	return out
}

func clampMin(v, lo float64) float64 {
	if v < lo {
		return lo
	}
	return v
}

func podName(wl string, k int) string { return fmt.Sprintf("%s-7d9f-%c", wl, 'a'+k) }

func (g *gen) pods(wl string, k int) []PodTrace {
	ps := make([]PodTrace, k)
	for j := range ps {
		ps[j] = PodTrace{Name: podName(wl, j), CPU: make([]float64, g.n), Mem: make([]float64, g.n)}
	}
	return ps
}

func genDiurnal(g *gen) *Trace {
	ps := g.pods("diurnal-weekly", 3)
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			load := g.diurnal(i) * g.weekly(i)
			ps[j].CPU[i] = clampMin((0.08+0.42*load)*(1+0.10*nc[i]), 0.005)
			ps[j].Mem[i] = clampMin((380+120*load)*mib*(1+0.02*nm[i]), 32*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 1, MemRequest: 1 * gib, MemLimit: 1 * gib}}
}

func genBursty(g *gen) *Trace {
	// Shared burst schedule (a traffic spike hits every replica).
	burst := make([]float64, g.n)
	perDay := 6.0
	pStart := perDay * float64(g.step) / 86400
	for i := 0; i < g.n; i++ {
		if g.rng.Float64() >= pStart {
			continue
		}
		durSteps := int(math.Ceil((5 + 25*g.rng.Float64()) * 60 / float64(g.step)))
		amp := 3 + 5*g.rng.Float64()
		for k := i; k < i+durSteps && k < g.n; k++ {
			burst[k] = math.Max(burst[k], amp)
		}
	}
	ps := g.pods("bursty", 3)
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		jit := 0.85 + 0.3*g.rng.Float64()
		for i := 0; i < g.n; i++ {
			base := 0.12 * (1 + 0.10*nc[i])
			cpu := base
			mem := 420.0
			if burst[i] > 0 {
				cpu += 0.12 * burst[i] * jit
				mem += 80
			}
			ps[j].CPU[i] = clampMin(cpu, 0.005)
			ps[j].Mem[i] = clampMin(mem*mib*(1+0.02*nm[i]), 32*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 0.5, MemRequest: 768 * mib, MemLimit: 1 * gib}}
}

func genStep(g *gen) *Trace {
	ps := g.pods("step-change", 3)
	change := g.n / 2
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			cpuLvl, memLvl := 0.20, 300.0
			if i >= change {
				cpuLvl, memLvl = 0.55, 650.0
			}
			d := 0.8 + 0.4*g.diurnal(i)
			ps[j].CPU[i] = clampMin(cpuLvl*d*(1+0.10*nc[i]), 0.005)
			ps[j].Mem[i] = clampMin(memLvl*mib*(1+0.03*nm[i]), 32*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 0.5, MemRequest: 512 * mib, MemLimit: 1 * gib}}
}

func genLeak(g *gen) *Trace {
	ps := g.pods("memory-leak", 2)
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			age := math.Mod(g.day(i), 5) // deploy every 5 days restarts the process
			ps[j].CPU[i] = clampMin((0.10+0.10*g.diurnal(i))*(1+0.10*nc[i]), 0.005)
			ps[j].Mem[i] = clampMin((280+110*age)*mib*(1+0.01*nm[i]), 32*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 0.5, MemRequest: 512 * mib, MemLimit: 1 * gib}}
}

func genHetero(g *gen) *Trace {
	mult := []float64{1.0, 0.9, 2.2, 0.4}
	ps := g.pods("heterogeneous", len(mult))
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			load := g.diurnal(i) * g.weekly(i)
			ps[j].CPU[i] = clampMin((0.06+0.30*load)*mult[j]*(1+0.10*nc[i]), 0.005)
			ps[j].Mem[i] = clampMin((250+60*load)*math.Sqrt(mult[j])*mib*(1+0.02*nm[i]), 32*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 1, MemRequest: 1 * gib, MemLimit: 1 * gib}}
}

// genSpikes: short, rare memory spikes above the initial limit — real OOMKills.
// The spike schedule is random (seeded) and shared by the replicas (a heavy
// request hitting the fleet), so no strategy can pre-fit its phase.
func genSpikes(g *gen) *Trace {
	spike := make([]float64, g.n)
	pStart := 0.5 * float64(g.step) / 86400 // ~1 spike every 2 days
	for i := 0; i < g.n; i++ {
		if g.rng.Float64() >= pStart {
			continue
		}
		durSteps := int(math.Ceil((10 + 10*g.rng.Float64()) * 60 / float64(g.step)))
		amp := 680 + 140*g.rng.Float64()
		for k := i; k < i+durSteps && k < g.n; k++ {
			spike[k] = math.Max(spike[k], amp)
		}
	}
	ps := g.pods("memory-spikes", 3)
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			ps[j].CPU[i] = clampMin((0.10+0.15*g.diurnal(i))*(1+0.10*nc[i]), 0.005)
			mem := 300.0
			if spike[i] > 0 {
				mem = spike[i]
			}
			ps[j].Mem[i] = clampMin(mem*mib*(1+0.02*nm[i]), 32*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 0.5, MemRequest: 512 * mib, MemLimit: 640 * mib}}
}

// genThrottled: a proxy whose demand is far above its CPU limit. The trace holds
// the DEMAND; the replay caps what every strategy measures at the CPU limit it
// applied (CFS quota), so a strategy that never raises the limit only ever sees
// ~50m. Demand range from the Istio mesh test (dev/istio/README.md: 51m used
// under a 50m limit, 276m once the limit was 500m).
func genThrottled(g *gen) *Trace {
	ps := g.pods("throttled-cap", 2)
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			ps[j].CPU[i] = clampMin((0.10+0.18*g.diurnal(i))*(1+0.10*nc[i]), 0.005)
			ps[j].Mem[i] = clampMin(44*mib*(1+0.02*nm[i]), 16*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 0.020, CPULimit: 0.050, MemRequest: 64 * mib, MemLimit: 256 * mib}}
}

// genGreedy: a busy loop. The trace holds the DEMAND (10 cores, never reachable);
// the replay caps what every strategy measures at the CPU limit it applied, so the
// measured usage always equals the limit and no raise is ever enough.
func genGreedy(g *gen) *Trace {
	ps := g.pods("cpu-greedy", 2)
	for j := range ps {
		nc, nm := g.ar(), g.ar()
		for i := 0; i < g.n; i++ {
			ps[j].CPU[i] = 10 * (1 + 0.05*nc[i])
			ps[j].Mem[i] = clampMin(96*mib*(1+0.02*nm[i]), 16*mib)
		}
	}
	return &Trace{Pods: ps, Initial: Resources{CPURequest: 0.100, CPULimit: 0.200, MemRequest: 128 * mib, MemLimit: 256 * mib}}
}

// genRestartRolling: the diurnal-weekly load on containers that restart on resize.
func genRestartRolling(g *gen) *Trace {
	tr := genDiurnal(g)
	for j := range tr.Pods {
		tr.Pods[j].Name = podName("restart-rolling", j)
	}
	tr.Disruption = &Disruption{PDBMinAvailable: 2, ReadySteps: 1}
	return tr
}
