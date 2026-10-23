package policy

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

var listKinds = map[schema.GroupVersionResource]string{
	policyGVR:  "TailorPolicyList",
	configGVR:  "TailorConfigList",
	profileGVR: "TailorProfileList",
	groupGVR:   "TailorGroupList",
}

func cr(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailor.k8s-tailor.dev/v1alpha1", "kind": kind,
		"metadata": map[string]any{"name": name}, "spec": spec,
	}}
}

func fallbackCfg() config.Config {
	return config.Config{
		CPUQuant: 0.9, MemQuant: 0.95, Margin: 0.15, MinCPUCores: 0.01, MinMemBytes: 32 << 20,
		MemLimitFactor: 1.0, HalfLife: 24 * time.Hour,
		ExcludeNamespaces: map[string]bool{"kube-system": true},
	}
}

func storeWith(t *testing.T, objs ...runtime.Object) *Store {
	t.Helper()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	s := NewStoreWithClient(dyn, fallbackCfg())
	s.Refresh(context.Background())
	return s
}

// Precedence: annotation > policy (inline > profileRef) > TailorConfig default > env config.
func TestResolvePrecedence(t *testing.T) {
	objs := []runtime.Object{
		cr("TailorConfig", "cluster", map[string]any{"defaultMode": "Webhook"}),
		cr("TailorProfile", "lean", map[string]any{
			"cpu":     map[string]any{"quantile": 0.5, "margin": 0.05, "max": "2"},
			"memory":  map[string]any{"quantile": 0.99, "limitFactor": 1.5, "min": "64Mi"},
			"scaleUp": map[string]any{"cooldown": "2m"}, "scaleDown": map[string]any{"cooldown": "6h", "stabilization": "1h"},
		}),
		cr("TailorProfile", "fat", map[string]any{"cpu": map[string]any{"margin": 0.5}}),
		cr("TailorPolicy", "prod", map[string]any{
			"priority": int64(10), "mode": "InPlace", "apply": "Rollout", "profileRef": "lean",
			"halfLife": "3d", "season": "weekly", "direction": "Down",
			"selector": map[string]any{"namespaces": []any{"prod"}},
			"cpu":      map[string]any{"margin": 0.25}, // inline beats profile margin 0.05
		}),
		cr("TailorPolicy", "low", map[string]any{
			"priority": int64(1), "mode": "Recommend",
			"selector": map[string]any{"namespaces": []any{"prod"}},
		}),
	}
	s := storeWith(t, objs...)

	cases := []struct {
		name  string
		ns    string
		ann   map[string]string
		check func(t *testing.T, r Resolved)
	}{
		{"no policy: TailorConfig defaultMode over env baseline", "dev", nil, func(t *testing.T, r Resolved) {
			if r.Mode != ModeWebhook || r.PolicyName != "" || r.CPU.Quantile != 0.9 || r.Mem.LimitFactor != 1.0 || r.HalfLife != 24*time.Hour {
				t.Fatalf("%+v", r)
			}
		}},
		{"highest-priority policy + profile + inline", "prod", nil, func(t *testing.T, r Resolved) {
			if r.Mode != ModeInPlace || r.Apply != ApplyRollout || r.Direction != DirDown || r.Season != "weekly" || r.HalfLife != 72*time.Hour {
				t.Fatalf("policy fields: %+v", r)
			}
			if r.CPU.Margin != 0.25 { // inline over profile
				t.Fatalf("cpu margin = %v, want inline 0.25", r.CPU.Margin)
			}
			if r.CPU.Quantile != 0.5 || r.CPU.Max != 2 || r.Mem.Quantile != 0.99 || r.Mem.LimitFactor != 1.5 || r.Mem.Min != 64<<20 {
				t.Fatalf("profile fields: cpu=%+v mem=%+v", r.CPU, r.Mem)
			}
			if r.ScaleUpCooldown != 2*time.Minute || r.ScaleDownCooldown != 6*time.Hour || r.ScaleDownStabilization != time.Hour {
				t.Fatalf("cooldowns: %v %v %v", r.ScaleUpCooldown, r.ScaleDownCooldown, r.ScaleDownStabilization)
			}
			if r.PolicyName != "prod (profile lean)" || r.Overridden {
				t.Fatalf("name=%q overridden=%v", r.PolicyName, r.Overridden)
			}
		}},
		{"annotation profile overrides policy scaling", "prod", map[string]string{AnnProfile: "fat"}, func(t *testing.T, r Resolved) {
			if r.CPU.Margin != 0.5 || r.Profile != "fat" || r.PolicyName != "workload:fat" || r.Mode != ModeInPlace {
				t.Fatalf("%+v", r)
			}
		}},
		{"unknown annotation profile is ignored", "prod", map[string]string{AnnProfile: "nope"}, func(t *testing.T, r Resolved) {
			if r.CPU.Margin != 0.25 || r.Profile != "" {
				t.Fatalf("%+v", r)
			}
		}},
		{"annotation mode beats policy", "prod", map[string]string{AnnMode: "Recommend"}, func(t *testing.T, r Resolved) {
			if r.Mode != ModeRecommend || !r.Overridden {
				t.Fatalf("%+v", r)
			}
		}},
		{"ignore is absolute: beats an explicit mode annotation", "prod", map[string]string{AnnIgnore: "true", AnnMode: "Both"}, func(t *testing.T, r Resolved) {
			if r.Mode != ModeOff || !r.Overridden || !r.Ignored {
				t.Fatalf("%+v", r)
			}
		}},
		{"ignore forces Off", "dev", map[string]string{AnnIgnore: "true"}, func(t *testing.T, r Resolved) {
			if r.Mode != ModeOff || r.PolicyName != "annotation:ignore" {
				t.Fatalf("%+v", r)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.check(t, s.Resolve(c.ns, nil, c.ann)) })
	}
}

func TestResolveSchedule(t *testing.T) {
	always := map[string]any{"timezone": "UTC", "windows": []any{map[string]any{"from": "2000-01-01"}}}
	never := map[string]any{"timezone": "UTC", "outsideMode": "Recommend", "windows": []any{map[string]any{"until": "2000-01-01"}}}
	neverDefault := map[string]any{"windows": []any{map[string]any{"until": "2000-01-01"}}}
	cases := []struct {
		name     string
		schedule map[string]any
		want     Mode
	}{
		{"inside window", always, ModeInPlace},
		{"outside window -> outsideMode", never, ModeRecommend},
		{"outside window -> default Off", neverDefault, ModeOff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := storeWith(t, cr("TailorPolicy", "p", map[string]any{"mode": "InPlace", "schedule": c.schedule}))
			if r := s.Resolve("any", nil, nil); r.Mode != c.want {
				t.Fatalf("mode = %v, want %v", r.Mode, c.want)
			}
		})
	}
}

