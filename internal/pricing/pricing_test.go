package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// vmStub answers instant queries: cpu/ram hourly cost by query text; status
// forces an HTTP error.
func vmStub(t *testing.T, cpu, ram string, status int) *vm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		q := r.URL.Query().Get("query")
		v := ""
		switch {
		case strings.Contains(q, "node_cpu_hourly_cost"):
			v = cpu
		case strings.Contains(q, "node_ram_hourly_cost"):
			v = ram
		}
		if v == "" {
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[1700000000,"` + v + `"]}]}}`))
	}))
	t.Cleanup(srv.Close)
	return vm.New(srv.URL)
}

func TestPricingRefresh(t *testing.T) {
	cases := []struct {
		name            string
		mode            string
		cpu, ram        string
		status          int
		wantCPU, wantGB float64
		wantSrc         string
	}{
		{"static", "static", "", "", 0, 0.031, 0.004, "static"},
		{"opencost", "opencost", "0.05", "0.007", 0, 0.05, 0.007, "opencost"},
		{"opencost missing ram -> static", "opencost", "0.05", "", 0, 0.031, 0.004, "static"},
		{"opencost zero cost -> static", "opencost", "0", "0.007", 0, 0.031, 0.004, "static"},
		{"opencost vm error -> static", "opencost", "", "", 500, 0.031, 0.004, "static"},
		{"aws stub -> static", "aws", "", "", 0, 0.031, 0.004, "static"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Config{PricingMode: c.mode, PriceCPUCoreHr: 0.031, PriceGBHr: 0.004}
			p := New(cfg, vmStub(t, c.cpu, c.ram, c.status))
			if p.Source() != "static" || p.CPUCoreHr() != 0.031 || p.GBHr() != 0.004 {
				t.Fatalf("initial basis must be static config: %v %v %v", p.Source(), p.CPUCoreHr(), p.GBHr())
			}
			p.refresh(context.Background())
			if p.CPUCoreHr() != c.wantCPU || p.GBHr() != c.wantGB || p.Source() != c.wantSrc {
				t.Fatalf("got cpu=%v gb=%v src=%s, want %v %v %s", p.CPUCoreHr(), p.GBHr(), p.Source(), c.wantCPU, c.wantGB, c.wantSrc)
			}
		})
	}
}

func TestPricingRunStopsOnCancel(t *testing.T) {
	p := New(config.Config{PricingMode: "static", PriceCPUCoreHr: 1, PriceGBHr: 1}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx, 1<<30); close(done) }()
	<-done
}
