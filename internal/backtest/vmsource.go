package backtest

import (
	"context"
	"fmt"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// VMQuery selects one workload container's pods in VictoriaMetrics (or any
// Prometheus-compatible query_range API).
type VMQuery struct {
	Namespace string
	Workload  string // label for the report only
	PodRegex  string // e.g. "api-7d9f.*"
	Container string
	// Job pins the usage series to one scrape job (the engine's VM_JOB, default
	// "kubelet-resource"); "" = no job filter.
	Job     string
	Start   time.Time
	End     time.Time
	StepSec int64
	Initial Resources // the resources the workload ran with (not in the metrics)
}

// LoadFromVM fetches per-pod CPU (rate of container_cpu_usage_seconds_total,
// 5m window — the same expression the engine uses) and memory working set,
// reusing internal/vm, and returns them as a Trace.
func LoadFromVM(ctx context.Context, c *vm.Client, q VMQuery) (*Trace, error) {
	if q.StepSec <= 0 {
		q.StepSec = 300
	}
	sel := vm.WithJob(fmt.Sprintf(`{namespace=%q,pod=~%q,container=%q}`, q.Namespace, q.PodRegex, q.Container), q.Job)
	cpu, err := c.QueryRange(ctx, `sum by (pod) (rate(container_cpu_usage_seconds_total`+sel+`[5m]))`, q.Start, q.End, time.Time{}, int(q.StepSec))
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}
	mem, err := c.QueryRange(ctx, fmt.Sprintf(`max by (pod) (max_over_time(container_memory_working_set_bytes%s[%ds]))`, sel, int(q.StepSec)), q.Start, q.End, time.Time{}, int(q.StepSec))
	if err != nil {
		return nil, fmt.Errorf("mem query: %w", err)
	}
	type pt struct{ cpu, mem float64 }
	byKey := map[string]map[int64]*pt{}
	add := func(series []vm.Series, set func(p *pt, v float64)) {
		for _, s := range series {
			pod := s.Metric["pod"]
			if pod == "" {
				continue
			}
			if byKey[pod] == nil {
				byKey[pod] = map[int64]*pt{}
			}
			for _, x := range s.Samples {
				t := int64(x.T)
				p := byKey[pod][t]
				if p == nil {
					p = &pt{cpu: -1, mem: -1}
					byKey[pod][t] = p
				}
				set(p, x.V)
			}
		}
	}
	add(cpu, func(p *pt, v float64) { p.cpu = v })
	add(mem, func(p *pt, v float64) { p.mem = v })
	var recs []Record
	for pod, pts := range byKey {
		for t, p := range pts {
			if p.cpu < 0 || p.mem < 0 {
				continue // need both resources at a point
			}
			recs = append(recs, Record{Timestamp: t, Namespace: q.Namespace, Workload: q.Workload, Pod: pod, Container: q.Container, CPU: p.cpu, Mem: p.mem})
		}
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("no samples for %s in %s", sel, q.Namespace)
	}
	trs, err := BuildTraces(recs, q.StepSec, q.Initial)
	if err != nil {
		return nil, err
	}
	return trs[0], nil
}
