// Package pricing resolves $/core-hr and $/GB-hr for the waste calculation.
// Modes: static (fixed), opencost (read node_*_hourly_cost from the same VM —
// spot-aware, no AWS creds), aws (stub; needs the SDK for Price List + spot).
package pricing

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

type Provider struct {
	mode      string
	vm        *vm.Client
	staticCPU float64
	staticGB  float64

	mu  sync.RWMutex
	cpu float64
	gb  float64
	src string
}

func New(cfg config.Config, vmc *vm.Client) *Provider {
	p := &Provider{
		mode: cfg.PricingMode, vm: vmc,
		staticCPU: cfg.PriceCPUCoreHr, staticGB: cfg.PriceGBHr,
		cpu: cfg.PriceCPUCoreHr, gb: cfg.PriceGBHr, src: "static",
	}
	return p
}

func (p *Provider) CPUCoreHr() float64 { p.mu.RLock(); defer p.mu.RUnlock(); return p.cpu }
func (p *Provider) GBHr() float64      { p.mu.RLock(); defer p.mu.RUnlock(); return p.gb }
func (p *Provider) Source() string     { p.mu.RLock(); defer p.mu.RUnlock(); return p.src }

func (p *Provider) Run(ctx context.Context, every time.Duration) {
	p.refresh(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh(ctx)
		}
	}
}

func (p *Provider) refresh(ctx context.Context) {
	cpu, gb, src := p.staticCPU, p.staticGB, "static"

	if p.mode == "opencost" {
		// OpenCost publishes per-node hourly cost (already spot-aware) to Prometheus/VM.
		// Cluster averages give a $/core-hr and $/GB-hr basis without AWS credentials.
		c, okc := p.vm.QueryScalar(ctx, "avg(node_cpu_hourly_cost)")
		g, okg := p.vm.QueryScalar(ctx, "avg(node_ram_hourly_cost)")
		if okc && c > 0 && okg && g > 0 {
			cpu, gb, src = c, g, "opencost"
		} else {
			log.Printf("pricing: opencost metrics unavailable, using static")
		}
	} else if p.mode == "aws" {
		// TODO: AWS Price List (on-demand) + DescribeSpotPriceHistory via the SDK,
		// keyed by node instance-type/region/capacity-type. Static until then.
		log.Printf("pricing: aws mode not implemented, using static")
	}

	p.mu.Lock()
	p.cpu, p.gb, p.src = cpu, gb, src
	p.mu.Unlock()
}
