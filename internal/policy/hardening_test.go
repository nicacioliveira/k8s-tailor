package policy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ignore=true is ABSOLUTE: neither a mode annotation, a policy, nor a
// per-container rule/annotation may re-enable actuation on an ignored workload.
func TestIgnoreAnnotationIsAbsolute(t *testing.T) {
	s := storeWith(t,
		cr("TailorPolicy", "all", map[string]any{
			"mode": "InPlace",
			"containers": []any{map[string]any{
				"match": map[string]any{"names": []any{"app"}}, "mode": "Both",
			}},
		}),
	)
	ann := map[string]string{AnnIgnore: "true", AnnMode: "InPlace", AnnContainer("app", "mode"): "Both"}
	r := s.Resolve("prod", nil, ann)
	if r.Mode != ModeOff {
		t.Fatalf("workload mode = %s, want Off (ignore wins over mode annotation)", r.Mode)
	}
	for _, class := range []string{ClassApp, ClassInjectedSidecar, ClassNativeSidecar} {
		if c := s.ResolveContainer(r, ann, "app", class); c.Mode != ModeOff {
			t.Fatalf("container (%s) mode = %s, want Off", class, c.Mode)
		}
	}
	// Without ignore the mode annotation still works.
	if r := s.Resolve("prod", nil, map[string]string{AnnMode: "Recommend"}); r.Mode != ModeRecommend {
		t.Fatalf("mode annotation = %s", r.Mode)
	}
}

func getSpec(t *testing.T, s *Store, name string) map[string]any {
	t.Helper()
	obj, err := s.dyn.Resource(policyGVR).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	return spec
}

func gitopsPolicy() map[string]any {
	return map[string]any{
		"priority": int64(5), "mode": "Recommend",
		"selector":          map[string]any{"namespaces": []any{"prod"}},
		"headroomGuard":     true,
		"headroomThreshold": 0.05,
		"cpu":               map[string]any{"quantile": 0.8, "margin": 0.2},
		"memory":            map[string]any{"quantile": 0.99, "limitFactor": 1.5},
		"scaleUp":           map[string]any{"cooldown": "2m"},
		"scaleDown":         map[string]any{"cooldown": "6h", "stabilization": "1h"},
		"containers": []any{map[string]any{
			"match": map[string]any{"names": []any{"istio-proxy"}}, "exclude": true,
		}},
	}
}

