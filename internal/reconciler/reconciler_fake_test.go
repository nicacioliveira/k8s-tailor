package reconciler

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

const (
	testNS  = "apps"
	testDep = "web"
	testRS  = "web-abc"
	mib     = 1024 * 1024
)

// resizeCall is one captured pods/resize (or template) patch.
type resizeCall struct {
	Kind, Name, Sub string
	Body            map[string]any
}

type harness struct {
	r     *Reconciler
	kc    *k8s.Client
	cs    *fake.Clientset
	eng   *recommender.Engine
	store *actuation.Store

	mu      sync.Mutex
	patches []resizeCall
	failPod map[string]bool // pod name -> resize returns an error
}

func (h *harness) calls(sub string) []resizeCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []resizeCall
	for _, c := range h.patches {
		if c.Sub == sub {
			out = append(out, c)
		}
	}
	return out
}

type podOpt func(*corev1.Pod)

func withQoS(q corev1.PodQOSClass) podOpt {
	return func(p *corev1.Pod) { p.Status.QOSClass = q }
}

func withResizeStatus(s corev1.PodResizeStatus) podOpt {
	return func(p *corev1.Pod) { p.Status.Resize = s }
}

func withResizeCondition(typ, reason string) podOpt {
	return func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodConditionType(typ), Status: corev1.ConditionTrue, Reason: reason,
		})
	}
}

func withLimits(cpu, mem string) podOpt {
	return func(p *corev1.Pod) {
		l := corev1.ResourceList{}
		if cpu != "" {
			l[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			l[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		p.Spec.Containers[0].Resources.Limits = l
	}
}

func mkPod(name, cpu, mem string, opts ...podOpt) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: testRS}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func newHarness(t *testing.T, inPlace bool, pods ...*corev1.Pod) *harness {
	t.Helper()
	objs := []runtime.Object{&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: testRS, Namespace: testNS,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: testDep}},
	}}}
	// The Deployment the pods roll up to, as the live disruption check reads it
	// (spec.replicas / status.readyReplicas): by default every pod is a desired,
	// Ready replica. Tests that need another state use setDeployment.
	n := int32(len(pods))
	objs = append(objs, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testDep, Namespace: testNS},
		Spec: appsv1.DeploymentSpec{Replicas: &n}, Status: appsv1.DeploymentStatus{ReadyReplicas: n}})
	for _, p := range pods {
		objs = append(objs, p)
	}
	cs := fake.NewSimpleClientset(objs...)
	// Discovery answers either way: inPlace=false is a cluster KNOWN to lack the
	// pods/resize subresource (not a discovery failure — see P1-7).
	apis := []metav1.APIResource{{Name: "pods"}}
	if inPlace {
		apis = append(apis, metav1.APIResource{Name: "pods/resize"})
	}
	cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: apis}}
	h := &harness{cs: cs, failPod: map[string]bool{}}
	cs.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchAction)
		var body map[string]any
		_ = json.Unmarshal(pa.GetPatch(), &body)
		h.mu.Lock()
		h.patches = append(h.patches, resizeCall{Kind: pa.GetResource().Resource, Name: pa.GetName(), Sub: pa.GetSubresource(), Body: body})
		fail := h.failPod[pa.GetName()]
		h.mu.Unlock()
		if fail {
			return true, nil, errFake
		}
		// Swallow: we only assert what the reconciler ASKED for.
		return true, nil, nil
	})
	cfg := config.Config{MinSamples: 1}
	kc := k8s.NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{}), cfg)
	eng := recommender.NewEngine(cfg, nil, kc, pol, pricing.New(cfg, nil))
	h.kc, h.eng, h.store = kc, eng, actuation.NewStore(100)
	h.r = New(cfg, eng, kc, h.store)
	return h
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errFake = fakeErr("injected resize failure")

// entry builds a confident in-place entry recommending cpu/mem for web/app.
func entry(cpu, mem float64, mut ...func(*recommender.Entry)) recommender.Entry {
	e := recommender.Entry{
		Rec: recommender.Recommendation{
			Namespace: testNS, Kind: "Deployment", Workload: testDep, Container: "app",
			Confident:   true,
			Current:     recommender.Resources{CPURequest: 1, MemRequest: 512 * mib},
			Recommended: recommender.Resources{CPURequest: cpu, MemRequest: mem},
		},
		Mode:              policy.ModeInPlace,
		Apply:             policy.ApplyInPlace,
		Direction:         policy.DirBoth,
		ScaleUpCooldown:   30 * time.Second,
		ScaleDownCooldown: time.Hour,
	}
	for _, m := range mut {
		m(&e)
	}
	return e
}

func (h *harness) seed(e recommender.Entry) {
	h.eng.Seed([]recommender.Recommendation{e.Rec}, map[string]recommender.Entry{
		recommender.Key(testNS, "Deployment", testDep, "app"): e,
	})
}

func (h *harness) cycle(t *testing.T) {
	t.Helper()
	snap, err := h.kc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	h.r.reconcileNS(context.Background(), testNS, snap.Running[testNS], snap.NotRunning)
}

