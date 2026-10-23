package backtest

// Adapter that drives the REAL k8s-tailor recommendation path — the unmodified
// recommender.Engine.Refresh → fetchAndCollapse → recommend → OOM-floor
// learning — inside an in-process sandbox:
//
//   - a client-go fake cluster holding ONE Deployment (+ ReplicaSet + pods)
//     whose template carries the trace's initial resources (static, like a
//     production in-place resize that never touches the template) and whose pods
//     carry the resources currently applied (plus OOMKilled last-state when the
//     simulation produced an OOM);
//   - an httptest VictoriaMetrics stand-in that answers the engine's
//     query_range calls from the trace, never returning a sample later than the
//     current simulated tick (no look-ahead).
//
// The engine runs on a SIMULATED clock (Engine.SetClock): every refresh at tick
// i sees now = T(i), so the query window, the seasonal-forecast target, the
// OOM-floor TTL and the scale-down stabilization window are all evaluated in
// trace time — no time shifting, and the engine sees the current tick.
//
// The actuation decision is the REAL reconciler code: reconciler.Decide (gate,
// direction, drift tolerance, cooldown, limit floor, OOM limit raise) evaluated
// at simulated time, for a Burstable pod with no HPA/PDB/pending resize.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/reconciler"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// EngineConfig controls how the real engine is replayed.
type EngineConfig struct {
	// Base is the engine configuration (window, quantiles, margin, half-life,
	// MinSamples, OOM learning...). Typically config.Load() so the backtest
	// uses exactly the defaults/env the product would.
	Base config.Config
	// RefreshEverySec: how often Engine.Refresh is replayed (production:
	// REFRESH_EVERY=60s). Between refreshes the last recommendation is
	// reconciled every tick, as the 30s reconcile loop would.
	RefreshEverySec int64
	// TemplateFollowsApplied: the Deployment template also carries the applied
	// resources (as with apply: Rollout, or a GitOps flow that persists the
	// change). Default false = pure in-place: the template keeps the initial
	// values. The engine reads the template as "current", so this exercises any
	// feedback of its own output into the next recommendation.
	TemplateFollowsApplied bool
	// Rollout replays the template path (Apply=Rollout, or InPlacePreferred falling
	// back): reconciler.PlanRollout and the 10 min rollout floor decide, the
	// controller template follows, and the replay rolls the pods behind the PDB. It
	// only applies to a trace with a Disruption; on any other the strategy is skipped.
	Rollout bool
}

type engineStrategy struct {
	cfg  EngineConfig
	tr   *Trace
	ctx  context.Context
	cs   *fake.Clientset
	eng  *recommender.Engine
	srv  *httptest.Server
	kind string

	mu   sync.Mutex
	upto int   // last tick visible to the VM stand-in
	now  int64 // simulated clock (unix seconds) of the current tick

	oomAt       []int64 // per pod: sim time of last OOM (0 = never)
	lastRefresh int64
	refreshed   bool
	entry       recommender.Entry
	hasEntry    bool
	lastAct     int64
	acted       bool
	rolling     bool // a disruptive rolling is in progress (ObservePods)
	Refreshes   int
}

// NewEngine returns a factory replaying the real recommender.Engine.
func NewEngine(cfg EngineConfig) Factory {
	return func(tr *Trace) (Strategy, error) { return newEngineStrategy(cfg, tr) }
}

// NewEngineRollout returns a factory replaying the engine on the rollout path
// (reported as k8s-tailor-rollout). It applies to traces with a Disruption only.
func NewEngineRollout(cfg EngineConfig) Factory {
	cfg.Rollout, cfg.TemplateFollowsApplied = true, true
	return func(tr *Trace) (Strategy, error) {
		if tr.Disruption == nil {
			return nil, ErrNotApplicable
		}
		return newEngineStrategy(cfg, tr)
	}
}

var (
	ksvcGVR    = schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}
	tailorGVRs = map[schema.GroupVersionResource]string{
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
		{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
	}
)

func cpuQty(cores float64) resource.Quantity {
	return *resource.NewMilliQuantity(int64(math.Round(cores*1000)), resource.DecimalSI)
}
func memQty(b float64) resource.Quantity {
	return *resource.NewQuantity(int64(math.Round(b)), resource.BinarySI)
}

func resourceReqs(r Resources) corev1.ResourceRequirements {
	rr := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQty(r.CPURequest), corev1.ResourceMemory: memQty(r.MemRequest)},
	}
	lim := corev1.ResourceList{}
	if r.CPULimit > 0 {
		lim[corev1.ResourceCPU] = cpuQty(r.CPULimit)
	}
	if r.MemLimit > 0 {
		lim[corev1.ResourceMemory] = memQty(r.MemLimit)
	}
	if len(lim) > 0 {
		rr.Limits = lim
	}
	return rr
}

