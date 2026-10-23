package recommender

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
)

var (
	ksvcRes  = schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}
	groupRes = schema.GroupVersionResource{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}
)

func uobj(apiVersion, kind, ns, name string, anns map[string]any) *unstructured.Unstructured {
	md := map[string]any{"name": name}
	if ns != "" {
		md["namespace"] = ns
	}
	if anns != nil {
		md["annotations"] = anns
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": md,
		"spec": map[string]any{"groupBy": "sandbox", "template": map[string]any{"spec": map[string]any{}}}}}
}

// A Knative Service (ksvc unit) persists its floor on the Service METADATA
// (never the template: no new revision) and is rebuilt from it after a restart.
func TestOOMFloorPersistedOnKnativeService(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{ksvcRes: "ServiceList"},
		uobj("serving.knative.dev/v1", "Service", "apps", "site", nil))
	kc := k8s.NewFromClients(fake.NewSimpleClientset(), dyn, nil)
	cfg := config.Config{OOMGrowthFactor: 1.5, OOMFloorTTL: 30 * 24 * time.Hour}
	key := "apps/ksvc/site/user-container"

	e1 := &Engine{cfg: cfg, k8s: kc, oomFloor: map[string]oomFloorEntry{}}
	changed := e1.learnOOM(map[string]k8s.OOMObservation{key: {MemBytes: 1000, At: time.Now()}})
	wl := k8s.Workload{Namespace: "apps", Kind: "ksvc", Name: "site"}
	e1.persistOOM(context.Background(), []k8s.Workload{wl}, changed, func(k8s.Workload) bool { return true })

	obj, err := dyn.Resource(ksvcRes).Namespace("apps").Get(context.Background(), "site", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetAnnotations()[k8s.AnnOOMFloor] == "" {
		t.Fatalf("floor not persisted on the ksvc: %v", obj.GetAnnotations())
	}
	if _, ok, _ := unstructured.NestedMap(obj.Object, "spec", "template", "metadata"); ok {
		t.Fatal("the ksvc template was touched (would mint a revision)")
	}

	e2 := &Engine{cfg: cfg, k8s: kc, oomFloor: map[string]oomFloorEntry{}}
	wl.Annotations = obj.GetAnnotations() // ListWorkloads reads the ksvc metadata annotations
	e2.seedOOM([]k8s.Workload{wl})
	if got := e2.oomFloorFor(key); got != 1500 {
		t.Fatalf("rebuilt ksvc floor = %v, want 1500", got)
	}
}

// A "group" unit has no object: its floor goes on the TailorGroup CR that
// folded it, keyed namespace/unit/container, keeping the other units' entries
// and dropping expired ones. A fresh engine rebuilds it from the CR.
func TestOOMFloorPersistedOnTailorGroup(t *testing.T) {
	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Truncate(time.Second)
	other := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	prev := k8s.FormatOOMFloors(map[string]k8s.OOMFloor{
		"apps/other-unit/app": {Mem: 42, At: other},
		"apps/stale-unit/app": {Mem: 7, At: old},
	})
	gvrs := map[schema.GroupVersionResource]string{
		groupRes: "TailorGroupList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrs,
		uobj("tailor.k8s-tailor.dev/v1alpha1", "TailorGroup", "", "sandboxes", map[string]any{k8s.AnnGroupOOMFloor: prev}))
	cfg := config.Config{OOMGrowthFactor: 1.5, OOMFloorTTL: 30 * 24 * time.Hour}
	pol := policy.NewStoreWithClient(dyn, cfg)
	pol.Refresh(context.Background())
	kc := k8s.NewFromClients(fake.NewSimpleClientset(), dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	key := "apps/group/sb-1/app"

	e1 := &Engine{cfg: cfg, k8s: kc, pol: pol, oomFloor: map[string]oomFloorEntry{}}
	changed := e1.learnOOM(map[string]k8s.OOMObservation{key: {MemBytes: 1000, At: time.Now()}})
	wl := k8s.Workload{Namespace: "apps", Kind: "group", Name: "sb-1", GroupSource: "sandboxes"}
	e1.persistOOM(context.Background(), []k8s.Workload{wl}, changed, func(k8s.Workload) bool { return true })

	obj, err := dyn.Resource(groupRes).Get(context.Background(), "sandboxes", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := k8s.ParseOOMFloors(obj.GetAnnotations()[k8s.AnnGroupOOMFloor])
	if got["apps/sb-1/app"].Mem != 1500 || got["apps/other-unit/app"].Mem != 42 {
		t.Fatalf("group floors = %v, want this unit's 1500 and the other unit kept", got)
	}
	if _, ok := got["apps/stale-unit/app"]; ok {
		t.Fatalf("expired entry kept: %v", got)
	}

	pol2 := policy.NewStoreWithClient(dyn, cfg)
	pol2.Refresh(context.Background())
	e2 := &Engine{cfg: cfg, k8s: kc, pol: pol2, oomFloor: map[string]oomFloorEntry{}}
	e2.seedOOM([]k8s.Workload{wl, {Namespace: "apps", Kind: "group", Name: "sb-2", GroupSource: "sandboxes"}})
	if got := e2.oomFloorFor(key); got != 1500 {
		t.Fatalf("rebuilt group floor = %v, want 1500", got)
	}
	if got := e2.oomFloorFor("apps/group/sb-2/app"); got != 0 {
		t.Fatalf("floor leaked to another unit: %v", got)
	}
}

// A bare Pod is never persisted (no controller recreates it; nothing to protect).
func TestOOMFloorNotPersistedForBarePod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	kc := k8s.NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	e := &Engine{cfg: config.Config{OOMGrowthFactor: 1.5, OOMFloorTTL: time.Hour}, k8s: kc, oomFloor: map[string]oomFloorEntry{}}
	changed := e.learnOOM(map[string]k8s.OOMObservation{"apps/Pod/solo/app": {MemBytes: 1000, At: time.Now()}})
	e.persistOOM(context.Background(), []k8s.Workload{{Namespace: "apps", Kind: "Pod", Name: "solo"}}, changed, func(k8s.Workload) bool { return true })
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			t.Fatalf("bare Pod patched: %v", a)
		}
	}
}