// Bug 1: the anti-thrash cooldown is keyed per workload/container, but the resize
// is per POD. After the first replica is resized the key is "hot", so every other
// replica is skipped for a full cooldown — N replicas converge in N×cooldown.
// All replicas of a workload must converge in the SAME cycle.
func TestReconcileAllReplicasConvergeInOneCycle(t *testing.T) {
	cases := []struct {
		name     string
		replicas int
		cpu, mem float64 // recommendation (current pods: 1 core / 512Mi)
	}{
		{"scale down 3 replicas", 3, 0.2, 128 * mib},
		{"scale up 4 replicas", 4, 2, 1024 * mib},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var pods []*corev1.Pod
			for i := 0; i < c.replicas; i++ {
				pods = append(pods, mkPod("web-"+string(rune('a'+i)), "1", "512Mi"))
			}
			h := newHarness(t, true, pods...)
			h.seed(entry(c.cpu, c.mem))
			h.cycle(t)
			if got := len(h.calls("resize")); got != c.replicas {
				t.Fatalf("resized %d pods in one cycle, want %d (all replicas)", got, c.replicas)
			}
			// The cooldown still holds ACROSS cycles: a second cycle in the same
			// instant must not re-resize (the fake doesn't apply the patch, so the
			// pods still drift — only the cooldown can stop it).
			h.cycle(t)
			if got := len(h.calls("resize")); got != c.replicas {
				t.Fatalf("second cycle re-resized: total %d, want %d (cooldown)", got, c.replicas)
			}
		})
	}
}

// Bug 5: a pod whose previous resize is still pending / deferred / infeasible
// (or in progress) must not be re-patched; and a Guaranteed-QoS pod can't be
// resized in place to unequal requests/limits (QoS is immutable), so the API
// server rejects it every cycle — retry forever. Both must be skipped.
func TestReconcileSkipsBlockedResizes(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want int // resize calls expected
	}{
		{"plain burstable pod is resized", mkPod("p", "1", "512Mi"), 1},
		{"legacy status.resize=Infeasible", mkPod("p", "1", "512Mi", withResizeStatus(corev1.PodResizeStatusInfeasible)), 0},
		{"legacy status.resize=Deferred", mkPod("p", "1", "512Mi", withResizeStatus(corev1.PodResizeStatusDeferred)), 0},
		{"legacy status.resize=InProgress", mkPod("p", "1", "512Mi", withResizeStatus(corev1.PodResizeStatusInProgress)), 0},
		{"condition PodResizePending/Infeasible", mkPod("p", "1", "512Mi", withResizeCondition("PodResizePending", "Infeasible")), 0},
		{"condition PodResizePending/Deferred", mkPod("p", "1", "512Mi", withResizeCondition("PodResizePending", "Deferred")), 0},
		{"condition PodResizeInProgress", mkPod("p", "1", "512Mi", withResizeCondition("PodResizeInProgress", "")), 0},
		{"guaranteed QoS", mkPod("p", "1", "512Mi", withLimits("1", "512Mi"), withQoS(corev1.PodQOSGuaranteed)), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, c.pod)
			h.seed(entry(0.2, 128*mib))
			h.cycle(t)
			if got := len(h.calls("resize")); got != c.want {
				t.Fatalf("resize calls = %d, want %d", got, c.want)
			}
		})
	}
}

// A failing resize (e.g. API-server rejection) must back off instead of
// hammering the API every reconcile tick.
func TestReconcileBacksOffOnResizeError(t *testing.T) {
	h := newHarness(t, true, mkPod("p", "1", "512Mi"))
	h.failPod["p"] = true
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	h.cycle(t)
	h.cycle(t)
	if got := len(h.calls("resize")); got != 1 {
		t.Fatalf("resize attempts = %d across 3 cycles, want 1 (backoff after failure)", got)
	}
	if n := len(h.store.Recent(10)); n != 0 {
		t.Fatalf("failed resize recorded %d actuation events, want 0", n)
	}
}

// Bug 4 (reconciler side): a container with NO memory limit must not get one
// imposed by an in-place resize, even when the recommendation carries a limit;
// only an OOM-learned floor may introduce one.
func TestReconcileDoesNotImposeMemLimit(t *testing.T) {
	cases := []struct {
		name      string
		pod       *corev1.Pod
		oom       bool
		wantLimit bool
	}{
		{"no limit, rec limit -> none imposed", mkPod("p", "1", "512Mi"), false, false},
		{"existing limit is kept/raised", mkPod("p", "1", "512Mi", withLimits("", "1Gi")), false, true},
		// P0-2: not even an OOM floor introduces one (a limit derived from the
		// request of an unbounded container could sit below its real usage).
		{"OOM floor never introduces a limit", mkPod("p", "1", "512Mi"), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, c.pod)
			h.seed(entry(1, 1024*mib, func(e *recommender.Entry) {
				e.Rec.Recommended.MemLimit = 1200 * mib
				e.Rec.OOMRisk = c.oom
			}))
			h.cycle(t)
			calls := h.calls("resize")
			if len(calls) != 1 {
				t.Fatalf("resize calls = %d, want 1", len(calls))
			}
			ctr := calls[0].Body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			res := ctr["resources"].(map[string]any)
			lim, _ := res["limits"].(map[string]any)
			_, has := lim["memory"]
			if has != c.wantLimit {
				t.Fatalf("memory limit patched=%v, want %v (body %v)", has, c.wantLimit, res)
			}
		})
	}
}

