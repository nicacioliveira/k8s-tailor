package backtest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Report is the full, serialisable output of a backtest run.
type Report struct {
	Tool    string    `json:"tool"`
	Seed    uint64    `json:"seed"`
	Source  string    `json:"source"`
	Options Options   `json:"options"`
	Params  any       `json:"params,omitempty"`
	Results []*Result `json:"results"`
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func fmtCPU(c float64) string { return fmt.Sprintf("%.0fm", c*1000) }
func fmtMem(b float64) string { return fmt.Sprintf("%.0fMi", b/mib) }

func fmtRes(r Resources) string {
	s := fmtCPU(r.CPURequest) + " / " + fmtMem(r.MemRequest)
	switch {
	case r.CPULimit > 0 && r.MemLimit > 0:
		s += " (lim " + fmtCPU(r.CPULimit) + " / " + fmtMem(r.MemLimit) + ")"
	case r.CPULimit > 0:
		s += " (lim " + fmtCPU(r.CPULimit) + " / -)"
	case r.MemLimit > 0:
		s += " (lim " + fmtMem(r.MemLimit) + ")"
	}
	return s
}

// WriteMarkdown writes one table per workload.
func (r *Report) WriteMarkdown(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Backtest k8s-tailor vs baselines\n\n")
	fmt.Fprintf(&b, "Source: `%s` · seed `%d` · warmup %.1fh · price $%.4g/core·h, $%.4g/GiB·h (on the REQUEST)\n\n",
		r.Source, r.Seed, float64(r.Options.WarmupSec)/3600, r.Options.PriceCPUCoreHr, r.Options.PriceGBHr)
	for _, res := range r.Results {
		fmt.Fprintf(&b, "## %s\n\n", res.Workload)
		if res.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", res.Description)
		}
		fmt.Fprintf(&b, "%d pods · step %ds · evaluated window %.1f days · initial %s\n\n", res.Pods, res.StepSec, res.EvalDays, fmtRes(res.Initial))
		if res.Disruptive {
			b.WriteString("| strategy | CPU>request (pod·h) | CPU>request (%) | CPU>limit (pod·h) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | restarts/day | max not Ready | rel. cost | final |\n")
			b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
			for _, m := range res.Metrics {
				fmt.Fprintf(&b, "| %s | %.1f | %.2f | %.1f | %d | %.1f | %.3f | %.3f | %.2f | %.2f | %d | %.3f | %s |\n",
					m.Strategy, m.CPUOverRequestHours, m.CPUOverRequestPct, m.CPUOverLimitHours, m.OOMEvents, m.MemOverCeilingHours,
					m.AvgCPUWasteCores, m.AvgMemWasteGiB, m.ChangesPerDay, m.RestartsPerDay, m.MaxNotReady, m.CostRelative, fmtRes(m.Final))
			}
			b.WriteString("\n")
			continue
		}
		b.WriteString("| strategy | CPU>request (pod·h) | CPU>request (%) | CPU>limit (pod·h) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |\n")
		b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
		for _, m := range res.Metrics {
			fmt.Fprintf(&b, "| %s | %.1f | %.2f | %.1f | %d | %.1f | %.3f | %.3f | %.2f | %.3f | %s |\n",
				m.Strategy, m.CPUOverRequestHours, m.CPUOverRequestPct, m.CPUOverLimitHours, m.OOMEvents, m.MemOverCeilingHours,
				m.AvgCPUWasteCores, m.AvgMemWasteGiB, m.ChangesPerDay, m.CostRelative, fmtRes(m.Final))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
