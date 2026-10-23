package backtest

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
)

// scripted applies a fixed schedule: resources[i] from tick i+1.
type scripted struct {
	sched map[int]Resources
	seen  []Tick
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Close()       {}
func (s *scripted) Observe(tk Tick, applied Resources) (Resources, error) {
	cp := tk
	cp.OOM = append([]bool(nil), tk.OOM...)
	s.seen = append(s.seen, cp)
	if r, ok := s.sched[tk.I]; ok {
		return r, nil
	}
	return applied, nil
}

func TestMetricsHandCounted(t *testing.T) {
	// 1 pod, 12 points (1h at 5m). warmup 0. Request 1 core / 100Mi, limit 200Mi.
	cpu := []float64{0.5, 1.5, 1.5, 0.5, 2, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5}
	mem := []float64{50, 250, 250, 50, 250, 50, 50, 50, 50, 50, 50, 50}
	init := Resources{CPURequest: 1, MemRequest: 100 * mib, MemLimit: 200 * mib}
	tr := fnTrace(1, 12, func(_, i int) float64 { return cpu[i] }, func(_, i int) float64 { return mem[i] * mib }, init)
	r, err := Run(tr, []Factory{NewStatic}, Options{PriceCPUCoreHr: 1, PriceGBHr: 1})
	if err != nil {
		t.Fatal(err)
	}
	m := r.Metrics[0]
	step := 300.0 / 3600
	if !near(m.CPUOverRequestHours, 3*step, 1e-12) {
		t.Errorf("cpu over-request hours = %v, want %v", m.CPUOverRequestHours, 3*step)
	}
	if !near(m.CPUOverRequestPct, 25, 1e-9) {
		t.Errorf("cpu over-request pct = %v, want 25", m.CPUOverRequestPct)
	}
	// Episodes: [1,2] and [4] → 2 OOMs, 3 samples above the 200Mi limit.
	if m.OOMEvents != 2 {
		t.Errorf("oom events = %d, want 2 (rising edges)", m.OOMEvents)
	}
	if !near(m.MemOverCeilingHours, 3*step, 1e-12) {
		t.Errorf("mem over ceiling = %v", m.MemOverCeilingHours)
	}
	// Waste: CPU slack = mean(max(0,1-u)) = (9*0.5)/12
	if !near(m.AvgCPUWasteCores, 4.5/12, 1e-12) {
		t.Errorf("cpu waste = %v", m.AvgCPUWasteCores)
	}
	if !near(m.AvgMemWasteGiB, (9*50.0/1024)/12, 1e-12) {
		t.Errorf("mem waste = %v", m.AvgMemWasteGiB)
	}
	// cost = 12 samples × (1 core + 100/1024 GiB) × $1 × 5/60 h
	if want := 12 * (1 + 100.0/1024) * step; !near(m.Cost, want, 1e-12) || m.CostRelative != 1 {
		t.Errorf("cost = %v rel %v, want %v rel 1", m.Cost, m.CostRelative, want)
	}
}

func TestMemCeilingFallsBackToRequest(t *testing.T) {
	tr := constTrace(1, 6, 0.1, 150*mib, Resources{CPURequest: 1, MemRequest: 100 * mib})
	r, err := Run(tr, []Factory{NewStatic}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if m := r.Metrics[0]; m.OOMEvents != 1 || !near(m.MemOverCeilingHours, 0.5, 1e-12) {
		t.Fatalf("no-limit container above request: oom=%d hours=%v, want 1 / 0.5", m.OOMEvents, m.MemOverCeilingHours)
	}
}

// The resources returned at tick i are in effect from i+1, not before; changes
// during warmup are discarded; OOM flags reach the strategy.
func TestReplayTimingAndWarmup(t *testing.T) {
	init := Resources{CPURequest: 1, MemRequest: 100 * mib}
	tr := constTrace(1, 10, 2, 150*mib, init) // always above request
	small := Resources{CPURequest: 3, MemRequest: 200 * mib}
	s := &scripted{sched: map[int]Resources{0: {CPURequest: 9, MemRequest: 9 * mib}, 4: small}}
	r, err := Run(tr, []Factory{NewStatic, func(*Trace) (Strategy, error) { return s, nil }}, Options{WarmupSec: 3 * 300})
	if err != nil {
		t.Fatal(err)
	}
	m := metricsOf(t, r, "scripted")
	// eval ticks 3..9; ticks 3,4 still at the initial request (over), 5..9 at 3 cores.
	if want := 2 * 300.0 / 3600; !near(m.CPUOverRequestHours, want, 1e-12) {
		t.Fatalf("over-request hours = %v, want %v (change effective from tick 5)", m.CPUOverRequestHours, want)
	}
	if m.Changes != 1 || len(m.ChangeLog) != 1 || m.ChangeLog[0].T != tr.TimeAt(5) {
		t.Fatalf("changes = %d log=%+v, want 1 at tick 5 (warmup change discarded)", m.Changes, m.ChangeLog)
	}
	if !s.seen[0].OOM[0] || s.seen[1].OOM[0] {
		t.Fatalf("OOM should be flagged only on the episode's first tick")
	}
	if s.seen[1].CanAct || !s.seen[2].CanAct {
		t.Fatalf("CanAct should flip at tick warmup-1")
	}
}

// Property: the static baseline never changes and its waste is exactly
// request − mean(usage) whenever usage stays below the request.
func TestStaticWasteIsConstantOffset(t *testing.T) {
	for _, name := range ScenarioNames() {
		tr, err := Generate(name, 7, 3, 300)
		if err != nil {
			t.Fatal(err)
		}
		// Push the request above every observed value so slack == req − usage.
		var maxC, maxM float64
		for _, p := range tr.Pods {
			for i := range p.CPU {
				maxC, maxM = math.Max(maxC, p.CPU[i]), math.Max(maxM, p.Mem[i])
			}
		}
		tr.Initial = Resources{CPURequest: maxC + 1, MemRequest: maxM + gib, MemLimit: maxM + 2*gib}
		r, err := Run(tr, []Factory{NewStatic}, testOpts())
		if err != nil {
			t.Fatal(err)
		}
		m := r.Metrics[0]
		warm := int(testOpts().WarmupSec / tr.StepSec)
		var sumC, sumM float64
		cnt := 0
		for _, p := range tr.Pods {
			for i := warm; i < tr.Len(); i++ {
				sumC += p.CPU[i]
				sumM += p.Mem[i]
				cnt++
			}
		}
		wantC := tr.Initial.CPURequest - sumC/float64(cnt)
		wantM := (tr.Initial.MemRequest - sumM/float64(cnt)) / gib
		if !near(m.AvgCPUWasteCores, wantC, 1e-9) || !near(m.AvgMemWasteGiB, wantM, 1e-9) {
			t.Errorf("%s: static waste cpu=%v mem=%v, want %v %v", name, m.AvgCPUWasteCores, m.AvgMemWasteGiB, wantC, wantM)
		}
		if m.Changes != 0 || m.CPUOverRequestHours != 0 || m.OOMEvents != 0 || m.Final != tr.Initial || m.CostRelative != 1 {
			t.Errorf("%s: static must be inert: %+v", name, m)
		}
		if !near(m.AvgCPURequest, tr.Initial.CPURequest, 1e-12) {
			t.Errorf("%s: avg request drifted", name)
		}
	}
}

// Property: a strategy's decisions up to tick k depend only on data up to k.
// Perturbing the future must not change any change applied at or before k+1.
func TestNoLookAhead(t *testing.T) {
	base, err := Generate("step-change", 3, 4, 300)
	if err != nil {
		t.Fatal(err)
	}
	k := base.Len() * 5 / 8
	fut := cloneTrace(base)
	for p := range fut.Pods {
		for i := k + 1; i < fut.Len(); i++ {
			fut.Pods[p].CPU[i] *= 7
			fut.Pods[p].Mem[i] *= 3
		}
	}
	facs, err := Factories("all", testEngineCfg(), DefaultKRR(), DefaultVPA())
	if err != nil {
		t.Fatal(err)
	}
	r1, err := Run(base, facs, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(fut, facs, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	cut := base.TimeAt(k + 1)
	for i := range r1.Metrics {
		a, b := prefix(r1.Metrics[i].ChangeLog, cut), prefix(r2.Metrics[i].ChangeLog, cut)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s peeks into the future:\n base=%+v\n fut =%+v", r1.Metrics[i].Strategy, a, b)
		}
	}
}

func prefix(cl []Change, cut int64) []Change {
	var out []Change
	for _, c := range cl {
		if c.T <= cut {
			out = append(out, c)
		}
	}
	return out
}

func cloneTrace(t *Trace) *Trace {
	c := *t
	c.Pods = make([]PodTrace, len(t.Pods))
	for i, p := range t.Pods {
		c.Pods[i] = PodTrace{Name: p.Name, CPU: append([]float64(nil), p.CPU...), Mem: append([]float64(nil), p.Mem...)}
	}
	return &c
}

// Property: the whole pipeline (generator + all four strategies, including the
// real engine behind its wall-clock time shift) is deterministic.
func TestDeterministicReport(t *testing.T) {
	run := func() []byte {
		facs, err := Factories("all", testEngineCfg(), DefaultKRR(), DefaultVPA())
		if err != nil {
			t.Fatal(err)
		}
		var traces []*Trace
		for _, n := range []string{"bursty", "memory-leak"} {
			tr, err := Generate(n, 42, 3, 300)
			if err != nil {
				t.Fatal(err)
			}
			traces = append(traces, tr)
		}
		res, err := RunAll(traces, facs, testOpts())
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		rep := &Report{Seed: 42, Source: "test", Options: testOpts(), Results: res}
		if err := rep.WriteJSON(&b); err != nil {
			t.Fatal(err)
		}
		if err := rep.WriteMarkdown(&b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	a, b := run(), run()
	if !bytes.Equal(a, b) {
		t.Fatal("two identical runs produced different reports")
	}
	if !strings.Contains(string(a), "| k8s-tailor |") || !strings.Contains(string(a), "| vpa |") {
		t.Fatal("markdown missing strategy rows")
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	tr := constTrace(1, 4, 0.1, mib, Resources{CPURequest: 1, MemRequest: mib})
	if _, err := Run(tr, []Factory{NewStatic}, Options{WarmupSec: 3600}); err == nil {
		t.Fatal("warmup longer than the trace must fail")
	}
	bad := constTrace(1, 4, 0.1, mib, Resources{})
	if _, err := Run(bad, []Factory{NewStatic}, Options{}); err == nil {
		t.Fatal("missing initial request must fail")
	}
	neg := constTrace(1, 4, -1, mib, Resources{CPURequest: 1, MemRequest: mib})
	if err := neg.Validate(); err == nil {
		t.Fatal("negative usage must fail validation")
	}
	if _, err := Factories("static,nope", testEngineCfg(), DefaultKRR(), DefaultVPA()); err == nil {
		t.Fatal("unknown strategy must fail")
	}
	f, _ := Factories("krr", testEngineCfg(), DefaultKRR(), DefaultVPA())
	if len(f) != 2 {
		t.Fatal("static must be added as the cost reference")
	}
}
