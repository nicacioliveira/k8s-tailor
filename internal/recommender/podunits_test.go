package recommender

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// A pod that was seen alive as part of a ksvc / group unit keeps its usage series
// attributed to that unit after it is gone (scale to zero, a revision replaced,
// eviction), for the usage window. Before, such a pod's series was dropped
// because a deleted pod no longer carries the labels that name its unit.
func TestPodUnitCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	e := &Engine{cfg: config.Config{Window: 7 * 24 * time.Hour}}
	e.SetClock(func() time.Time { return now })
	ksvc := k8s.Owner{Kind: "ksvc", Name: "web"}
	group := k8s.Owner{Kind: "group", Name: "pool"}

	live := &k8s.ClusterSnapshot{Ownership: map[string]k8s.Owner{
		"apps/web-00001-deployment-abc": ksvc,
		"apps/sandbox-1":                group,
		"apps/api-7d9c5-xk2lp":          {Kind: "Deployment", Name: "api"}, // resolved by name, not cached
	}}
	e.rememberUnits(live)

	gone := &k8s.ClusterSnapshot{Ownership: map[string]k8s.Owner{}}
	of := e.ownerOf(gone)
	for pod, want := range map[string]k8s.Owner{"web-00001-deployment-abc": ksvc, "sandbox-1": group} {
		if got, ok := of("apps", pod); !ok || got != want {
			t.Errorf("%s after it is gone: %v %v, want %v", pod, got, ok, want)
		}
	}
	if _, ok := of("apps", "api-7d9c5-xk2lp"); ok {
		t.Error("a Deployment pod must stay name-resolved, not cached")
	}
	if _, ok := of("other", "sandbox-1"); ok {
		t.Error("cache is namespaced")
	}

	// A live pod always wins over the cache.
	moved := &k8s.ClusterSnapshot{Ownership: map[string]k8s.Owner{"apps/sandbox-1": {Kind: "group", Name: "other-pool"}}}
	if got, _ := e.ownerOf(moved)("apps", "sandbox-1"); got.Name != "other-pool" {
		t.Errorf("live ownership lost to the cache: %v", got)
	}

	// A pod dropped from the snapshot as a stale cohort stays dropped: its idle
	// usage must not dilute the group.
	stale := &k8s.ClusterSnapshot{Ownership: map[string]k8s.Owner{}, Stale: map[string]bool{"apps/sandbox-1": true}}
	e.rememberUnits(stale)
	if _, ok := e.ownerOf(gone)("apps", "sandbox-1"); ok {
		t.Error("stale-cohort pod still attributed from the cache")
	}

	// TTL = the usage window since the pod was last seen alive.
	now = now.Add(7*24*time.Hour - time.Minute)
	e.rememberUnits(gone)
	if _, ok := e.ownerOf(gone)("apps", "web-00001-deployment-abc"); !ok {
		t.Error("entry expired before the window ended")
	}
	now = now.Add(2 * time.Minute)
	e.rememberUnits(gone)
	if _, ok := e.ownerOf(gone)("apps", "web-00001-deployment-abc"); ok {
		t.Error("entry outlived the usage window")
	}
	if n := e.podUnitCount(); n != 0 {
		t.Errorf("cache not pruned: %d entries", n)
	}
}

// End to end through the series collapse: the series of a pod that is gone stays
// in its unit thanks to the cache, and is dropped without it.
func TestFetchAndCollapseUsesPodUnitCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		vals := []string{fmt.Sprintf(`[%d,"0.05"]`, now-300), fmt.Sprintf(`[%d,"0.05"]`, now)}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"apps","pod":"sandbox-1","container":"app"},"values":[%s]}]}}`,
			strings.Join(vals, ","))
	}))
	defer srv.Close()
	cfg := config.Config{Window: 7 * 24 * time.Hour, MinSamples: 1}
	e := NewEngine(cfg, vm.New(srv.URL), nil, nil, pricing.New(cfg, nil))
	unit := k8s.Owner{Kind: "group", Name: "pool"}
	e.rememberUnits(&k8s.ClusterSnapshot{Ownership: map[string]k8s.Owner{"apps/sandbox-1": unit}})
	gone := &k8s.ClusterSnapshot{Ownership: map[string]k8s.Owner{}}

	end := time.Now()
	cpu, _, _, err := e.fetchAndCollapse(context.Background(), "apps", end.Add(-time.Hour), end, e.ownerOf(gone))
	if err != nil {
		t.Fatal(err)
	}
	if len(cpu["group/pool"]["app"]) == 0 {
		t.Fatalf("series of the departed pod lost its unit: %v", cpu)
	}
	cpu, _, _, _ = e.fetchAndCollapse(context.Background(), "apps", end.Add(-time.Hour), end, gone.OwnerOf)
	if len(cpu) != 0 {
		t.Fatalf("without the cache the series should be dropped, got %v", cpu)
	}
}
