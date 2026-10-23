package backtest

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestCSVRoundTrip(t *testing.T) {
	tr, err := Generate("heterogeneous", 9, 1, 300)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteCSV(&b, []*Trace{tr}); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadCSV(&b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BuildTraces(recs, 300, Resources{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("traces = %d", len(got))
	}
	g := got[0]
	if g.Start != tr.Start || g.Len() != tr.Len() || len(g.Pods) != len(tr.Pods) || g.Initial != tr.Initial {
		t.Fatalf("shape mismatch: %+v vs %+v", g.Initial, tr.Initial)
	}
	for p := range tr.Pods {
		for i := 0; i < tr.Len(); i++ {
			if g.Pods[p].CPU[i] != tr.Pods[p].CPU[i] || math.Abs(g.Pods[p].Mem[i]-tr.Pods[p].Mem[i]) > 0.5 {
				t.Fatalf("pod %d point %d differs", p, i)
			}
		}
	}
}

func TestReadJSONLAndGridding(t *testing.T) {
	in := `{"timestamp":"2026-01-05T00:00:00Z","namespace":"ns","workload":"api","pod":"api-b","container":"c","cpu_cores":0.5,"mem_bytes":100,"cpu_request":1,"mem_request":2000,"mem_limit":4000}
{"timestamp":1767571210,"namespace":"ns","workload":"api","pod":"api-b","container":"c","cpu_cores":0.9,"mem_bytes":50}

{"timestamp":1767571800,"namespace":"ns","workload":"api","pod":"api-a","container":"c","cpu_cores":0.1,"mem_bytes":10}
`
	recs, err := ReadJSONL(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	trs, err := BuildTraces(recs, 300, Resources{CPULimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	tr := trs[0]
	if tr.Start != 1767571200 || tr.Len() != 3 || tr.Pods[0].Name != "api-a" {
		t.Fatalf("grid: start=%d len=%d first=%s", tr.Start, tr.Len(), tr.Pods[0].Name)
	}
	b := tr.Pods[1]
	if b.CPU[0] != 0.9 || b.Mem[0] != 100 {
		t.Fatalf("same-bucket samples must keep the max: cpu=%v mem=%v", b.CPU[0], b.Mem[0])
	}
	if !math.IsNaN(b.CPU[2]) || !math.IsNaN(tr.Pods[0].CPU[0]) {
		t.Fatal("missing points must be NaN")
	}
	want := Resources{CPURequest: 1, CPULimit: 3, MemRequest: 2000, MemLimit: 4000}
	if tr.Initial != want {
		t.Fatalf("initial = %+v, want %+v (input wins, defaults fill gaps)", tr.Initial, want)
	}
}

func TestReadCSVErrors(t *testing.T) {
	if _, err := ReadCSV(strings.NewReader("timestamp,namespace\n1,a\n")); err == nil {
		t.Fatal("missing columns must fail")
	}
	bad := "timestamp,namespace,workload,pod,container,cpu_cores,mem_bytes\nyesterday,a,b,c,d,1,2\n"
	if _, err := ReadCSV(strings.NewReader(bad)); err == nil {
		t.Fatal("bad timestamp must fail")
	}
	if _, err := BuildTraces([]Record{{Timestamp: 1, Namespace: "a"}}, 300, Resources{}); err == nil {
		t.Fatal("record without identity must fail")
	}
}

func TestGeneratorDeterministicAndSeeded(t *testing.T) {
	for _, n := range ScenarioNames() {
		a, err := Generate(n, 42, 2, 300)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := Generate(n, 42, 2, 300)
		c, _ := Generate(n, 43, 2, 300)
		if a.Len() != 2*288 {
			t.Fatalf("%s: len %d", n, a.Len())
		}
		same, diff := true, false
		for p := range a.Pods {
			for i := range a.Pods[p].CPU {
				if a.Pods[p].CPU[i] != b.Pods[p].CPU[i] || a.Pods[p].Mem[i] != b.Pods[p].Mem[i] {
					same = false
				}
				if a.Pods[p].CPU[i] != c.Pods[p].CPU[i] {
					diff = true
				}
			}
		}
		if !same || !diff {
			t.Fatalf("%s: same-seed identical=%v, other-seed differs=%v", n, same, diff)
		}
	}
	if _, err := Generate("nope", 1, 1, 300); err == nil {
		t.Fatal("unknown scenario must fail")
	}
}

// The scenarios must actually exhibit the behaviour they are named after.
func TestScenarioShapes(t *testing.T) {
	mean := func(xs []float64, from, to int) float64 {
		s := 0.0
		for _, x := range xs[from:to] {
			s += x
		}
		return s / float64(to-from)
	}
	d, _ := Generate("diurnal-weekly", 1, 7, 300)
	peak, trough := 16*12, 4*12 // 16:00 and 04:00 on Monday
	if d.Pods[0].CPU[peak] < 3*d.Pods[0].CPU[trough] {
		t.Error("diurnal: 16h should be far above 04h")
	}
	if sat := mean(d.Pods[0].CPU, 5*288, 6*288); sat > 0.8*mean(d.Pods[0].CPU, 0, 288) {
		t.Error("weekly: Saturday should be quieter than Monday")
	}
	s, _ := Generate("step-change", 1, 14, 300)
	h := s.Len() / 2
	if mean(s.Pods[0].CPU, h, s.Len()) < 2*mean(s.Pods[0].CPU, 0, h) {
		t.Error("step-change: second half CPU should be > 2× first half")
	}
	l, _ := Generate("memory-leak", 1, 14, 300)
	if l.Pods[0].Mem[5*288-1] < 2.5*l.Pods[0].Mem[0] || l.Pods[0].Mem[5*288+1] > 1.2*l.Pods[0].Mem[0] {
		t.Error("memory-leak: should grow then reset at the day-5 deploy")
	}
	b, _ := Generate("bursty", 1, 7, 300)
	var vals []float64
	vals = append(vals, b.Pods[0].CPU...)
	mx, md := 0.0, mean(vals, 0, len(vals))
	for _, v := range vals {
		mx = math.Max(mx, v)
	}
	if mx < 3*md {
		t.Error("bursty: peaks should be several times the mean")
	}
	sp, _ := Generate("memory-spikes", 1, 14, 300)
	over := 0
	for _, v := range sp.Pods[0].Mem {
		if v > sp.Initial.MemLimit {
			over++
		}
	}
	if over == 0 || over > sp.Len()/20 {
		t.Errorf("memory-spikes: %d samples above the limit, want rare but present", over)
	}
	het, _ := Generate("heterogeneous", 1, 2, 300)
	if mean(het.Pods[2].CPU, 0, het.Len()) < 4*mean(het.Pods[3].CPU, 0, het.Len()) {
		t.Error("heterogeneous: hot shard should dwarf the cold one")
	}
}