func newEngineStrategy(cfg EngineConfig, tr *Trace) (*engineStrategy, error) {
	if cfg.RefreshEverySec <= 0 {
		cfg.RefreshEverySec = 3600
	}
	if cfg.Base.ExcludeNamespaces[tr.Namespace] {
		return nil, fmt.Errorf("namespace %q is in EXCLUDE_NAMESPACES; the engine would skip it", tr.Namespace)
	}
	s := &engineStrategy{cfg: cfg, tr: tr, ctx: context.Background(), kind: "Deployment", oomAt: make([]int64, len(tr.Pods)), upto: -1}

	labels := map[string]string{"app": tr.Workload}
	replicas := int32(len(tr.Pods))
	rsName := tr.Workload + "-rs"
	objs := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: tr.Workload, Namespace: tr.Namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: tr.Container, Resources: resourceReqs(tr.Initial)}}},
				},
			},
		},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: rsName, Namespace: tr.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: tr.Workload}},
		}},
	}
	for _, p := range tr.Pods {
		objs = append(objs, s.pod(p.Name, rsName, tr.Initial, 0))
	}
	s.cs = fake.NewSimpleClientset(objs...)
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"}
	for g, k := range tailorGVRs {
		listKinds[g] = k
	}
	kc := k8s.NewFromClients(s.cs, dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds), cfg.Base.ExcludeNamespaces)
	// Policy store with no CRs → the env/config fallback defaults (Resolve).
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), cfg.Base)
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveVM))
	s.eng = recommender.NewEngine(cfg.Base, vm.New(s.srv.URL), kc, pol, pricing.New(cfg.Base, nil))
	s.eng.SetClock(func() time.Time {
		s.mu.Lock()
		defer s.mu.Unlock()
		return time.Unix(s.now, 0)
	})
	return s, nil
}

func (s *engineStrategy) pod(name, rsName string, r Resources, oomFinished int64) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: s.tr.Namespace, Labels: map[string]string{"app": s.tr.Workload},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rsName}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: s.tr.Container, Resources: resourceReqs(r)}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable,
			ContainerStatuses: []corev1.ContainerStatus{{Name: s.tr.Container, Ready: true}},
		},
	}
	if oomFinished != 0 {
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(time.Unix(oomFinished, 0)),
		}
	}
	return p
}

func (s *engineStrategy) Name() string {
	if s.cfg.Rollout {
		return "k8s-tailor-rollout"
	}
	return "k8s-tailor"
}

func (s *engineStrategy) Close() {
	if s.srv != nil {
		s.srv.Close()
	}
}

// serveVM answers /api/v1/query_range for the two queries fetchAndCollapse
// issues (cpu rate / memory working set), from the trace.
func (s *engineStrategy) serveVM(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/query_range" {
		http.Error(w, "unsupported", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	expr := q.Get("query")
	start, err1 := strconv.ParseInt(q.Get("start"), 10, 64)
	end, err2 := strconv.ParseInt(q.Get("end"), 10, 64)
	if err1 != nil || err2 != nil {
		http.Error(w, "bad start/end", http.StatusBadRequest)
		return
	}
	var pick func(p PodTrace) []float64
	switch {
	case strings.Contains(expr, "container_cpu_usage_seconds_total"):
		pick = func(p PodTrace) []float64 { return p.CPU }
	case strings.Contains(expr, "container_memory_working_set_bytes"):
		pick = func(p PodTrace) []float64 { return p.Mem }
	default:
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		return
	}
	s.mu.Lock()
	upto := s.upto
	s.mu.Unlock()
	tr := s.tr
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	first := true
	for _, p := range tr.Pods {
		vals := pick(p)
		var sb strings.Builder
		n := 0
		for i := 0; i <= upto && i < len(vals); i++ {
			t := tr.TimeAt(i)
			if t < start || t > end || math.IsNaN(vals[i]) {
				continue
			}
			if n > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`[`)
			sb.WriteString(strconv.FormatInt(t, 10))
			sb.WriteString(`,"`)
			sb.WriteString(strconv.FormatFloat(vals[i], 'g', -1, 64))
			sb.WriteString(`"]`)
			n++
		}
		if n == 0 {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, `{"metric":{"namespace":%q,"pod":%q,"container":%q},"values":[%s]}`, tr.Namespace, p.Name, tr.Container, sb.String())
	}
	b.WriteString(`]}}`)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(b.String()))
}

