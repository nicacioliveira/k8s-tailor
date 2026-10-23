package server

import (
	"strings"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

func TestMetricsOperationalSeries(t *testing.T) {
	s, eng := newTestServer(t, config.Config{})
	mib := 1024.0 * 1024
	eng.Seed([]recommender.Recommendation{
		{Namespace: "a", Kind: "Deployment", Workload: "w1", Container: "app", Mode: "InPlace", Confident: true,
			Applied: recommender.Resources{CPURequest: 1, MemRequest: 512 * mib}, Recommended: recommender.Resources{CPURequest: 0.2, MemRequest: 128 * mib}},
		{Namespace: "a", Kind: "Deployment", Workload: "w1", Container: "sidecar", Mode: "InPlace", Confident: true,
			Applied: recommender.Resources{CPURequest: 0.1, MemRequest: 64 * mib}, Recommended: recommender.Resources{CPURequest: 0.1, MemRequest: 64 * mib}},
		{Namespace: "a", Kind: "Deployment", Workload: "w2", Container: "app", Mode: "Recommend", Confident: false},
		{Namespace: "b", Kind: "StatefulSet", Workload: "db", Container: "pg", Mode: "Recommend", Confident: true, OOMRisk: true},
		{Namespace: "b", Kind: "Deployment", Workload: "tiny", Container: "app", Mode: "Off", Confident: true,
			Applied: recommender.Resources{CPURequest: 0.1, MemRequest: 64 * mib}, Recommended: recommender.Resources{CPURequest: 1, MemRequest: 64 * mib}},
	}, nil)
	rr := do(s.Handler(), "GET", "/metrics", "", "")
	out := rr.Body.String()
	for _, want := range []string{
		// workloads (not containers) by effective mode
		`tailor_workloads{mode="InPlace"} 1`,
		`tailor_workloads{mode="Recommend"} 2`,
		`tailor_workloads{mode="Off"} 1`,
		// container recommendations by status
		`tailor_recommendations{status="overprovisioned"} 1`,
		`tailor_recommendations{status="rightsized"} 1`,
		`tailor_recommendations{status="learning"} 1`,
		`tailor_recommendations{status="oom_risk"} 1`,
		`tailor_recommendations{status="underprovisioned"} 1`,
		"tailor_paused 0",
		// operational families from the shared registry are always exposed
		"# TYPE tailor_resizes_total counter",
		"# TYPE tailor_refresh_duration_seconds histogram",
		"# TYPE tailor_query_errors_total counter",
		"# TYPE tailor_leader gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// One HELP/TYPE header per family (Prometheus rejects duplicates).
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "# TYPE ") {
			if seen[l] {
				t.Errorf("duplicate %q", l)
			}
			seen[l] = true
		}
	}
}

func TestRecStatus(t *testing.T) {
	r := recommender.Recommendation{Confident: true, Applied: recommender.Resources{CPURequest: 1, MemRequest: 100}, Recommended: recommender.Resources{CPURequest: 1.05, MemRequest: 100}}
	if got := recStatus(r); got != "rightsized" {
		t.Fatalf("got %s", got)
	}
	r.Recommended.MemRequest = 50
	if got := recStatus(r); got != "overprovisioned" {
		t.Fatalf("got %s", got)
	}
	r.Recommended.CPURequest = 2 // one resource short wins: under beats over
	if got := recStatus(r); got != "underprovisioned" {
		t.Fatalf("got %s", got)
	}
}
