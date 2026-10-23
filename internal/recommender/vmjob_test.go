package recommender

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Every usage query (container_* series) must be pinned to cfg.VMJob, so a
// series with the same metric name and labels exposed by another scrape job
// (any pod a fleet scraper picks up) can't feed the recommendation. Empty
// VMJob = no job matcher at all (own Prometheus with other job names).
//
// The optional CFS throttling query reads cAdvisor series, which live in a
// different scrape job (/metrics/cadvisor): it is pinned to cfg.VMThrottleJob
// on both series, independently of VMJob ("" = no job matcher).
func TestUsageQueriesPinScrapeJob(t *testing.T) {
	for _, tc := range []struct{ job, thrJob string }{
		{"kubelet-resource", "kubelet-cadvisor"}, {"", ""}, {"kubelet-resource", ""}, {"", "cadvisor"},
	} {
		job, thrJob := tc.job, tc.thrJob
		var mu sync.Mutex
		var queries []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queries = append(queries, r.URL.Query().Get("query"))
			mu.Unlock()
			// Constant usage so Refresh yields a recommendation (and therefore
			// a namespace for ClusterUsageSeries to query).
			now := time.Now().Unix()
			var vals []string
			for i := 0; i < 12; i++ {
				vals = append(vals, fmt.Sprintf(`[%d,"%s"]`, now-int64(i*300), "0.05"))
			}
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"apps","pod":"web-abc-x","container":"app"},"values":[%s]}]}}`,
				strings.Join(vals, ","))
		}))
		cfg := config.Config{MinSamples: 1, Window: 2 * time.Hour, HalfLife: time.Hour, CPUQuant: 0.9, MemQuant: 0.95,
			Margin: 0.15, VMJob: job, VMThrottleJob: thrJob, TrendWindow: time.Hour, TrendStepSec: 60}
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{reqCtr("app", "1", "512Mi")}}}}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc-x", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{reqCtr("app", "1", "512Mi")}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable}}
		kc := k8s.NewFromClients(fake.NewSimpleClientset(dep, rs, pod), dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{{Group: "serving.knative.dev", Version: "v1", Resource: "services"}: "ServiceList"}), nil)
		pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
			{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
			{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
			{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
		}), cfg)
		pol.Refresh(context.Background())
		e := NewEngine(cfg, vm.New(srv.URL), kc, pol, pricing.New(cfg, nil))
		ctx := context.Background()
		_ = e.Refresh(ctx)
		_, _ = e.ClusterUsageSeries(ctx, time.Time{}, time.Time{}, 0)
		_ = e.DetailSeries(ctx, "apps", "Deployment", "web", "app", time.Time{}, time.Time{}, 0)
		srv.Close()

		var usage, throttle int
		for _, q := range queries {
			if strings.Contains(q, "container_cpu_cfs_throttled_periods_total") {
				// The optional throttling query reads the cAdvisor job's series.
				throttle++
				if thrJob != "" && strings.Count(q, `job="`+thrJob+`"`) != 2 {
					t.Errorf("VMThrottleJob=%q: throttling query not pinned on both series: %s", thrJob, q)
				}
				if thrJob == "" && strings.Contains(q, "job=") {
					t.Errorf("VMThrottleJob empty: throttling query still filters by job: %s", q)
				}
				continue
			}
			if !strings.Contains(q, "container_cpu_usage_seconds_total") && !strings.Contains(q, "container_memory_working_set_bytes") {
				continue
			}
			usage++
			has := strings.Contains(q, `job=`)
			if job != "" && !strings.Contains(q, `job="`+job+`"`) {
				t.Errorf("VMJob=%q: usage query without the job matcher: %s", job, q)
			}
			if job == "" && has {
				t.Errorf("VMJob empty: query still filters by job: %s", q)
			}
		}
		// fetchAndCollapse (2) + ClusterUsageSeries (2) + DetailSeries (2).
		if throttle != 1 {
			t.Fatalf("VMJob=%q: %d throttling queries, want 1 per namespace refresh", job, throttle)
		}
		if usage < 6 {
			t.Fatalf("VMJob=%q: only %d usage queries observed (%v) — test not exercising every path", job, usage, queries)
		}
	}
}