// syncPods writes the applied resources and OOM state onto the fake pods, as the
// cluster would show them at this refresh (and onto the Deployment template when
// TemplateFollowsApplied).
func (s *engineStrategy) syncPods(applied Resources) error {
	if s.cfg.TemplateFollowsApplied {
		d, err := s.cs.AppsV1().Deployments(s.tr.Namespace).Get(s.ctx, s.tr.Workload, metav1.GetOptions{})
		if err != nil {
			return err
		}
		d.Spec.Template.Spec.Containers[0].Resources = resourceReqs(applied)
		if _, err := s.cs.AppsV1().Deployments(s.tr.Namespace).Update(s.ctx, d, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	for j, p := range s.tr.Pods {
		pod := s.pod(p.Name, s.tr.Workload+"-rs", applied, s.oomAt[j])
		if _, err := s.cs.CoreV1().Pods(s.tr.Namespace).Update(s.ctx, pod, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// syncEach is syncPods with each replica's own resources (a rolling in progress).
func (s *engineStrategy) syncEach(pods []PodState) error {
	for j, p := range s.tr.Pods {
		pod := s.pod(p.Name, s.tr.Workload+"-rs", pods[j].Applied, s.oomAt[j])
		if _, err := s.cs.CoreV1().Pods(s.tr.Namespace).Update(s.ctx, pod, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *engineStrategy) refresh(tk Tick, applied Resources) error {
	if err := s.syncPods(applied); err != nil {
		return err
	}
	return s.refreshEngine()
}

func (s *engineStrategy) refreshEngine() error {
	if err := s.eng.Refresh(s.ctx); err != nil {
		return fmt.Errorf("engine refresh: %w", err)
	}
	s.Refreshes++
	en, ok := s.eng.Get(s.tr.Namespace, s.kind, s.tr.Workload, s.tr.Container)
	s.entry, s.hasEntry = en, ok
	return nil
}

func (s *engineStrategy) Observe(tk Tick, applied Resources) (Resources, error) {
	s.mu.Lock()
	s.upto, s.now = tk.I, tk.T
	s.mu.Unlock()
	for j := range s.tr.Pods {
		if tk.OOM[j] {
			s.oomAt[j] = tk.T
		}
	}
	if !tk.CanAct {
		return applied, nil
	}
	if !s.refreshed || tk.T-s.lastRefresh >= s.cfg.RefreshEverySec {
		if err := s.refresh(tk, applied); err != nil {
			return applied, err
		}
		s.refreshed, s.lastRefresh = true, tk.T
	}
	if !s.hasEntry {
		return applied, nil
	}
	if s.cfg.Rollout {
		return s.observeRollout(tk, applied), nil
	}
	// QoS is immutable: the reconciler never resizes a Guaranteed pod in place
	// (Apply=InPlace default). Faithfully, neither do we.
	if applied.CPULimit > 0 && applied.CPULimit == applied.CPURequest && applied.MemLimit > 0 && applied.MemLimit == applied.MemRequest {
		return applied, nil
	}
	next, act := reconcileDecision(s.entry, applied, tk.T, s.lastAct, s.acted)
	if act {
		s.lastAct, s.acted = tk.T, true
	}
	return next, nil
}

// observeRollout is the rollout path of the reconciler: PlanRollout against the
// template (which follows the target here), gated by the confidence check and the
// 10 min rollout floor, and the resources the template would then carry.
func (s *engineStrategy) observeRollout(tk Tick, applied Resources) Resources {
	en := s.entry
	if !en.Rec.Confident && !en.Rec.OOMRisk {
		return applied
	}
	plan, ok := reconciler.PlanRollout(en)
	if !ok {
		return applied
	}
	var last time.Time
	if s.acted {
		last = time.Unix(s.lastAct, 0)
	}
	if !reconciler.Cooled(en, plan.Up, reconciler.MinRolloutInterval, time.Unix(tk.T, 0), last) {
		return applied
	}
	s.lastAct, s.acted = tk.T, true
	next := applied
	next.CPURequest, next.MemRequest = reconciler.QuantizeCPU(plan.TargetCPU), reconciler.QuantizeMem(plan.TargetMem)
	if plan.CPULimit > 0 {
		next.CPULimit = reconciler.QuantizeCPU(plan.CPULimit)
	}
	if plan.MemLimit > 0 {
		next.MemLimit = reconciler.QuantizeMem(plan.MemLimit)
	}
	return next
}

// ObservePods is the in-place path on a trace whose resizes restart containers
// (resizePolicy RestartContainer): the reconciler's own PlanInPlace, Cooled and
// RollingGate, replica by replica, in simulated time. A restarted replica is not
// Ready for readySteps; a Ready one is restarted only while every other replica is
// Ready and the PDB budget (Ready pods above minAvailable) is not spent; a NotReady
// one goes at once. The rolling continues without waiting the cooldown, like
// reconcileNS does for a rolling in progress.
func (s *engineStrategy) ObservePods(tk Tick, pods []PodState) ([]PodAction, error) {
	s.mu.Lock()
	s.upto, s.now = tk.I, tk.T
	s.mu.Unlock()
	for j := range s.tr.Pods {
		if tk.OOM[j] {
			s.oomAt[j] = tk.T
		}
	}
	out := make([]PodAction, len(pods))
	for j := range out {
		out[j].Next = pods[j].Applied
	}
	if !tk.CanAct {
		return out, nil
	}
	if !s.refreshed || tk.T-s.lastRefresh >= s.cfg.RefreshEverySec {
		if err := s.syncEach(pods); err != nil {
			return out, err
		}
		if err := s.refreshEngine(); err != nil {
			return out, err
		}
		s.refreshed, s.lastRefresh = true, tk.T
	}
	if !s.hasEntry || (!s.entry.Rec.Confident && !s.entry.Rec.OOMRisk) {
		return out, nil
	}
	d := s.tr.Disruption
	now := time.Unix(tk.T, 0)
	var last time.Time
	if s.acted {
		last = time.Unix(s.lastAct, 0)
	}
	ready := countReady(pods)
	restarted, wanted := 0, false
	for j, pd := range pods {
		live := k8s.Container{Name: "app", CPURequest: pd.Applied.CPURequest, CPULimit: pd.Applied.CPULimit,
			MemRequest: pd.Applied.MemRequest, MemLimit: pd.Applied.MemLimit, CPURestartOnResize: true, MemRestartOnResize: true}
		plan := reconciler.PlanInPlace(s.entry, live)
		if !plan.Wants() {
			continue
		}
		wanted = true
		if !s.rolling && !reconciler.Cooled(s.entry, plan.Up(live), 0, now, last) {
			continue
		}
		if pd.Ready {
			others := true
			for k, o := range pods {
				if k != j && !o.Ready {
					others = false
				}
			}
			st := reconciler.RollingState{OthersReady: others, ReadyRestarted: restarted, PDBProtected: true, PDBAllowed: pdbAllowed(ready, d.PDBMinAvailable)}
			if reconciler.RollingGate(st) != reconciler.RollingGo {
				continue
			}
			restarted++
		}
		out[j] = PodAction{Next: resourcesAfter(s.entry, plan, live), Restart: true}
		s.lastAct, s.acted, s.rolling = tk.T, true, true
	}
	if !wanted {
		s.rolling = false
	}
	return out, nil
}

// resourcesAfter is what the API holds for a container after plan is patched
// (quantized like cpuStr/memStr).
func resourcesAfter(entry recommender.Entry, plan reconciler.Plan, live k8s.Container) Resources {
	next := Resources{CPURequest: live.CPURequest, CPULimit: live.CPULimit, MemRequest: live.MemRequest, MemLimit: live.MemLimit}
	cpuLim, memLim := plan.Limits(entry.Rec.Recommended, live)
	if cpuLim > 0 {
		next.CPULimit = reconciler.QuantizeCPU(cpuLim)
	}
	if memLim > 0 {
		next.MemLimit = reconciler.QuantizeMem(memLim)
	}
	next.CPURequest = reconciler.QuantizeCPU(plan.TargetCPU)
	next.MemRequest = reconciler.QuantizeMem(plan.TargetMem)
	return next
}

// reconcileDecision runs reconciler.Decide — the reconciler's own in-place
// decision (confidence gate, direction, drift tolerance, cooldown, limit floor,
// OOM limit raise) — at simulated time now, for a Burstable pod with no HPA /
// PDB / pending resize, and returns the resources the API would hold after the
// patch (quantized like cpuStr/memStr) and whether a resize happened.
func reconcileDecision(entry recommender.Entry, c Resources, now, lastAct int64, acted bool) (Resources, bool) {
	live := k8s.Container{Name: "app", CPURequest: c.CPURequest, CPULimit: c.CPULimit, MemRequest: c.MemRequest, MemLimit: c.MemLimit}
	var last time.Time
	if acted {
		last = time.Unix(lastAct, 0)
	}
	plan, ok := reconciler.Decide(entry, live, time.Unix(now, 0), last)
	if !ok {
		return c, false
	}
	return resourcesAfter(entry, plan, live), true
}
