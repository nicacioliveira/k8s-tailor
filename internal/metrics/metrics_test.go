package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestRegistryTextFormat(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "a counter", "mode", "result")
	g := r.Gauge("y", "a gauge")
	h := r.Histogram("z_seconds", "a histogram", []float64{1, 5}, "result")

	c.Inc("inplace", "applied")
	c.Add(2, "inplace", "applied")
	c.Inc("rollout", `fa"il`)
	g.Set(3)
	h.Observe(0.5, "ok")
	h.Observe(3, "ok")
	h.Observe(10, "ok")

	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE x_total counter\n",
		`x_total{mode="inplace",result="applied"} 3` + "\n",
		`x_total{mode="rollout",result="fa\"il"} 1` + "\n",
		"# TYPE y gauge\ny 3\n",
		"# TYPE z_seconds histogram\n",
		`z_seconds_bucket{result="ok",le="1"} 1` + "\n",
		`z_seconds_bucket{result="ok",le="5"} 2` + "\n",
		`z_seconds_bucket{result="ok",le="+Inf"} 3` + "\n",
		`z_seconds_sum{result="ok"} 13.5` + "\n",
		`z_seconds_count{result="ok"} 3` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if c.Value("inplace", "applied") != 3 || h.Count("ok") != 3 {
		t.Fatalf("value accessors wrong")
	}
	g.Reset()
	b.Reset()
	r.Write(&b)
	if strings.Contains(b.String(), "\ny 3\n") {
		t.Fatalf("Reset gauge still rendered")
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "c", "k")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc("a")
				var b strings.Builder
				if j%100 == 0 {
					r.Write(&b)
				}
			}
		}()
	}
	wg.Wait()
	if v := c.Value("a"); v != 8000 {
		t.Fatalf("got %v want 8000", v)
	}
}

func TestLabelArityPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on wrong label arity")
		}
	}()
	NewRegistry().Counter("a_total", "a", "x").Inc()
}