func TestScheduleActiveWindows(t *testing.T) {
	mon1030 := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC) // a Monday
	cases := []struct {
		name string
		s    scheduleSpec
		want bool
	}{
		{"weekday hours inside", scheduleSpec{Windows: []window{{Days: []string{"Mon"}, Start: "09:00", End: "17:00"}}}, true},
		{"weekday hours before start", scheduleSpec{Windows: []window{{Days: []string{"Mon"}, Start: "11:00", End: "17:00"}}}, false},
		{"other day", scheduleSpec{Windows: []window{{Days: []string{"Tue"}, Start: "09:00", End: "17:00"}}}, false},
		{"timezone shifts the clock", scheduleSpec{Timezone: "America/Sao_Paulo", Windows: []window{{Start: "09:00", End: "17:00"}}}, false},
		{"campaign range inclusive of until day", scheduleSpec{Windows: []window{{From: "2026-10-19", Until: "2026-10-19"}}}, true},
		{"bad HH:MM never matches", scheduleSpec{Windows: []window{{Start: "x", End: "17:00"}}}, false},
	}
	for _, c := range cases {
		if got := scheduleActive(c.s, mon1030); got != c.want {
			t.Errorf("%s: active=%v want %v", c.name, got, c.want)
		}
	}
}

func TestStoreCRUDAndGlobal(t *testing.T) {
	ctx := context.Background()
	s := storeWith(t)

	// Defaults when the CRs are absent.
	g := s.Global()
	if len(g.ObservedKinds) != 4 || len(g.ExcludeNamespaces) != 1 || g.ExcludeNamespaces[0] != "kube-system" {
		t.Fatalf("default global = %+v", g)
	}
	if gr := s.Groups(); len(gr) != 1 || gr[0].GroupBy != KnativeServiceLabel {
		t.Fatalf("default groups = %+v", gr)
	}

	s.EnsureConfig(ctx)
	if err := s.SetPaused(ctx, true); err != nil || !s.Paused() {
		t.Fatalf("SetPaused: %v paused=%v", err, s.Paused())
	}
	if err := s.UpsertPolicy(ctx, PolicyView{Name: "p", Mode: "InPlace", Namespaces: []string{"prod"}, CPUMargin: 0.3, MemMax: "1Gi", Direction: "Down",
		Schedule: ScheduleView{Windows: []WindowView{{Days: []string{"Mon"}, Start: "09:00", End: "17:00"}}}}); err != nil {
		t.Fatalf("UpsertPolicy create: %v", err)
	}
	if err := s.UpsertPolicy(ctx, PolicyView{Name: "p", Mode: "Both", Namespaces: []string{"prod"}}); err != nil {
		t.Fatalf("UpsertPolicy update: %v", err)
	}
	if err := s.UpsertGroup(ctx, GroupView{Name: "g", GroupBy: "app", CohortBy: "rev"}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	s.Refresh(ctx)

	if !s.Paused() {
		t.Fatal("pause not persisted to the TailorConfig CR")
	}
	pv := s.ListPolicies()
	if len(pv) != 1 || pv[0].Mode != "Both" || pv[0].Scheduled {
		t.Fatalf("policies = %+v", pv)
	}
	if r := s.Resolve("prod", nil, nil); r.Mode != ModeBoth {
		t.Fatalf("resolve after upsert = %v", r.Mode)
	}
	gv := s.ListGroups(ctx)
	if len(gv) != 1 || gv[0].GroupBy != "app" || gv[0].CohortSelect != "latest" {
		t.Fatalf("groups = %+v", gv)
	}
	if gr := s.Groups(); len(gr) != 1 || gr[0].Actuation != "ksvc" || gr[0].CohortBy != "rev" {
		t.Fatalf("group rules = %+v", gr)
	}
	if err := s.DeletePolicy(ctx, "p"); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}
	if err := s.DeleteGroup(ctx, "g"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	s.Refresh(ctx)
	if len(s.ListPolicies()) != 0 || len(s.ListGroups(ctx)) != 0 {
		t.Fatal("delete did not remove the CRs")
	}
}
