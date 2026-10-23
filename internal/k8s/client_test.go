package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func TestNsMatch(t *testing.T) {
	cases := []struct {
		globs []string
		ns    string
		want  bool
	}{
		{nil, "anything", true},          // empty = all
		{[]string{"prod"}, "prod", true}, // exact
		{[]string{"prod"}, "dev", false}, // miss
		{[]string{"apps-*"}, "apps-a", true},
		{[]string{"apps-*"}, "other", false},
		{[]string{"a", "b*"}, "bee", true}, // any-of
	}
	for _, c := range cases {
		if got := nsMatch(c.globs, c.ns); got != c.want {
			t.Errorf("nsMatch(%v,%q)=%v want %v", c.globs, c.ns, got, c.want)
		}
	}
}

func TestSetGroupKeysKnativeMerge(t *testing.T) {
	c := &Client{}
	// A user group targeting the Knative service label should MERGE its cohort into
	// the built-in key (kept first, kind ksvc), not be shadowed by it.
	c.SetGroupKeys([]GroupKey{
		{Label: lblKnativeService, CohortLabel: "serving.knative.dev/revision", CohortSelect: "latest"},
		{Label: "app", CohortSelect: "all"}, // a second, generic group
	})
	if len(c.groupKeys) < 2 {
		t.Fatalf("want >=2 keys, got %d", len(c.groupKeys))
	}
	kn := c.groupKeys[0]
	if kn.Label != lblKnativeService || kn.Kind != "ksvc" {
		t.Fatalf("first key should be the knative key: %+v", kn)
	}
	if kn.CohortLabel != "serving.knative.dev/revision" || kn.CohortSelect != "latest" {
		t.Fatalf("cohort not merged into knative key: %+v", kn)
	}
	// The generic key gets the default kind "group".
	if c.groupKeys[1].Label != "app" || c.groupKeys[1].Kind != "group" {
		t.Fatalf("generic key wrong: %+v", c.groupKeys[1])
	}
}

func TestMatchGroupKeyNamespaceScope(t *testing.T) {
	c := &Client{}
	c.SetGroupKeys([]GroupKey{{Label: "app", Kind: "group", Namespaces: []string{"prod"}}})
	if _, _, ok := c.matchGroupKey("dev", map[string]string{"app": "x"}); ok {
		t.Fatal("key scoped to prod should not match ns dev")
	}
	if _, v, ok := c.matchGroupKey("prod", map[string]string{"app": "x"}); !ok || v != "x" {
		t.Fatalf("key should match ns prod: ok=%v v=%q", ok, v)
	}
}

func TestMatchGroupKeyNamespaceCollapse(t *testing.T) {
	c := &Client{}
	// "namespace" sentinel: every workload in a matching namespace folds into one
	// unit keyed by the namespace, regardless of labels (pods share none here).
	c.SetGroupKeys([]GroupKey{{Label: "namespace", Kind: "group", Namespaces: []string{"apps-*"}}})
	k, v, ok := c.matchGroupKey("apps-demo", map[string]string{"app": "apps-demo--qimbxo"})
	if !ok || v != "apps-demo" || k.Kind != "group" {
		t.Fatalf("namespace collapse should key by namespace: ok=%v v=%q kind=%q", ok, v, k.Kind)
	}
	// a different STS in the SAME namespace collapses to the SAME unit
	if _, v2, _ := c.matchGroupKey("apps-demo", map[string]string{"app": "apps-demo--abc123"}); v2 != v {
		t.Fatalf("two STSs in one namespace must share a unit: %q != %q", v2, v)
	}
	// out-of-scope namespace does not match
	if _, _, ok := c.matchGroupKey("kube-system", map[string]string{"app": "x"}); ok {
		t.Fatal("namespace key scoped to apps-* should not match kube-system")
	}
}

func mkPod(ns, name string, labels map[string]string, created time.Time) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(created),
	}}
}

func TestStaleCohortLatest(t *testing.T) {
	c := &Client{}
	c.SetGroupKeys([]GroupKey{{Label: "app", Kind: "group", CohortLabel: "version", CohortSelect: "latest"}})

	base := time.Unix(1_700_000_000, 0)
	pods := []corev1.Pod{
		mkPod("ns", "old-a", map[string]string{"app": "demo", "version": "v1"}, base),
		mkPod("ns", "old-b", map[string]string{"app": "demo", "version": "v1"}, base.Add(time.Second)),
		mkPod("ns", "new-a", map[string]string{"app": "demo", "version": "v2"}, base.Add(time.Minute)), // newest
		// a different group value must be unaffected
		mkPod("ns", "other", map[string]string{"app": "svc2", "version": "r1"}, base),
	}
	stale := c.staleCohortPods(context.Background(), pods)

	if !stale["ns/old-a"] || !stale["ns/old-b"] {
		t.Fatalf("v1 pods should be stale (superseded cohort): %v", stale)
	}
	if stale["ns/new-a"] {
		t.Fatal("newest cohort pod must NOT be stale")
	}
	if stale["ns/other"] {
		t.Fatal("a single-cohort group must have no stale pods")
	}
}

func TestStaleCohortActiveByTraffic(t *testing.T) {
	// A canary: rev-2 (90%) + rev-1 (10%) serve traffic; rev-0 (0%) is idle. The
	// active set must keep BOTH serving revisions and drop only the idle one — which
	// "latest" (newest-only) would get wrong. status.traffic drives it.
	ksvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.knative.dev/v1",
		"kind":       "Service",
		"metadata":   map[string]any{"namespace": "ns", "name": "svc"},
		"status": map[string]any{"traffic": []any{
			map[string]any{"revisionName": "svc-rev-2", "percent": int64(90)},
			map[string]any{"revisionName": "svc-rev-1", "percent": int64(10)},
			map[string]any{"revisionName": "svc-rev-0", "percent": int64(0)},
		}},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}, ksvc)
	c := &Client{dyn: dyn}
	c.SetGroupKeys([]GroupKey{{Label: lblKnativeService, CohortSelect: "activeByTraffic", CohortLabel: lblKnativeRevision}})

	base := time.Unix(1_700_000_000, 0)
	rev := func(name, r string) corev1.Pod {
		return mkPod("ns", name, map[string]string{lblKnativeService: "svc", lblKnativeRevision: r}, base)
	}
	stale := c.staleCohortPods(context.Background(), []corev1.Pod{
		rev("p2", "svc-rev-2"), rev("p1", "svc-rev-1"), rev("p0", "svc-rev-0"),
	})
	if stale["ns/p2"] || stale["ns/p1"] {
		t.Fatalf("both serving revisions must count (canary): %v", stale)
	}
	if !stale["ns/p0"] {
		t.Fatal("the idle (no-traffic) revision must be stale")
	}
}

func TestStaleCohortAllModeNoOp(t *testing.T) {
	c := &Client{}
	// select defaults to "" (all) → nothing is ever stale.
	c.SetGroupKeys([]GroupKey{{Label: "app", Kind: "group", CohortLabel: "version"}})
	base := time.Unix(1_700_000_000, 0)
	pods := []corev1.Pod{
		mkPod("ns", "a", map[string]string{"app": "demo", "version": "v1"}, base),
		mkPod("ns", "b", map[string]string{"app": "demo", "version": "v2"}, base.Add(time.Minute)),
	}
	if got := c.staleCohortPods(context.Background(), pods); len(got) != 0 {
		t.Fatalf("cohort=all should mark nothing stale, got %v", got)
	}
}
