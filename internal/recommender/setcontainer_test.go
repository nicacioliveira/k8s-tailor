package recommender

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func depAnnotations(t *testing.T, e *Engine) map[string]string {
	t.Helper()
	d, err := e.k8s.Clientset().AppsV1().Deployments("apps").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Annotations
}

// Round trip: SetContainer writes tailor.k8s-tailor.dev/container.<name>.mode|
// exclude on the workload, and the next refresh resolves the container's
// effective mode from it (the same path a kubectl annotate takes). This is the
// explicit opt-in that lifts the injected-sidecar cap; "inherit" / exclude=false
// remove the annotations and restore the default.
func TestSetContainerRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := meshEngine(t, meshCase{policies: []runtime.Object{inPlacePolicy}})
	modeKey := policy.AnnContainer("istio-proxy", "mode")
	exclKey := policy.AnnContainer("istio-proxy", "exclude")

	refresh := func() {
		t.Helper()
		if err := e.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	proxyMode := func() (policy.Mode, bool) {
		en, ok := e.Get("apps", "Deployment", "web", "istio-proxy")
		return en.Mode, ok
	}
	if m, _ := proxyMode(); m != policy.ModeRecommend {
		t.Fatalf("baseline proxy mode = %q, want Recommend (sidecar cap)", m)
	}

	// Opt the injected sidecar in.
	if err := e.SetContainer(ctx, "apps", "Deployment", "web", "istio-proxy", strp("InPlace"), nil); err != nil {
		t.Fatal(err)
	}
	if got := depAnnotations(t, e)[modeKey]; got != "InPlace" {
		t.Fatalf("annotation %s = %q, want InPlace", modeKey, got)
	}
	if m, _ := proxyMode(); m != policy.ModeInPlace {
		t.Fatalf("in-memory proxy mode right after the write = %q, want InPlace", m)
	}
	refresh()
	if m, ok := proxyMode(); !ok || m != policy.ModeInPlace {
		t.Fatalf("proxy mode after refresh = %q (present %v), want InPlace", m, ok)
	}
	if app, _ := e.Get("apps", "Deployment", "web", "app"); app.Mode != policy.ModeInPlace {
		t.Fatalf("app mode changed to %q", app.Mode)
	}

	// Off on the app container only.
	if err := e.SetContainer(ctx, "apps", "Deployment", "web", "app", strp("Off"), nil); err != nil {
		t.Fatal(err)
	}
	refresh()
	if app, _ := e.Get("apps", "Deployment", "web", "app"); app.Mode != policy.ModeOff || app.Rec.ContainerPolicy != "annotation" {
		t.Fatalf("app = mode %q policy %q, want Off from annotation", app.Mode, app.Rec.ContainerPolicy)
	}

	// inherit removes the annotation: back to the sidecar default.
	if err := e.SetContainer(ctx, "apps", "Deployment", "web", "istio-proxy", strp("inherit"), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := depAnnotations(t, e)[modeKey]; ok {
		t.Fatalf("inherit must remove %s", modeKey)
	}
	refresh()
	if m, _ := proxyMode(); m != policy.ModeRecommend {
		t.Fatalf("proxy mode after inherit = %q, want Recommend", m)
	}

	// exclude=true: annotation "true", the container leaves the recommendation set.
	if err := e.SetContainer(ctx, "apps", "Deployment", "web", "istio-proxy", nil, boolp(true)); err != nil {
		t.Fatal(err)
	}
	if got := depAnnotations(t, e)[exclKey]; got != "true" {
		t.Fatalf("annotation %s = %q, want true", exclKey, got)
	}
	if m, _ := proxyMode(); m != policy.ModeOff {
		t.Fatalf("in-memory mode of an excluded container = %q, want Off", m)
	}
	refresh()
	if _, ok := proxyMode(); ok {
		t.Fatal("excluded container still has a recommendation after refresh")
	}
	// The container is still known (excluded containers stay addressable), so it
	// can be re-included through the same API.
	if err := e.SetContainer(ctx, "apps", "Deployment", "web", "istio-proxy", nil, boolp(false)); err != nil {
		t.Fatal(err)
	}
	if _, ok := depAnnotations(t, e)[exclKey]; ok {
		t.Fatalf("exclude=false must remove %s", exclKey)
	}
	refresh()
	if m, ok := proxyMode(); !ok || m != policy.ModeRecommend {
		t.Fatalf("re-included proxy = %q (present %v), want Recommend", m, ok)
	}
}

func TestSetContainerValidation(t *testing.T) {
	ctx := context.Background()
	e := meshEngine(t, meshCase{policies: []runtime.Object{inPlacePolicy}})
	cases := []struct {
		name, kind, wl, ctr string
		mode                *string
		excl                *bool
		want                error
	}{
		{"unknown workload", "Deployment", "nope", "app", strp("Off"), nil, ErrWorkloadNotFound},
		{"unknown container", "Deployment", "web", "nope", strp("Off"), nil, ErrContainerNotFound},
		{"init container", "Deployment", "web", "istio-init", strp("InPlace"), nil, ErrContainerNotResizable},
		{"invalid mode", "Deployment", "web", "app", strp("inplace"), nil, policy.ErrInvalid},
		{"nothing to change", "Deployment", "web", "app", nil, nil, ErrNothingToChange},
		{"ksvc kind", "ksvc", "web", "app", strp("Off"), nil, k8s.ErrUnsupportedKind},
		{"empty container", "Deployment", "web", "", strp("Off"), nil, ErrContainerNotFound},
	}
	for _, c := range cases {
		err := e.SetContainer(ctx, "apps", c.kind, c.wl, c.ctr, c.mode, c.excl)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	for k := range depAnnotations(t, e) {
		if len(k) > len(policy.AnnContainerPrefix) && k[:len(policy.AnnContainerPrefix)] == policy.AnnContainerPrefix {
			t.Fatalf("a rejected request wrote annotation %s", k)
		}
	}
}

// The workload-level write validates the mode the same way and accepts
// "inherit" as an alias of "" (remove the annotation).
func TestSetWorkloadModeValidation(t *testing.T) {
	ctx := context.Background()
	e := meshEngine(t, meshCase{})
	e.refreshImpl = func(context.Context) error { return nil }
	if err := e.SetWorkload(ctx, "apps", "Deployment", "web", strp("Bogus"), nil); !errors.Is(err, policy.ErrInvalid) {
		t.Fatalf("invalid mode: err = %v", err)
	}
	if _, ok := depAnnotations(t, e)[policy.AnnMode]; ok {
		t.Fatal("invalid mode was written")
	}
	if err := e.SetWorkload(ctx, "apps", "Deployment", "web", strp("Webhook"), nil); err != nil {
		t.Fatal(err)
	}
	if got := depAnnotations(t, e)[policy.AnnMode]; got != "Webhook" {
		t.Fatalf("mode annotation = %q", got)
	}
	if err := e.SetWorkload(ctx, "apps", "Deployment", "web", strp("inherit"), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := depAnnotations(t, e)[policy.AnnMode]; ok {
		t.Fatal("inherit must remove the workload mode annotation")
	}
	if err := e.SetWorkload(ctx, "apps", "Deployment", "gone", strp("Off"), nil); !errors.Is(err, ErrWorkloadNotFound) {
		t.Fatalf("missing workload: err = %v, want ErrWorkloadNotFound", err)
	}
	if err := e.SetWorkload(ctx, "apps", "group", "g", strp("Off"), nil); !errors.Is(err, k8s.ErrUnsupportedKind) {
		t.Fatalf("group kind: err = %v, want ErrUnsupportedKind", err)
	}
}

// ignore=true on the workload is absolute, and a per-container mode / exclude
// beats the workload mode. The UI writes reflect in memory at once (before the
// refresh) — that reflection must resolve exactly like the refresh does, or the
// reconciler/webhook would actuate an ignored workload or a pinned container
// until the next successful refresh.
func TestSetOverridesReflectHonorsIgnoreAndContainerPins(t *testing.T) {
	ctx := context.Background()
	modeOf := func(e *Engine, c string) policy.Mode {
		t.Helper()
		en, ok := e.Get("apps", "Deployment", "web", c)
		if !ok {
			t.Fatalf("%s has no entry", c)
		}
		return en.Mode
	}
	rowMode := func(e *Engine, c string) string {
		for _, r := range e.List() {
			if r.Workload == "web" && r.Container == c {
				return r.Mode
			}
		}
		return ""
	}

	t.Run("ignored workload: workload mode write", func(t *testing.T) {
		e := meshEngine(t, meshCase{depAnns: map[string]string{policy.AnnIgnore: "true"}})
		e.refreshImpl = func(context.Context) error { return nil } // the refresh never lands
		if err := e.SetWorkload(ctx, "apps", "Deployment", "web", strp("InPlace"), nil); err != nil {
			t.Fatal(err)
		}
		if m := modeOf(e, "app"); m != policy.ModeOff {
			t.Fatalf("app mode = %q after a mode write on an ignored workload, want Off", m)
		}
		if m := rowMode(e, "app"); m != string(policy.ModeOff) {
			t.Fatalf("app row mode = %q, want Off", m)
		}
	})
	t.Run("ignored workload: container mode write", func(t *testing.T) {
		e := meshEngine(t, meshCase{depAnns: map[string]string{policy.AnnIgnore: "true"}})
		e.refreshImpl = func(context.Context) error { return nil }
		if err := e.SetContainer(ctx, "apps", "Deployment", "web", "app", strp("InPlace"), nil); err != nil {
			t.Fatal(err)
		}
		if m := modeOf(e, "app"); m != policy.ModeOff {
			t.Fatalf("app mode = %q after a container mode write on an ignored workload, want Off", m)
		}
	})
	t.Run("container pinned Off: workload mode write", func(t *testing.T) {
		e := meshEngine(t, meshCase{depAnns: map[string]string{policy.AnnContainer("app", "mode"): "Off"}})
		e.refreshImpl = func(context.Context) error { return nil }
		if err := e.SetWorkload(ctx, "apps", "Deployment", "web", strp("InPlace"), nil); err != nil {
			t.Fatal(err)
		}
		if m := modeOf(e, "app"); m != policy.ModeOff {
			t.Fatalf("pinned app mode = %q after a workload mode write, want Off", m)
		}
	})
	t.Run("container excluded then mode write", func(t *testing.T) {
		e := meshEngine(t, meshCase{policies: []runtime.Object{inPlacePolicy}})
		e.refreshImpl = func(context.Context) error { return nil }
		if err := e.SetContainer(ctx, "apps", "Deployment", "web", "app", nil, boolp(true)); err != nil {
			t.Fatal(err)
		}
		// A later mode write on the still-excluded container must not re-enable it.
		if err := e.SetContainer(ctx, "apps", "Deployment", "web", "app", strp("InPlace"), nil); err != nil {
			t.Fatal(err)
		}
		if m := modeOf(e, "app"); m != policy.ModeOff {
			t.Fatalf("excluded app mode = %q, want Off", m)
		}
	})
	t.Run("plain workload: the reflection still applies", func(t *testing.T) {
		e := meshEngine(t, meshCase{})
		e.refreshImpl = func(context.Context) error { return nil }
		if err := e.SetWorkload(ctx, "apps", "Deployment", "web", strp("InPlace"), nil); err != nil {
			t.Fatal(err)
		}
		if m := modeOf(e, "app"); m != policy.ModeInPlace {
			t.Fatalf("app mode = %q, want InPlace", m)
		}
	})
}
