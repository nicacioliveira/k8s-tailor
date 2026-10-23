package recommender

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// A weekly-only pattern (a Monday peak, flat otherwise) cannot be told from noise
// in the default 7-day window: there are no week-lag pairs, the daily proxy
// rejects it and the forecast is off. Given the longer series SEASON_WINDOW
// fetches for weekly policies, the real week-lag correlation accepts it and the
// request anticipates the next Monday peak.
func TestRecommendWeeklyUsesSeasonSeries(t *testing.T) {
	all := weeklyOnly(15)
	nowT := float64(SyntheticMonday) + 14*86400 + 8*3600 // Monday 08:00, one hour before... the peak starts at 08:00
	var long []Sample
	for _, s := range all {
		if s.T <= nowT {
			long = append(long, s)
		}
	}
	var short []Sample
	for _, s := range long {
		if s.T > nowT-7*86400 {
			short = append(short, s)
		}
	}
	mem := series(short[0].T, 300, len(short), constant(100*mib))
	run := func(season []Sample) Recommendation {
		res := testResolved()
		res.Season = "weekly"
		return Recommend(RecommendInput{
			Workload: testWL, Container: k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mib},
			CPU: short, Mem: mem, SeasonCPU: season, Policy: res, HalfLifeSec: 3600,
			Now: time.Unix(int64(nowT), 0), MinSamples: 1,
		})
	}
	without, with := run(nil), run(long)
	if without.Signals.CPUSeason != "off" {
		t.Fatalf("7d window: season %q, want off (no week-lag pairs, no daily cycle)", without.Signals.CPUSeason)
	}
	if with.Signals.CPUSeason != "weekly" {
		t.Fatalf("14d series: season %q, want weekly", with.Signals.CPUSeason)
	}
	if with.Recommended.CPURequest < 0.8 || without.Recommended.CPURequest > 0.4 {
		t.Fatalf("request with the long series %v (want the Monday peak, >= 0.8), without %v (want the flat level)", with.Recommended.CPURequest, without.Recommended.CPURequest)
	}
}

type vmCall struct {
	query      string
	start, end int64
}

func seasonEngine(t *testing.T, season string, cfg config.Config) (*Engine, *[]vmCall, func(time.Time)) {
	t.Helper()
	var mu sync.Mutex
	var calls []vmCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		e, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		mu.Lock()
		calls = append(calls, vmCall{q.Get("query"), s, e})
		mu.Unlock()
		var vals []string
		for ts := e; ts > e-3600 && ts > s; ts -= 300 {
			vals = append(vals, fmt.Sprintf(`[%d,"0.05"]`, ts))
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"apps","pod":"web-abc-x","container":"app"},"values":[%s]}]}}`, strings.Join(vals, ","))
	}))
	t.Cleanup(srv.Close)
	cfg.MinSamples, cfg.Window, cfg.HalfLife, cfg.CPUQuant, cfg.MemQuant, cfg.Margin = 1, 7*24*time.Hour, time.Hour, 0.9, 0.95, 0.15
	cfg.TrendWindow, cfg.TrendStepSec = time.Hour, 60
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{reqCtr("app", "1", "512Mi")}}}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc-x", Namespace: "apps", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{reqCtr("app", "1", "512Mi")}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable}}
	kc := k8s.NewFromClients(fake.NewSimpleClientset(dep, rs, pod), dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{{Group: "serving.knative.dev", Version: "v1", Resource: "services"}: "ServiceList"}), nil)
	pol1 := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailor.k8s-tailor.dev/v1alpha1", "kind": "TailorPolicy", "metadata": map[string]any{"name": "p"},
		"spec": map[string]any{"mode": "Recommend", "season": season, "selector": map[string]any{"namespaces": []any{"apps"}}},
	}}
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
	}, pol1), cfg)
	pol.Refresh(context.Background())
	e := NewEngine(cfg, vm.New(srv.URL), kc, pol, pricing.New(cfg, nil))
	now := time.Unix(1_800_000_000, 0)
	e.SetClock(func() time.Time { return now })
	return e, &calls, func(t time.Time) { now = t }
}

func spans(calls []vmCall) (n7, n14 int) {
	for _, c := range calls {
		if !strings.Contains(c.query, "container_cpu_usage_seconds_total") {
			continue
		}
		switch d := time.Duration(c.end-c.start) * time.Second; {
		case d >= 13*24*time.Hour:
			n14++
		case d >= 6*24*time.Hour:
			n7++
		}
	}
	return
}

// The longer window is fetched only for namespaces with a weekly (or both)
// policy, once per SEASON_REFRESH, and not at all when SEASON_WINDOW is 0 or not
// longer than WINDOW: the extra query load is bounded.
func TestSeasonWindowFetchIsBounded(t *testing.T) {
	ctx := context.Background()
	base := config.Config{SeasonWindow: 14 * 24 * time.Hour, SeasonRefresh: time.Hour}

	e, calls, setNow := seasonEngine(t, "weekly", base)
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if n7, n14 := spans(*calls); n7 != 1 || n14 != 1 {
		t.Fatalf("weekly policy: %d window queries and %d long queries, want 1 and 1", n7, n14)
	}
	_ = e.Refresh(ctx)
	if _, n14 := spans(*calls); n14 != 1 {
		t.Fatalf("a second refresh inside SEASON_REFRESH re-fetched the long window (%d)", n14)
	}
	setNow(time.Unix(1_800_000_000, 0).Add(61 * time.Minute))
	_ = e.Refresh(ctx)
	if _, n14 := spans(*calls); n14 != 2 {
		t.Fatalf("after SEASON_REFRESH the long window was fetched %d times in total, want 2", n14)
	}

	for _, c := range []struct {
		name   string
		season string
		cfg    config.Config
	}{
		{"daily policy", "daily", base},
		{"SEASON_WINDOW=0", "weekly", config.Config{SeasonRefresh: time.Hour}},
		{"SEASON_WINDOW not longer than WINDOW", "weekly", config.Config{SeasonWindow: 7 * 24 * time.Hour, SeasonRefresh: time.Hour}},
	} {
		e, calls, _ := seasonEngine(t, c.season, c.cfg)
		if err := e.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if _, n14 := spans(*calls); n14 != 0 {
			t.Errorf("%s: %d long queries, want 0", c.name, n14)
		}
	}
	// "both" counts as weekly.
	e, calls, _ = seasonEngine(t, "both", base)
	_ = e.Refresh(ctx)
	if _, n14 := spans(*calls); n14 != 1 {
		t.Errorf("season both: %d long queries, want 1", n14)
	}
}
