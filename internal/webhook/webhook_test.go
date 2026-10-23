package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

const mib = 1024 * 1024

var tailorGVRs = map[schema.GroupVersionResource]string{
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
}

type fixture struct {
	wh    *Webhook
	eng   *recommender.Engine
	pol   *policy.Store
	store *actuation.Store
}

func newFixture(t *testing.T, e *recommender.Entry) *fixture {
	t.Helper()
	cs := fake.NewSimpleClientset(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc", Namespace: "apps",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}},
	}})
	cfg := config.Config{}
	kc := k8s.NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), cfg)
	eng := recommender.NewEngine(cfg, nil, kc, pol, pricing.New(cfg, nil))
	if e != nil {
		eng.Seed([]recommender.Recommendation{e.Rec}, map[string]recommender.Entry{
			recommender.Key("apps", "Deployment", "web", "app"): *e,
		})
	}
	store := actuation.NewStore(10)
	return &fixture{wh: New(cfg, eng, kc), eng: eng, pol: pol, store: store}
}

func webEntry(cpu, mem float64, mut ...func(*recommender.Entry)) *recommender.Entry {
	e := &recommender.Entry{
		Rec: recommender.Recommendation{
			Namespace: "apps", Kind: "Deployment", Workload: "web", Container: "app",
			// Confident: the webhook applies the reconciler's gate
			// (Confident || OOMRisk). Was false here, which masked that the
			// webhook ignored the gate altogether.
			Confident:   true,
			Recommended: recommender.Resources{CPURequest: cpu, MemRequest: mem},
		},
		Mode: policy.ModeWebhook, Direction: policy.DirBoth,
	}
	for _, m := range mut {
		m(e)
	}
	return e
}

type podSpec struct {
	owners  []metav1.OwnerReference
	req     corev1.ResourceList
	lim     corev1.ResourceList
	rawJSON []byte // overrides the marshalled pod when set
}

func rl(kv ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		out[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return out
}

var rsOwner = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}

func review(t *testing.T, ps podSpec) []byte {
	t.Helper()
	raw := ps.rawJSON
	if raw == nil {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-abc-x", Namespace: "apps", OwnerReferences: ps.owners},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Resources: corev1.ResourceRequirements{Requests: ps.req, Limits: ps.lim},
			}}},
		}
		raw, _ = json.Marshal(pod)
	}
	ar := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID("uid-1"), Namespace: "apps",
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	b, _ := json.Marshal(ar)
	return b
}

type patchResult struct {
	status  int
	allowed bool
	uid     string
	ops     []map[string]any // resources ops (/spec/...)
	annOps  []map[string]any // decision-marker ops (/metadata/...)
}

func call(t *testing.T, wh *Webhook, body []byte) patchResult {
	t.Helper()
	rr := httptest.NewRecorder()
	wh.Handle(rr, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body)))
	res := patchResult{status: rr.Code}
	if rr.Code != http.StatusOK {
		return res
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Response == nil {
		t.Fatalf("bad response %q: %v", rr.Body.String(), err)
	}
	res.allowed, res.uid = out.Response.Allowed, string(out.Response.UID)
	if len(out.Response.Patch) > 0 {
		var all []map[string]any
		if err := json.Unmarshal(out.Response.Patch, &all); err != nil {
			t.Fatalf("patch not JSON: %v", err)
		}
		for _, op := range all {
			if strings.HasPrefix(fmt.Sprint(op["path"]), "/metadata/") {
				res.annOps = append(res.annOps, op)
			} else {
				res.ops = append(res.ops, op)
			}
		}
	}
	return res
}

func resourcesOf(t *testing.T, ops []map[string]any) (req, lim map[string]any) {
	t.Helper()
	if len(ops) != 1 || ops[0]["path"] != "/spec/containers/0/resources" || ops[0]["op"] != "add" {
		t.Fatalf("unexpected ops %v", ops)
	}
	v := ops[0]["value"].(map[string]any)
	req, _ = v["requests"].(map[string]any)
	lim, _ = v["limits"].(map[string]any)
	return
}

