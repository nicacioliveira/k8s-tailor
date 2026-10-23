package reconciler

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
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

var ksvcGVR = schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}

// P1-3 (2nd review): when the recommended container does not exist in the ksvc
// template the patch returned nil and the reconciler recorded 'applied' (Event, metric,
// actuation history) without changing anything. Now: no success is recorded.
func TestKsvcRolloutWithoutTargetIsNotApplied(t *testing.T) {
	ksvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.knative.dev/v1", "kind": "Service",
		"metadata": map[string]any{"namespace": testNS, "name": "site"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "app", "image": "x"},
			map[string]any{"name": "side", "image": "y"},
		}}}},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}, ksvc)
	cfg := config.Config{MinSamples: 1}
	kc := k8s.NewFromClients(fake.NewSimpleClientset(), dyn, nil)
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{}), cfg)
	eng := recommender.NewEngine(cfg, nil, kc, pol, pricing.New(cfg, nil))
	store := actuation.NewStore(100)
	r := New(cfg, eng, kc, store)

	e := entry(0.2, 128*mib, func(e *recommender.Entry) {
		e.Rec.Kind, e.Rec.Workload, e.Rec.Container = "ksvc", "site", "gone"
		e.Rec.Knative = k8s.KnativeRef{Service: "site"}
		e.Apply = policy.ApplyRollout
	})
	p := k8s.RunningPod{Name: "site-00001-deployment-x", Owner: k8s.Owner{Kind: "ksvc", Name: "site"}}
	c := k8s.Container{Name: "gone", CPURequest: 1, MemRequest: 512 * mib}
	cy := &cycle{rolled: map[string]bool{}, acted: map[string]bool{}, stamps: map[string]map[string]time.Time{},
		disrupted: map[string]map[string]bool{}, readyDisrupted: map[string]int{}, wantDisrupt: map[string]bool{}}
	before := metrics.Resizes.Value("ksvc", "applied")
	r.rollout(context.Background(), testNS, p, c, e, cy)

	if n := len(store.EventsFor(testNS, "ksvc", "site", "gone")); n != 0 {
		t.Fatalf("%d actuation events recorded without a target in the template", n)
	}
	if got := metrics.Resizes.Value("ksvc", "applied"); got != before {
		t.Fatalf("applied metric went up (%v → %v) without changing anything", before, got)
	}
}

// An in-place resize of a pod owned by a synthetic unit records the
// workload-level Event on a REAL object: the Knative Service
// (serving.knative.dev/v1, kind Service) for a ksvc unit; nothing for a
// TailorGroup unit (no object exists) — the pod Event is always recorded.
func TestInPlaceEventsOnSyntheticOwners(t *testing.T) {
	cases := []struct {
		name      string
		labels    map[string]string
		groupKeys []k8s.GroupKey
		owner     k8s.Owner
		want      []corev1.ObjectReference // involvedObject kind/apiVersion/name
	}{
		{"ksvc", map[string]string{"serving.knative.dev/service": "site"}, nil, k8s.Owner{Kind: "ksvc", Name: "site"},
			[]corev1.ObjectReference{{Kind: "Pod", APIVersion: "v1", Name: "site-1"}, {Kind: "Service", APIVersion: "serving.knative.dev/v1", Name: "site"}}},
		{"group", map[string]string{"pool": "py"}, []k8s.GroupKey{{Label: "pool"}}, k8s.Owner{Kind: "group", Name: "py"},
			[]corev1.ObjectReference{{Kind: "Pod", APIVersion: "v1", Name: "site-1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true, mkPod("site-1", "1", "512Mi", func(p *corev1.Pod) {
				p.Labels = tc.labels
				p.OwnerReferences = []metav1.OwnerReference{{Kind: "Custom", Name: "x"}}
			}))
			h.kc.SetGroupKeys(tc.groupKeys)
			e := entry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Kind, e.Rec.Workload = tc.owner.Kind, tc.owner.Name })
			h.eng.Seed([]recommender.Recommendation{e.Rec}, map[string]recommender.Entry{
				recommender.Key(testNS, tc.owner.Kind, tc.owner.Name, "app"): e,
			})
			h.cycle(t)
			if n := len(h.calls("resize")); n != 1 {
				t.Fatalf("resizes = %d, want 1", n)
			}
			var got []corev1.ObjectReference
			for _, a := range h.cs.Actions() {
				if ca, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "events" {
					o := ca.GetObject().(*corev1.Event).InvolvedObject
					got = append(got, corev1.ObjectReference{Kind: o.Kind, APIVersion: o.APIVersion, Name: o.Name})
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("events on %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d on %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
