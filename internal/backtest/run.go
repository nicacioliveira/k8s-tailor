package backtest

import (
	"fmt"
	"strings"
)

// StrategyNames are the strategies Factories understands, in report order.
// k8s-tailor-rollout (the template path) is also understood, and runs in "all"
// only on traces whose resizes restart containers (see Disruption).
var StrategyNames = []string{"static", "krr", "vpa", "k8s-tailor", "k8s-tailor-rollout"}

// Factories resolves strategy names (comma-separated list or "all").
func Factories(spec string, eng EngineConfig, krr KRRConfig, vpa VPAConfig) ([]Factory, error) {
	names := StrategyNames
	if s := strings.TrimSpace(spec); s != "" && s != "all" {
		names = strings.Split(s, ",")
	}
	var out []Factory
	hasStatic := false
	for _, n := range names {
		switch strings.TrimSpace(n) {
		case "static":
			out = append(out, NewStatic)
			hasStatic = true
		case "krr":
			out = append(out, NewKRR(krr))
		case "vpa":
			out = append(out, NewVPA(vpa))
		case "k8s-tailor", "engine", "tailor":
			out = append(out, NewEngine(eng))
		case "k8s-tailor-rollout":
			out = append(out, NewEngineRollout(eng))
		default:
			return nil, fmt.Errorf("unknown strategy %q (known: %s)", n, strings.Join(StrategyNames, ","))
		}
	}
	if !hasStatic {
		// static is the cost reference; always run it first.
		out = append([]Factory{NewStatic}, out...)
	}
	return out, nil
}

// RunAll replays every trace and assembles a report.
func RunAll(traces []*Trace, factories []Factory, opt Options) ([]*Result, error) {
	var out []*Result
	for _, tr := range traces {
		r, err := Run(tr, factories, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