func TestWebhookMutateHappyPath(t *testing.T) {
	f := newFixture(t, webEntry(0.2, 128*mib))
	res := call(t, f.wh, review(t, podSpec{
		owners: rsOwner,
		req:    rl("cpu", "1", "memory", "512Mi", "ephemeral-storage", "1Gi"),
		lim:    rl("nvidia.com/gpu", "1"),
	}))
	if res.status != 200 || !res.allowed || res.uid != "uid-1" {
		t.Fatalf("status=%d allowed=%v uid=%q", res.status, res.allowed, res.uid)
	}
	req, lim := resourcesOf(t, res.ops)
	if req["cpu"] != "200m" || req["memory"] != "128Mi" {
		t.Fatalf("requests = %v", req)
	}
	if req["ephemeral-storage"] != "1Gi" || lim["nvidia.com/gpu"] != "1" {
		t.Fatalf("other resources not preserved: req=%v lim=%v", req, lim)
	}
	if _, ok := lim["memory"]; ok {
		t.Fatalf("memory limit must not be introduced: %v", lim)
	}
	// Recorded later, by the reconciler, once the pod exists (never at admission).
	if ev := f.store.Recent(5); len(ev) != 0 {
		t.Fatalf("admission recorded a decision: %+v", ev)
	}
}

// Bug 4 (webhook side): never introduce a mem limit; never shrink an existing
// limit (same "limits are safety ceilings" rule as the reconciler); an OOM floor
// raises an existing one but never introduces one (P0-2).
func TestWebhookMemLimit(t *testing.T) {
	cases := []struct {
		name    string
		lim     corev1.ResourceList
		recLim  float64
		oom     bool
		wantMem any // nil = no memory limit in the patch
	}{
		{"no limit, rec limit -> none introduced", nil, 256 * mib, false, nil},
		{"existing limit never shrunk", rl("memory", "1Gi"), 256 * mib, false, "1024Mi"},
		{"existing limit raised", rl("memory", "1Gi"), 2048 * mib, false, "2048Mi"},
		{"OOM floor never introduces a limit", nil, 768 * mib, true, nil},
		{"OOM floor raises an existing limit", rl("memory", "512Mi"), 768 * mib, true, "768Mi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, webEntry(0.2, 128*mib, func(e *recommender.Entry) {
				e.Rec.Recommended.MemLimit = c.recLim
				e.Rec.OOMRisk = c.oom
			}))
			res := call(t, f.wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: c.lim}))
			_, lim := resourcesOf(t, res.ops)
			got, ok := lim["memory"]
			if c.wantMem == nil {
				if ok {
					t.Fatalf("memory limit %v introduced", got)
				}
				return
			}
			if got != c.wantMem {
				t.Fatalf("memory limit = %v, want %v", got, c.wantMem)
			}
		})
	}
}

func TestWebhookIgnores(t *testing.T) {
	cases := []struct {
		name  string
		entry *recommender.Entry
		pod   podSpec
		pause bool
	}{
		{"no recommendation", nil, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}, false},
		{"mode Recommend", webEntry(0.2, 128*mib, func(e *recommender.Entry) { e.Mode = policy.ModeRecommend }), podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}, false},
		{"mode InPlace only", webEntry(0.2, 128*mib, func(e *recommender.Entry) { e.Mode = policy.ModeInPlace }), podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}, false},
		{"globally paused", webEntry(0.2, 128*mib), podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}, true},
		{"owner-less pod (unresolvable)", webEntry(0.2, 128*mib), podSpec{req: rl("cpu", "1", "memory", "512Mi")}, false},
		{"unknown ReplicaSet", webEntry(0.2, 128*mib), podSpec{owners: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nope"}}, req: rl("cpu", "1", "memory", "512Mi")}, false},
		{"direction gate holds both", webEntry(2, 1024*mib, func(e *recommender.Entry) { e.Direction = policy.DirDown }), podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}, false},
		{"undecodable pod object", webEntry(0.2, 128*mib), podSpec{rawJSON: []byte(`{"spec":"not-an-object"}`)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.entry)
			if c.pause {
				if err := f.pol.SetPaused(context.Background(), true); err != nil {
					t.Fatalf("pause: %v", err)
				}
			}
			res := call(t, f.wh, review(t, c.pod))
			if res.status != 200 || !res.allowed {
				t.Fatalf("must always admit: status=%d allowed=%v", res.status, res.allowed)
			}
			if len(res.ops) != 0 {
				t.Fatalf("expected no patch, got %v", res.ops)
			}
		})
	}
}

func TestWebhookBadRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", "{"},
		{"no request", `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			if res := call(t, f.wh, []byte(c.body)); res.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.status)
			}
		})
	}
}

// Bug 7: the owner lookup (an API GET inside the admission path) ran on
// context.Background() — no deadline, not cancelled when the API server gives up
// on the webhook call. It must get a bounded context derived from the request.
func TestWebhookResolveOwnerContext(t *testing.T) {
	f := newFixture(t, webEntry(0.2, 128*mib))
	var gotDeadline bool
	var parentCancelled bool
	reqCtx, cancelReq := context.WithCancel(context.Background())
	f.wh.resolveOwner = func(ctx context.Context, ns string, l map[string]string, o []metav1.OwnerReference) (k8s.Owner, bool) {
		dl, ok := ctx.Deadline()
		gotDeadline = ok && time.Until(dl) <= resolveTimeout && time.Until(dl) > 0
		cancelReq() // the API server hangs up mid-lookup...
		parentCancelled = ctx.Err() != nil
		return k8s.Owner{}, false
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(review(t, podSpec{owners: rsOwner}))).WithContext(reqCtx)
	f.wh.Handle(rr, req)
	if !gotDeadline {
		t.Fatal("owner lookup ran without a bounded deadline")
	}
	if !parentCancelled {
		t.Fatal("owner lookup context is not derived from the request context")
	}
}

// P0-4: the webhook must apply the SAME gate as the reconciler — confidence
// (Confident || OOMRisk), HPA holds (CPU/memory), and the OOM pure-limit raise.
func TestWebhookSharesReconcilerGate(t *testing.T) {
	pod := podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}
	t.Run("not confident, no OOM -> no patch", func(t *testing.T) {
		f := newFixture(t, webEntry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.Confident = false }))
		if res := call(t, f.wh, review(t, pod)); len(res.ops) != 0 {
			t.Fatalf("patched on thin data: %v", res.ops)
		}
	})
	t.Run("HPA holds CPU -> only memory moves", func(t *testing.T) {
		f := newFixture(t, webEntry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.CPUHeldByHPA = true }))
		req, _ := resourcesOf(t, call(t, f.wh, review(t, pod)).ops)
		if req["cpu"] != "1" || req["memory"] != "128Mi" {
			t.Fatalf("requests = %v, want cpu held at 1 and memory 128Mi", req)
		}
	})
	t.Run("HPA holds memory -> only CPU moves", func(t *testing.T) {
		f := newFixture(t, webEntry(0.2, 128*mib, func(e *recommender.Entry) { e.Rec.MemHeldByHPA = true }))
		req, _ := resourcesOf(t, call(t, f.wh, review(t, pod)).ops)
		if req["cpu"] != "200m" || req["memory"] != "512Mi" {
			t.Fatalf("requests = %v, want memory held at 512Mi", req)
		}
	})
	t.Run("OOM: limit raised even when the request is unchanged", func(t *testing.T) {
		f := newFixture(t, webEntry(1, 512*mib, func(e *recommender.Entry) {
			e.Rec.Confident = false
			e.Rec.OOMRisk = true
			e.Rec.Recommended.MemLimit = 1536 * mib
		}))
		res := call(t, f.wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: rl("memory", "1Gi")}))
		req, lim := resourcesOf(t, res.ops)
		if req["memory"] != "512Mi" || lim["memory"] != "1536Mi" {
			t.Fatalf("req=%v lim=%v, want request kept and limit raised to the OOM floor", req, lim)
		}
	})
}

// P1-11: a dry-run admission must return the same patch but record nothing.
func TestWebhookDryRunHasNoSideEffects(t *testing.T) {
	f := newFixture(t, webEntry(0.2, 128*mib))
	var ar admissionv1.AdmissionReview
	_ = json.Unmarshal(review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi")}), &ar)
	dry := true
	ar.Request.DryRun = &dry
	b, _ := json.Marshal(ar)
	res := call(t, f.wh, b)
	if len(res.ops) != 1 {
		t.Fatalf("dry-run must still return the patch, got %v", res.ops)
	}
	if n := len(f.store.Recent(10)); n != 0 {
		t.Fatalf("dry-run recorded %d actuation events, want 0", n)
	}
}

// P1-11: the request body is bounded.
func TestWebhookRejectsOversizedBody(t *testing.T) {
	f := newFixture(t, nil)
	big := bytes.Repeat([]byte(" "), maxReviewBytes+1)
	if res := call(t, f.wh, big); res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d, want 413", res.status)
	}
}
