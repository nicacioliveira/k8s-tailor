package webhook

import (
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// /mutate is reachable by anything that can talk to :8443. A forged
// AdmissionReview — wrong kind/resource, a subresource, a non-CREATE operation —
// must be admitted untouched (no patch) and must never write the decision log:
// only pods that really exist, observed later by the reconciler, are recorded.
func TestWebhookForgedReviewsNoPatchNoRecord(t *testing.T) {
	base := func() admissionv1.AdmissionReview {
		var ar admissionv1.AdmissionReview
		_ = json.Unmarshal(review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}), &ar)
		return ar
	}
	cases := map[string]func(r *admissionv1.AdmissionRequest){
		"kind Deployment": func(r *admissionv1.AdmissionRequest) {
			r.Kind = metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
		},
		"resource configmaps": func(r *admissionv1.AdmissionRequest) {
			r.Resource = metav1.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		},
		"subresource status":  func(r *admissionv1.AdmissionRequest) { r.SubResource = "status" },
		"subresource resize":  func(r *admissionv1.AdmissionRequest) { r.SubResource = "resize" },
		"operation UPDATE":    func(r *admissionv1.AdmissionRequest) { r.Operation = admissionv1.Update },
		"operation DELETE":    func(r *admissionv1.AdmissionRequest) { r.Operation = admissionv1.Delete },
		"operation CONNECT":   func(r *admissionv1.AdmissionRequest) { r.Operation = admissionv1.Connect },
		"no operation at all": func(r *admissionv1.AdmissionRequest) { r.Operation = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, webEntry(0.2, 128*mib))
			ar := base()
			mut(ar.Request)
			b, _ := json.Marshal(ar)
			res := call(t, f.wh, b)
			if res.status != 200 || !res.allowed {
				t.Fatalf("must admit: status=%d allowed=%v", res.status, res.allowed)
			}
			if len(res.ops)+len(res.annOps) != 0 {
				t.Fatalf("forged review got a patch: %v %v", res.ops, res.annOps)
			}
			if n := len(f.store.Recent(10)); n != 0 {
				t.Fatalf("forged review recorded %d decisions", n)
			}
		})
	}
	// The genuine CREATE is patched — but still records nothing at admission.
	t.Run("genuine CREATE", func(t *testing.T) {
		f := newFixture(t, webEntry(0.2, 128*mib))
		b, _ := json.Marshal(base())
		res := call(t, f.wh, b)
		if len(res.ops) != 1 {
			t.Fatalf("genuine CREATE not patched: %v", res.ops)
		}
		if n := len(f.store.Recent(10)); n != 0 {
			t.Fatalf("admission recorded %d decisions; recording belongs to the reconciler", n)
		}
	})
}

// The patch marks the pod with the decision (container -> from/to), so the
// reconciler can record it once the pod really exists. Reinvocation merges.
func TestWebhookAdmissionAnnotation(t *testing.T) {
	f := newFixture(t, webEntry(0.2, 128*mib))
	res := call(t, f.wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}))
	if len(res.annOps) != 1 {
		t.Fatalf("want one annotation op, got %v", res.annOps)
	}
	op := res.annOps[0]
	var raw string
	switch op["path"] {
	case "/metadata/annotations": // pod had no annotations
		raw = op["value"].(map[string]any)[k8s.AnnAdmissionSized].(string)
	default:
		t.Fatalf("unexpected annotation path %v", op["path"])
	}
	got, ok := k8s.ParseAdmissionSized(raw)
	if !ok {
		t.Fatalf("annotation %q not parseable", raw)
	}
	d := got["app"]
	if d.FromCPU != 1 || d.ToCPU != 0.2 || d.FromMem != 512*mib || d.ToMem != 128*mib {
		t.Fatalf("decision = %+v", d)
	}
}

// Reinvocation: the marker of the previous invocation is kept and extended.
func TestWebhookAdmissionAnnotationMergesOnReinvocation(t *testing.T) {
	f := newFixture(t, nil)
	seedMany(f, map[string]*recommender.Entry{
		"app":         webEntry(0.2, 128*mib),
		"istio-proxy": proxyWebEntry(0.02, 64*mib, policy.ModeWebhook),
	})
	pod := meshPod("native") // has annotations (istio status) → keyed add
	pod.Spec.Containers[0].Resources.Requests = rl("cpu", "200m", "memory", "128Mi")
	pod.Annotations[k8s.AnnAdmissionSized] = `{"app":{"fromCpu":1,"toCpu":0.2,"fromMem":536870912,"toMem":134217728}}`
	res := call(t, f.wh, podReview(t, pod))
	if len(res.annOps) != 1 || res.annOps[0]["path"] != "/metadata/annotations/tailor.k8s-tailor.dev~1admission-sized" {
		t.Fatalf("annotation ops = %v", res.annOps)
	}
	got, ok := k8s.ParseAdmissionSized(res.annOps[0]["value"].(string))
	if !ok || got["app"].ToCPU != 0.2 || got["istio-proxy"].ToCPU != 0.02 {
		t.Fatalf("merged marker = %+v", got)
	}
}
