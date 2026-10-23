package recommender

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// P1-12: the learned OOM floor lived only in memory. After a restart / leader
// failover the OOMKilled pod has usually been replaced (its lastState is gone),
// so the floor was lost and the next Down could re-shrink into the same OOM.
// It is now persisted on the workload (annotation) and rebuilt from it.
func TestOOMFloorSurvivesRestart(t *testing.T) {
	cs := fake.NewSimpleClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps-x"}})
	kc := k8s.NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	cfg := config.Config{OOMGrowthFactor: 1.5, OOMFloorTTL: 30 * 24 * time.Hour}
	now := time.Now()

	// Process 1 learns an OOM and persists it.
	e1 := &Engine{cfg: cfg, k8s: kc, oomFloor: map[string]oomFloorEntry{}}
	changed := e1.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: now}})
	wl := k8s.Workload{Namespace: "apps-x", Kind: "Deployment", Name: "web"}
	e1.persistOOM(context.Background(), []k8s.Workload{wl}, changed, func(k8s.Workload) bool { return true })

	d, err := cs.AppsV1().Deployments("apps-x").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Annotations[k8s.AnnOOMFloor] == "" {
		t.Fatalf("floor not persisted: %v", d.Annotations)
	}

	// Process 2 (fresh memory, the OOMKilled pod is gone) rebuilds it.
	e2 := &Engine{cfg: cfg, k8s: kc, oomFloor: map[string]oomFloorEntry{}}
	wl.Annotations = d.Annotations
	e2.seedOOM([]k8s.Workload{wl})
	if got := e2.oomFloorFor(oomKey); got != 1500 {
		t.Fatalf("rebuilt floor = %v, want 1500", got)
	}

	// An expired persisted floor is not resurrected.
	e3 := &Engine{cfg: config.Config{OOMGrowthFactor: 1.5, OOMFloorTTL: time.Nanosecond}, oomFloor: map[string]oomFloorEntry{}}
	e3.seedOOM([]k8s.Workload{wl})
	if got := e3.oomFloorFor(oomKey); got != 0 {
		t.Fatalf("expired floor resurrected: %v", got)
	}
}

// Recommend-only workloads are never written to.
func TestOOMFloorNotPersistedWhenNotActuating(t *testing.T) {
	cs := fake.NewSimpleClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps-x"}})
	kc := k8s.NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	e := &Engine{cfg: config.Config{OOMGrowthFactor: 1.5, OOMFloorTTL: time.Hour}, k8s: kc, oomFloor: map[string]oomFloorEntry{}}
	changed := e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: time.Now()}})
	e.persistOOM(context.Background(), []k8s.Workload{{Namespace: "apps-x", Kind: "Deployment", Name: "web"}}, changed,
		func(k8s.Workload) bool { return false })
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			t.Fatalf("recommend-only workload was patched: %v", a)
		}
	}
}

// A repeated observation of the same OOM (the pod's lastState persists across
// refreshes) is not a change: no annotation write per refresh.
func TestLearnOOMSameObservationNotChanged(t *testing.T) {
	e := newOOMEngine(1.5, time.Hour)
	at := time.Now()
	if ch := e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: at}}); !ch[oomKey] {
		t.Fatal("first observation must be a change")
	}
	if ch := e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: at.UTC()}}); len(ch) != 0 {
		t.Fatalf("same OOM reported as changed: %v", ch)
	}
}

// LimitCapped (P2): set when the request wanted more memory than the current
// limit allows (it used to be hard-wired false).
func TestRecommendLimitCapped(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(900*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	e := testEngine(1)
	rec := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 256 * mib, MemLimit: 512 * mib}, cpu, mem, 0, testResolved(), 0)
	if !rec.LimitCapped {
		t.Fatal("LimitCapped = false although the request was capped at the 512Mi limit")
	}
	rec = e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 256 * mib, MemLimit: 4096 * mib}, cpu, mem, 0, testResolved(), 0)
	if rec.LimitCapped {
		t.Fatal("LimitCapped = true with ample limit")
	}
}