// Round-trip: what the UI reads (ListPolicies) and writes back unchanged must
// not lose fields the view does not carry (headroomGuard, quantile, cooldowns).
func TestUpsertPolicyRoundTripPreservesSpec(t *testing.T) {
	s := storeWith(t, cr("TailorPolicy", "p", gitopsPolicy()))
	before := getSpec(t, s, "p")
	v := s.ListPolicies()[0]
	if err := s.UpsertPolicy(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if after := getSpec(t, s, "p"); !reflect.DeepEqual(before, after) {
		t.Fatalf("round-trip changed the spec:\nbefore=%v\nafter =%v", before, after)
	}
}

// A partial write (MCP tool with only some args; JSON keys tracked in Present)
// changes only what was sent.
func TestUpsertPolicyPartialMerge(t *testing.T) {
	s := storeWith(t, cr("TailorPolicy", "p", gitopsPolicy()))
	in := PolicyView{Name: "p", Mode: "InPlace", CPUMargin: 0.3}
	in.Present = map[string]bool{"name": true, "mode": true, "cpuMargin": true}
	if err := s.UpsertPolicy(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	spec := getSpec(t, s, "p")
	want := gitopsPolicy()
	want["mode"] = "InPlace"
	want["cpu"].(map[string]any)["margin"] = 0.3
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("partial merge:\n got=%v\nwant=%v", spec, want)
	}
	// Sending a field empty clears it (explicit), still preserving the rest.
	in = PolicyView{Name: "p"}
	in.Present = map[string]bool{"name": true, "namespaces": true}
	if err := s.UpsertPolicy(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	spec = getSpec(t, s, "p")
	if sel, _, _ := unstructured.NestedStringSlice(spec, "selector", "namespaces"); len(sel) != 0 {
		t.Fatalf("namespaces not cleared: %v", sel)
	}
	if spec["headroomGuard"] != true || spec["mode"] != "InPlace" {
		t.Fatalf("clearing one field touched others: %v", spec)
	}
}

// A full-view write (UI) on a policy where the user CLEARED a view field removes
// it, but fields outside the view survive.
func TestUpsertPolicyFullViewClearsViewFieldsOnly(t *testing.T) {
	s := storeWith(t, cr("TailorPolicy", "p", gitopsPolicy()))
	v := s.ListPolicies()[0]
	v.CPUMargin = 0 // user cleared the margin in the form
	if err := s.UpsertPolicy(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	spec := getSpec(t, s, "p")
	cpu, _, _ := unstructured.NestedMap(spec, "cpu")
	if _, ok := cpu["margin"]; ok {
		t.Fatalf("margin not cleared: %v", cpu)
	}
	if cpu["quantile"] != 0.8 || spec["headroomThreshold"] != 0.05 {
		t.Fatalf("non-view fields lost: %v", spec)
	}
}

func TestValidatePolicyView(t *testing.T) {
	bad := []PolicyView{
		{Name: "Bad_Name"},
		{Name: "p", Mode: "Turbo"},
		{Name: "p", Apply: "Yolo"},
		{Name: "p", Direction: "Sideways"},
		{Name: "p", Season: "monthly"},
		{Name: "p", CPULimitFactor: 0.5},
		{Name: "p", MemLimitFactor: -1},
		{Name: "p", CPUMargin: -0.1},
		{Name: "p", MemMargin: 11},
		{Name: "p", MemMax: "lots"},
		{Name: "p", CPUMin: "2", CPUMax: "1"},
		{Name: "p", HalfLife: "soon"},
		{Name: "p", Priority: 1 << 30},
		{Name: "p", Schedule: ScheduleView{OutsideMode: "Nope", Windows: []WindowView{{Start: "09:00", End: "17:00"}}}},
		{Name: "p", Schedule: ScheduleView{Windows: []WindowView{{Start: "9am"}}}},
		{Name: "p", Schedule: ScheduleView{Timezone: "Mars/Olympus", Windows: []WindowView{{Days: []string{"Mon"}}}}},
		{Name: "p", Schedule: ScheduleView{Windows: []WindowView{{Days: []string{"Funday"}}}}},
		{Name: "p", Containers: []ContainerRule{{Mode: "InPlace"}}}, // match required
	}
	for i, v := range bad {
		err := ValidatePolicyView(v)
		if err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d %+v: err = %v, want ErrInvalid", i, v, err)
		}
		if err := (&Store{}).UpsertPolicy(context.Background(), v); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: UpsertPolicy must validate before touching the API (err=%v)", i, err)
		}
	}
	good := []PolicyView{
		{Name: "p"},
		{Name: "prod-db", Mode: "InPlace", Apply: "Rollout", Direction: "Down", Season: "both", CPULimitFactor: 0, MemLimitFactor: 1, CPUMargin: 0.15,
			CPUMin: "10m", CPUMax: "4", MemMin: "32Mi", MemMax: "16Gi", HalfLife: "3d",
			Schedule: ScheduleView{Timezone: "America/Sao_Paulo", OutsideMode: "Recommend", Windows: []WindowView{{Days: []string{"Mon"}, Start: "09:00", End: "17:30", From: "2026-11-27", Until: "2026-11-30"}}}},
	}
	for i, v := range good {
		if err := ValidatePolicyView(v); err != nil {
			t.Errorf("good case %d: %v", i, err)
		}
	}
}

func TestGlobalAlwaysExcludesSelfNamespace(t *testing.T) {
	cfg := fallbackCfg()
	cfg.SelfNamespace = "tailor-here"
	s := NewStoreWithClient(storeWith(t, cr("TailorConfig", "cluster", map[string]any{"excludeNamespaces": []any{"kube-system"}})).dyn, cfg)
	s.Refresh(context.Background())
	g := s.Global()
	found := false
	for _, ns := range g.ExcludeNamespaces {
		found = found || ns == "tailor-here"
	}
	if !found {
		t.Fatalf("self namespace missing from %v (the CR list must not drop it)", g.ExcludeNamespaces)
	}
}
