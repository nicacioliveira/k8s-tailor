package k8s

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func ksvcWithResources(res map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.knative.dev/v1",
		"kind":       "Service",
		"metadata":   map[string]any{"namespace": "ns", "name": "svc"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "user-container", "image": "x", "resources": res},
		}}}},
	}}
}

// P0-3: the ksvc patch replaced the whole resources object — dropping the
// existing limits (when none was passed), GPUs, ephemeral-storage, …
func TestPatchKnativeServicePreservesUnmanagedResources(t *testing.T) {
	obj := ksvcWithResources(map[string]any{
		"requests": map[string]any{"cpu": "1", "memory": "1Gi", "ephemeral-storage": "2Gi"},
		"limits":   map[string]any{"cpu": "2", "memory": "2Gi", "nvidia.com/gpu": "1", "ephemeral-storage": "4Gi"},
	})
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}, obj)
	c := &Client{dyn: dyn}
	// Only requests move; no limit is passed (empty) → existing limits must stay.
	if err := c.PatchKnativeService(context.Background(), "ns", "svc", "user-container", "500m", "512Mi", "", "", nil); err != nil {
		t.Fatal(err)
	}
	got, err := dyn.Resource(ksvcGVR).Namespace("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cs, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	res := cs[0].(map[string]any)["resources"].(map[string]any)
	req := res["requests"].(map[string]any)
	lim, _ := res["limits"].(map[string]any)
	want := map[string]string{"cpu": "500m", "memory": "512Mi", "ephemeral-storage": "2Gi"}
	for k, v := range want {
		if req[k] != v {
			t.Errorf("requests[%s] = %v, want %v", k, req[k], v)
		}
	}
	wantLim := map[string]string{"cpu": "2", "memory": "2Gi", "nvidia.com/gpu": "1", "ephemeral-storage": "4Gi"}
	for k, v := range wantLim {
		if lim[k] != v {
			t.Errorf("limits[%s] = %v, want %v (unmanaged/unchanged limit lost)", k, lim[k], v)
		}
	}

	// A passed limit overrides only that key.
	if err := c.PatchKnativeService(context.Background(), "ns", "svc", "user-container", "500m", "3Gi", "", "3Gi", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = dyn.Resource(ksvcGVR).Namespace("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	cs, _, _ = unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	lim = cs[0].(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)
	if lim["memory"] != "3Gi" || lim["nvidia.com/gpu"] != "1" || lim["cpu"] != "2" {
		t.Fatalf("limits after mem-limit patch = %v", lim)
	}
}

// ksvcContainers must read limits too: the limit floor / health cap / the
// "never introduce a limit" rule all depend on the current limit.
func TestKsvcContainersReadsLimits(t *testing.T) {
	obj := ksvcWithResources(map[string]any{
		"requests": map[string]any{"cpu": "1", "memory": "1Gi"},
		"limits":   map[string]any{"cpu": "2", "memory": "2Gi"},
	})
	cs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	got := ksvcContainers(cs, NewClassifier(nil))
	if len(got) != 1 {
		t.Fatalf("containers = %d", len(got))
	}
	if got[0].CPULimit != 2 || got[0].MemLimit != 2*1024*1024*1024 {
		t.Fatalf("limits not read: cpu=%v mem=%v", got[0].CPULimit, got[0].MemLimit)
	}
}

func ksvcWith(containers ...map[string]any) *unstructured.Unstructured {
	cs := make([]any, len(containers))
	for i, c := range containers {
		cs[i] = c
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.knative.dev/v1",
		"kind":       "Service",
		"metadata":   map[string]any{"namespace": "ns", "name": "svc"},
		"spec":       map[string]any{"template": map[string]any{"spec": map[string]any{"containers": cs}}},
	}}
}

func ksvcRequests(t *testing.T, c *Client) []map[string]any {
	t.Helper()
	got, err := c.dyn.Resource(ksvcGVR).Namespace("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cs, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	var out []map[string]any
	for _, ci := range cs {
		req, _, _ := unstructured.NestedMap(ci.(map[string]any), "resources", "requests")
		out = append(out, req)
	}
	return out
}

// P1-3 (2nd review): the ksvc patch matched by name only and returned nil when it
// found nothing. An unnamed container in the template is common (Knative calls it
// user-container in the revision), so the reconciler recorded 'applied' without changing anything.
func TestPatchKnativeServiceUnnamedSingleContainer(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}, ksvcWith(map[string]any{"image": "x"}))
	c := &Client{dyn: dyn}
	if err := c.PatchKnativeService(context.Background(), "ns", "svc", "user-container", "250m", "256Mi", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if req := ksvcRequests(t, c)[0]; req["cpu"] != "250m" || req["memory"] != "256Mi" {
		t.Fatalf("requests = %v, want the single (unnamed) container resized", req)
	}
}

// No target in the template: a distinguishable error (ErrNoTemplateContainer), nothing changes,
// including for queue-proxy in a single-container ksvc (never falls on the wrong container).
func TestPatchKnativeServiceNoTargetIsError(t *testing.T) {
	cases := []struct {
		name       string
		containers []map[string]any
		target     string
	}{
		{"queue-proxy with 1 container", []map[string]any{{"name": "app", "image": "x"}}, "queue-proxy"},
		{"unknown name with 2 containers", []map[string]any{{"name": "app", "image": "x"}, {"name": "side", "image": "y"}}, "gone"},
		{"user-container with 2 unnamed containers", []map[string]any{{"image": "x"}, {"image": "y"}}, "user-container"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}, ksvcWith(tc.containers...))
			c := &Client{dyn: dyn}
			err := c.PatchKnativeService(context.Background(), "ns", "svc", tc.target, "250m", "256Mi", "", "", nil)
			if !errors.Is(err, ErrNoTemplateContainer) {
				t.Fatalf("err = %v, want ErrNoTemplateContainer", err)
			}
			for i, req := range ksvcRequests(t, c) {
				if len(req) != 0 {
					t.Fatalf("container %d changed: %v", i, req)
				}
			}
		})
	}
}

// The unnamed container of a single-container ksvc shows up as user-container (the
// name Knative gives it in the revision); it used to be dropped from the listing.
func TestKsvcContainersNamesUnnamedSingle(t *testing.T) {
	got := ksvcContainers([]any{map[string]any{"image": "x"}}, NewClassifier(nil))
	if len(got) != 1 || got[0].Name != KnativeUserContainer {
		t.Fatalf("got %+v, want [user-container]", got)
	}
}