// Gates that must keep the reconciler hands-off.
func TestReconcileGates(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*recommender.Entry)
		want int
	}{
		{"mode Recommend never actuates", func(e *recommender.Entry) { e.Mode = policy.ModeRecommend }, 0},
		{"not confident, no OOM -> skip", func(e *recommender.Entry) { e.Rec.Confident = false }, 0},
		{"not confident but OOM -> act", func(e *recommender.Entry) { e.Rec.Confident = false; e.Rec.OOMRisk = true }, 1},
		{"HPA holds cpu and mem -> nothing", func(e *recommender.Entry) { e.Rec.CPUHeldByHPA = true; e.Rec.MemHeldByHPA = true }, 0},
		{"shrink-only with grow rec -> nothing", func(e *recommender.Entry) {
			e.Direction = policy.DirDown
			e.Rec.Recommended = recommender.Resources{CPURequest: 2, MemRequest: 1024 * mib}
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, true, mkPod("p", "1", "512Mi"))
			h.seed(entry(0.2, 128*mib, c.mut))
			h.cycle(t)
			if got := len(h.calls("resize")); got != c.want {
				t.Fatalf("resize calls = %d, want %d", got, c.want)
			}
		})
	}
}

// No pods/resize subresource → a managed workload falls back to ONE template
// patch (rollout) per workload, not one per pod.
func TestReconcileFallsBackToRolloutOnce(t *testing.T) {
	h := newHarness(t, false, mkPod("a", "1", "512Mi"), mkPod("b", "1", "512Mi"))
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	if got := len(h.calls("resize")); got != 0 {
		t.Fatalf("resize calls = %d, want 0 (no in-place support)", got)
	}
	var deps int
	for _, c := range h.calls("") {
		if c.Kind == "deployments" && c.Name == testDep {
			deps++
		}
	}
	if deps != 1 {
		t.Fatalf("deployment template patches = %d, want 1", deps)
	}
}

func TestDrift(t *testing.T) {
	cases := []struct {
		name                string
		cur, tgt, rel, abs_ float64
		want                bool
	}{
		{"below abs tolerance", 1.0, 1.01, 0.15, 0.02, false},
		{"below rel tolerance", 1.0, 1.1, 0.15, 0.02, false},
		{"large relative move", 1.0, 0.5, 0.15, 0.02, true},
		{"zero to something", 0, 1, 0.15, 0.02, true},
	}
	for _, c := range cases {
		if got := drift(c.cur, c.tgt, c.rel, c.abs_); got != c.want {
			t.Errorf("%s: drift=%v want %v", c.name, got, c.want)
		}
	}
}

// Bug 4 (rollout side): the template patch must not introduce a memory limit
// the template never had.
func TestRolloutDoesNotImposeMemLimit(t *testing.T) {
	cases := []struct {
		name         string
		currentLimit float64
		wantLimit    bool
	}{
		{"template without limit", 0, false},
		{"template with limit", 2048 * mib, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, false, mkPod("a", "1", "512Mi"))
			h.seed(entry(0.2, 128*mib, func(e *recommender.Entry) {
				e.Apply = policy.ApplyRollout
				e.Rec.Current.MemLimit = c.currentLimit
				e.Rec.Recommended.MemLimit = 128 * mib
			}))
			h.cycle(t)
			calls := h.calls("")
			if len(calls) != 1 {
				t.Fatalf("template patches = %d, want 1", len(calls))
			}
			tmpl := calls[0].Body["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			res := tmpl["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
			lim, _ := res["limits"].(map[string]any)
			if _, has := lim["memory"]; has != c.wantLimit {
				t.Fatalf("memory limit patched=%v, want %v (%v)", has, c.wantLimit, res)
			}
		})
	}
}

// P1-7: a transient discovery error used to be cached forever (sync.Once) as
// "no in-place support", turning every InPlace workload into a template rollout
// for the life of the process. Unknown support must HOLD (no rollout, no resize).
func TestReconcileHoldsWhenInPlaceSupportUnknown(t *testing.T) {
	h := newHarness(t, true, mkPod("a", "1", "512Mi"))
	h.cs.PrependReactor("get", "resource", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fakeErr("discovery: connection refused")
	})
	h.seed(entry(0.2, 128*mib))
	h.cycle(t)
	if n := len(h.calls("resize")); n != 0 {
		t.Fatalf("resized with unknown support: %d", n)
	}
	if n := len(h.calls("")); n != 0 {
		t.Fatalf("rolled the template on a transient discovery error: %d patches", n)
	}
}
