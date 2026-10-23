package webhook

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// historyVM answers the usage queries with one series per pod: pod name ->
// number of 300 s-spaced points ending now (CPU 0.05 cores, memory 64Mi).
// Any other query (throttling) gets an empty result.
func historyVM(t *testing.T, now time.Time, pods map[string]int) *vm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		val := ""
		switch {
		case strings.Contains(q, "container_cpu_usage_seconds_total"):
			val = "0.05"
		case strings.Contains(q, "container_memory_working_set_bytes"):
			val = fmt.Sprint(64 * mib)
		}
		var series []string
		if val != "" {
			for pod, n := range pods {
				var vals []string
				for i := 0; i < n; i++ {
					vals = append(vals, fmt.Sprintf(`[%d,"%s"]`, now.Unix()-int64(i*300), val))
				}
				series = append(series, fmt.Sprintf(`{"metric":{"namespace":"apps","pod":%q,"container":"app"},"values":[%s]}`,
					pod, strings.Join(vals, ",")))
			}
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(series, ","))
	}))
	t.Cleanup(srv.Close)
	return vm.New(srv.URL)
}

// freshEngine builds a NEW engine (as after a restart: no in-memory state)
// over a cluster with Deployment web (ReplicaSets web-abc = current and
// web-old = a previous revision, scaled to 0) and the running pods given, and
// runs exactly ONE refresh.
func freshEngine(t *testing.T, now time.Time, running []string, history map[string]int) (*recommender.Engine, *k8s.Client) {
	t.Helper()
	objs := []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "500m", "memory", "256Mi")},
			}}}}}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-old", Namespace: "apps",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}},
	}
	for _, name := range running {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", OwnerReferences: rsOwner},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "500m", "memory", "256Mi")},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
		})
	}
	ksvc := schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}
	kc := k8s.NewFromClients(fake.NewSimpleClientset(objs...),
		dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{ksvc: "ServiceList"}), nil)
	cfg := config.Config{MinSamples: 6, Window: 2 * time.Hour, HalfLife: time.Hour, CPUQuant: 0.9, MemQuant: 0.95, Margin: 0.15}
	webhookPolicy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailor.k8s-tailor.dev/v1alpha1", "kind": "TailorPolicy",
		"metadata": map[string]any{"name": "wh"},
		"spec":     map[string]any{"mode": "Webhook", "selector": map[string]any{"namespaces": []any{"apps"}}},
	}}
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs, webhookPolicy), cfg)
	pol.Refresh(context.Background())
	eng := recommender.NewEngine(cfg, historyVM(t, now, history), kc, pol, pricing.New(cfg, nil))
	eng.SetClock(func() time.Time { return now })
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return eng, kc
}

// After an engine restart the webhook must be able to mutate as soon as the
// FIRST refresh completed, using the history already in VictoriaMetrics.
func TestWebhookMutatesAfterFirstRefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name    string
		history map[string]int
	}{
		// Running pods with a full history: the plain restart case.
		{"running pods carry the history", map[string]int{"web-abc-x": 12}},
		// The pods that produced the history were replaced (kubectl delete pod, a
		// rollout, a node drain): their series are still in VM, under pod names
		// that no longer exist. Same ReplicaSet and previous ReplicaSet.
		{"history from replaced pods (same ReplicaSet)", map[string]int{"web-abc-x": 1, "web-abc-g7x2k": 12}},
		{"history from a previous ReplicaSet", map[string]int{"web-abc-x": 1, "web-old-zz9k2": 12}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, kc := freshEngine(t, now, []string{"web-abc-x"}, tc.history)
			en, ok := eng.Get("apps", "Deployment", "web", "app")
			if !ok {
				t.Fatal("no entry after the first refresh")
			}
			if !en.Rec.Confident {
				t.Fatalf("not confident after the first refresh (samples=%d): history of replaced pods ignored", en.Rec.Signals.Samples)
			}
			wh := New(config.Config{}, eng, kc)
			res := call(t, wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "500m", "memory", "256Mi")}))
			if len(res.ops) != 1 {
				t.Fatalf("webhook did not mutate after the first refresh: ops=%v", res.ops)
			}
		})
	}
}

// A series whose pod name maps to no known ReplicaSet of the namespace is never
// attributed (a bare pod that is gone, or another workload's pod).
func TestRefreshIgnoresUnattributableGonePods(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	eng, _ := freshEngine(t, now, []string{"web-abc-x"}, map[string]int{"web-abc-x": 1, "other-xyz-b7k2m": 12, "web-b7k2m": 12, "web-abc-gone1": 12})
	en, ok := eng.Get("apps", "Deployment", "web", "app")
	if !ok {
		t.Fatal("no entry")
	}
	if en.Rec.Confident || en.Rec.Signals.Samples != 1 {
		t.Fatalf("unattributable series were folded in: confident=%v samples=%d", en.Rec.Confident, en.Rec.Signals.Samples)
	}
}

// Before the first refresh completes the webhook has nothing to apply: it
// admits unmodified and counts it as warming_up (never silent).
func TestWebhookCountsWarmingUp(t *testing.T) {
	f := newFixture(t, nil)
	before := metrics.WebhookSkips.Value("warming_up")
	res := call(t, f.wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}))
	if len(res.ops) != 0 || !res.allowed {
		t.Fatalf("warming-up webhook mutated: %v", res.ops)
	}
	if got := metrics.WebhookSkips.Value("warming_up") - before; got != 1 {
		t.Fatalf("tailor_webhook_skipped_total{reason=warming_up} += %v, want 1", got)
	}
}

// The warm cache closes the gap after a restart: the leader writes what the
// webhook can serve, a NEW engine loads it before its first refresh, and a pod
// created in that gap is sized instead of admitted unmodified. The loaded entries
// are invisible to the reconciler (Get), and the first real refresh replaces them.
func TestWebhookServesWarmCacheBeforeFirstRefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	leaderEng, kc := freshEngine(t, now, []string{"web-abc-x"}, map[string]int{"web-abc-x": 12})
	cfg := config.Config{WarmCache: true, WarmTTL: 15 * time.Minute, SelfNamespace: "k8s-tailor"}
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), cfg)
	writer := recommender.NewEngine(cfg, nil, kc, pol, pricing.New(cfg, nil))
	writer.SetClock(func() time.Time { return now })
	// Hand the leader's refreshed entries to an engine configured for the cache.
	en, ok := leaderEng.Get("apps", "Deployment", "web", "app")
	if !ok || !en.Rec.Confident {
		t.Fatalf("precondition: a confident webhook entry after the refresh (ok=%v)", ok)
	}
	writer.Seed(nil, map[string]recommender.Entry{recommender.Key("apps", "Deployment", "web", "app"): en})
	if err := writer.SaveWarm(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A restarted replica: no refresh yet.
	restarted := recommender.NewEngine(cfg, nil, kc, pol, pricing.New(cfg, nil))
	restarted.SetClock(func() time.Time { return now.Add(30 * time.Second) })
	if n, err := restarted.LoadWarm(context.Background()); err != nil || n != 1 {
		t.Fatalf("LoadWarm: %d entries, err %v", n, err)
	}
	before := metrics.WebhookSkips.Value("warming_up")
	wh := New(config.Config{}, restarted, kc)
	res := call(t, wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "500m", "memory", "256Mi")}))
	if len(res.ops) != 1 {
		t.Fatalf("webhook did not size the pod from the warm cache: ops=%v", res.ops)
	}
	if got := metrics.WebhookSkips.Value("warming_up") - before; got != 0 {
		t.Fatalf("warming_up += %v with a warm cache loaded, want 0", got)
	}
	if _, ok := restarted.Get("apps", "Deployment", "web", "app"); ok {
		t.Fatal("the reconciler can see an entry loaded from the cache")
	}

	// An engine that finds nothing (first install, expired TTL) still reports warming_up.
	cold := recommender.NewEngine(cfg, nil, k8s.NewFromClients(fake.NewSimpleClientset(), nil, nil), pol, pricing.New(cfg, nil))
	cold.SetClock(func() time.Time { return now })
	_, _ = cold.LoadWarm(context.Background())
	before = metrics.WebhookSkips.Value("warming_up")
	res = call(t, New(config.Config{}, cold, kc), review(t, podSpec{owners: rsOwner, req: rl("cpu", "500m", "memory", "256Mi")}))
	if len(res.ops) != 0 || metrics.WebhookSkips.Value("warming_up")-before != 1 {
		t.Fatalf("a cold engine must still admit unmodified and count warming_up (ops=%v)", res.ops)
	}
}
