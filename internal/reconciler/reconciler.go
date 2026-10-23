// Package reconciler continuously right-sizes RUNNING pods in place (no restart)
// as their recommendation drifts — the live-actuation half of the tool.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

type Reconciler struct {
	cfg   config.Config
	eng   *recommender.Engine
	kc    *k8s.Client
	store *actuation.Store

	mu       sync.Mutex
	lastAct  map[string]time.Time // per workload/container — anti-thrash cooldown
	lastFail map[string]time.Time // per pod/container — backoff after a rejected resize
	lastSkip map[string]time.Time // per object/reason — skip-Event dedupe
	// rolling: workload/containers with a disruptive (container-restarting)
	// in-place resize in progress, rolled one pod (or PDB budget) per cycle. The
	// next pod continues without waiting the cooldown; cleared once no pod of the
	// workload wants a disruptive resize any more.
	rolling map[string]bool
	// notReadySince: first cycle a Running pod ("ns/pod") was seen NotReady. A
	// sibling NotReady for DISRUPTIVE_MAX_WAIT is "stuck" and stops holding the
	// disruptive rolling (see maxWaitProceeds). In memory: an engine restart or a
	// leader change starts the wait over.
	notReadySince map[string]time.Time

	// admSeen: pods (UID) whose admission-time decision was already handled;
	// started bounds how far back a fresh leader records (admissionLookback).
	admSeen map[string]bool
	started time.Time

	now func() time.Time // injectable clock (tests)
}

func New(cfg config.Config, eng *recommender.Engine, kc *k8s.Client, store *actuation.Store) *Reconciler {
	return &Reconciler{cfg: cfg, eng: eng, kc: kc, store: store,
		lastAct: map[string]time.Time{}, lastFail: map[string]time.Time{}, lastSkip: map[string]time.Time{},
		rolling: map[string]bool{}, notReadySince: map[string]time.Time{}, admSeen: map[string]bool{}, started: time.Now(), now: time.Now}
}

// admissionLookback: a newly started/elected reconciler still records pods the
// webhook sized up to this long before its term began (admitted during the
// failover gap — with the ordered Lease release it is ~RetryPeriod; one missed
// reconcile cycle at most otherwise). Older pods were recorded by the previous
// leader; a pod admitted within the window right before a failover may be
// recorded twice (best-effort log, see docs/KNOWN-ISSUES.md).
func (r *Reconciler) admissionLookback() time.Duration {
	if d := 2 * r.cfg.ReconcileEvery; d > 30*time.Second {
		return d
	}
	return 30 * time.Second
}

// recordAdmissions records the webhook's admission-time decisions for pods that
// now EXIST (k8s.AnnAdmissionSized), once per pod. The marker must agree with
// the requests the pod really has (rounded as the patch writes them: 1m CPU,
// 1Mi memory) — a pod created with a hand-written marker it doesn't match is
// ignored. This, not the admission request, is what feeds the decision log and
// tailor_mutations_total, so a forged AdmissionReview can't fabricate entries.
func (r *Reconciler) recordAdmissions(snap *k8s.ClusterSnapshot) {
	if r.store == nil {
		return
	}
	live := make(map[string]bool, len(snap.Admitted))
	for _, ap := range snap.Admitted {
		live[ap.UID] = true
		if r.admSeen[ap.UID] {
			continue
		}
		r.admSeen[ap.UID] = true
		if ap.Created.Before(r.started.Add(-r.admissionLookback())) {
			continue
		}
		spec := map[string]k8s.Container{}
		for _, c := range ap.Containers {
			spec[c.Name] = c
		}
		for name, d := range ap.Decisions {
			c, ok := spec[name]
			if !ok || milli(c.CPURequest) != milli(d.ToCPU) || mebi(c.MemRequest) != mebi(d.ToMem) {
				log.Printf("webhook decision marker on %s/%s container %s does not match the pod — not recorded", ap.Namespace, ap.Name, name)
				continue
			}
			r.store.Record(actuation.Event{
				Time: ap.Created.Unix(), Mode: "webhook", Namespace: ap.Namespace, Kind: ap.Owner.Kind,
				Workload: ap.Owner.Name, Container: name,
				FromCPU: d.FromCPU, ToCPU: d.ToCPU, FromMem: d.FromMem, ToMem: d.ToMem,
			})
		}
	}
	for uid := range r.admSeen { // forget pods that are gone (bounded memory)
		if !live[uid] {
			delete(r.admSeen, uid)
		}
	}
}

func milli(cores float64) int64 { return int64(cores*1000 + 0.5) }
func mebi(bytes float64) int64  { return int64(bytes/(1024*1024) + 0.5) }

// skipEventEvery bounds how often the same "held back" Event is emitted for one
// object+reason: the condition is re-evaluated every reconcile tick (30s) and an
// Event per tick would flood `kubectl get events`.
const skipEventEvery = time.Hour

// skip accounts for an evaluation that WANTED to act but was held back: always
// bumps the per-reason counter; emits a (deduplicated) Event when ref is set.
func (r *Reconciler) skip(ctx context.Context, reason string, ref *k8s.EventRef, evType, evReason, msg string) {
	metrics.Skips.Inc(reason)
	if ref == nil {
		return
	}
	k := ref.Namespace + "/" + ref.Kind + "/" + ref.Name + "/" + evReason + "/" + msg
	r.mu.Lock()
	last, ok := r.lastSkip[k]
	emit := !ok || r.now().Sub(last) >= skipEventEvery
	if emit {
		r.lastSkip[k] = r.now()
	}
	r.mu.Unlock()
	if emit {
		r.kc.RecordEventFor(ctx, *ref, evType, evReason, msg)
	}
}

// cooled reports whether enough time has passed since the last actuation of key
// to act again. Scale-up uses the (short) up-cooldown so we react fast to load;
// scale-down uses the (long) down-cooldown so we don't flap capacity away.
//
// persisted is the last actuation read back from the AnnLastApplied annotation
// (zero if none): after a restart or leader failover the in-memory map is empty,
// and the annotation is what keeps the new process from re-actuating at once.
func (r *Reconciler) cooled(key string, up bool, entry recommender.Entry, floor time.Duration, persisted time.Time) bool {
	return Cooled(entry, up, floor, r.now(), r.lastActed(key, persisted))
}

// lastActed is the latest known actuation of key: in-memory or persisted.
func (r *Reconciler) lastActed(key string, persisted time.Time) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.lastAct[key]
	if persisted.After(last) {
		last = persisted
	}
	return last
}

// lastAppliedOf is the most recent persisted actuation of container across the
// running pods of one workload (replicas may disagree mid-rollout).
func lastAppliedOf(pods []k8s.RunningPod, kind, name, container string) time.Time {
	var t time.Time
	for _, p := range pods {
		if p.Owner.Kind != kind || p.Owner.Name != name {
			continue
		}
		if v := p.LastApplied[container]; v.After(t) {
			t = v
		}
	}
	return t
}

// failBackoff is how long a pod/container whose resize the API server rejected
// is left alone before we try again — a deterministic rejection (validation,
// quota, QoS) would otherwise be retried every reconcile tick forever.
const failBackoff = 5 * time.Minute

// failedRecently reports whether pod/container key was rejected within the
// backoff: in memory, or persisted on the pod (AnnResizeFailed; zero = none) so
// a restarted or newly elected engine does not retry it once more at once.
func (r *Reconciler) failedRecently(key string, persisted time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.lastFail[key]
	if persisted.After(t) {
		t = persisted
	}
	return !t.IsZero() && r.now().Sub(t) < failBackoff
}

// markFailed starts the failure backoff of a pod/container, in memory and on
// the pod (AnnResizeFailed, merged with the other containers' entries).
// Best-effort: a failed metadata patch only means a restart would retry once.
func (r *Reconciler) markFailed(ctx context.Context, ns string, p k8s.RunningPod, container, key string, cause error) {
	now := r.now()
	r.mu.Lock()
	r.lastFail[key] = now
	r.mu.Unlock()
	m := make(map[string]k8s.ResizeFailure, len(p.ResizeFailed)+1)
	for k, v := range p.ResizeFailed {
		m[k] = v
	}
	m[container] = k8s.ResizeFailure{At: now, Reason: cause.Error()}
	v := k8s.FormatResizeFailed(m)
	if err := r.kc.AnnotateWorkload(ctx, "Pod", ns, p.Name, map[string]*string{k8s.AnnResizeFailed: &v}); err != nil {
		log.Printf("persist %s on pod %s/%s: %v", k8s.AnnResizeFailed, ns, p.Name, err)
	}
}

func (r *Reconciler) markActed(key string) {
	r.mu.Lock()
	r.lastAct[key] = r.now()
	r.mu.Unlock()
}

func cpuStr(cores float64) string { return fmt.Sprintf("%dm", int64(cores*1000+0.5)) }
func memStr(bytes float64) string { return fmt.Sprintf("%dMi", int64(bytes/(1024*1024)+0.5)) }

func (r *Reconciler) Run(ctx context.Context) {
	log.Printf("in-place reconciler running, every %s", r.cfg.ReconcileEvery)
	// A leadership term starts here: admission markers older than the lookback
	// were the previous term's (or leader's) to record.
	r.started = r.now()
	r.admSeen = map[string]bool{}
	t := time.NewTicker(r.cfg.ReconcileEvery)
	defer t.Stop()
	for {
		if !r.eng.Paused() { // global kill switch (TailorConfig.spec.paused)
			// One cluster-wide read per cycle (scale: O(1) not O(namespaces)).
			snap, err := r.kc.Snapshot(ctx)
			if err != nil {
				log.Printf("reconcile snapshot: %v", err)
			} else {
				observeResizeStates(snap)
				r.recordAdmissions(snap)
				for _, ns := range r.namespaces() {
					r.reconcileNS(ctx, ns, snap.Running[ns], snap.NotRunning)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// observeResizeStates publishes how many running pods have an outstanding
// in-place resize, by state (Pending/Infeasible is the actionable one: the node
// can never fit the request).
func observeResizeStates(snap *k8s.ClusterSnapshot) {
	counts := map[string]float64{}
	for _, pods := range snap.Running {
		for _, p := range pods {
			if p.ResizeState != "" {
				counts[p.ResizeState]++
			}
		}
	}
	metrics.PodsResizeState.Reset()
	for st, n := range counts {
		metrics.PodsResizeState.Set(n, st)
	}
}

// namespaces = every namespace that currently has recommendations. Whether a
// workload is actually actuated is decided per-workload by its resolved policy
// mode (TailorPolicy CRs, managed from the UI) — there is no separate
// namespace allowlist: activation lives entirely in the CRs.
func (r *Reconciler) namespaces() []string {
	return r.eng.Namespaces()
}

// limitFloor is the limit the reconciler patches: the recommended limit, but
// NEVER below the current one. A limit is a safety ceiling — we only ever raise
// it (when a growing request would exceed it), never shrink it. Shrinking a mem
// limit risks OOM and can force a restart, and a shrinking request never needs a
// lower limit. current<=0 means "unset" → take the recommendation as-is.
func limitFloor(rec, current float64) float64 {
	if current > 0 && rec < current {
		return current
	}
	return rec
}

// cycle carries per-namespace, per-tick state.
type cycle struct {
	pods []k8s.RunningPod
	// notRunning: per workload (k8s.WorkloadKey), live pods that are not Running
	// (Pending/Unknown) — invisible in pods but not serving.
	notRunning map[string]int
	rolled     map[string]bool // dedupe: one rollout patch per workload/container
	// acted: workload/containers resized THIS cycle. The cooldown is per workload
	// but the resize is per pod: once the first replica is resized every sibling
	// must follow in the same cycle, or N replicas would converge in N×cooldown.
	acted map[string]bool
	// stamps accumulates the AnnLastApplied value written to a workload's pod
	// TEMPLATE this cycle (ns/kind/name -> container -> time), so a second
	// container rolled in the same cycle doesn't overwrite the first's stamp.
	stamps map[string]map[string]time.Time
	// disrupted: pods restarted by a disruptive resize THIS cycle, per workload
	// (ns/kind/name -> pod set). Only the Ready ones (readyDisrupted) count
	// against the disruption budget: restarting a NotReady pod costs nothing.
	disrupted      map[string]map[string]bool
	readyDisrupted map[string]int
	// wantDisrupt: workload/containers that still want a disruptive resize.
	wantDisrupt map[string]bool
	// live: the workload's disruption state read from the API server right
	// before its first Ready pod is restarted this cycle (k8s.LiveDisruption),
	// per workload (WorkloadKey). The pods restarted later in the same cycle are
	// accounted by readyDisrupted on top of it.
	live map[string]*k8s.LiveDisruption
}

// liveDisruption reads (once per workload per cycle) the live PDB budget and
// the controller's desired/ready counts, right before the first Ready pod of
// the workload is restarted by a disruptive resize.
func (r *Reconciler) liveDisruption(ctx context.Context, cy *cycle, ns string, p k8s.RunningPod) (*k8s.LiveDisruption, error) {
	wkey := k8s.WorkloadKey(ns, p.Owner)
	if l, ok := cy.live[wkey]; ok {
		return l, nil
	}
	l, err := r.kc.LiveDisruption(ctx, ns, p.Owner, p.Labels)
	if err != nil {
		return nil, err
	}
	cy.live[wkey] = &l
	return &l, nil
}

// othersReady reports whether every replica of the workload except pod is
// Ready — the condition for restarting a Ready one. Besides the Running pods it
// accounts for replicas that are not Running (Pending/Unknown: notRunning) and
// for replicas the controller hasn't even created yet: fewer Ready pods than
// desired (spec.replicas / desiredNumberScheduled; 0 = unknown) means the
// workload is already short and must not lose another.
func othersReady(pods []k8s.RunningPod, notRunning int, desired int, owner k8s.Owner, pod string) bool {
	if notRunning > 0 {
		return false
	}
	ready := 0
	for _, p := range pods {
		if p.Owner != owner {
			continue
		}
		if p.Ready {
			ready++
		} else if p.Name != pod {
			return false
		}
	}
	return desired <= 0 || ready >= desired
}

// trackNotReady records since when each Running pod of ns has been NotReady, and
// forgets the pods that are Ready again or gone.
func (r *Reconciler) trackNotReady(ns string, pods []k8s.RunningPod) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, p := range pods {
		if p.Ready {
			continue
		}
		k := ns + "/" + p.Name
		seen[k] = true
		if _, ok := r.notReadySince[k]; !ok {
			r.notReadySince[k] = now
		}
	}
	for k := range r.notReadySince {
		if strings.HasPrefix(k, ns+"/") && !seen[k] {
			delete(r.notReadySince, k)
		}
	}
}

type waitVerdict int

const (
	waitHolds       waitVerdict = iota // the wait has not expired (or maxWait is 0): keep waiting
	waitProceeds                       // expired, and the live state allows a restart
	waitExpiredHeld                    // expired, but a restart would leave too few Ready replicas
)

// maxWaitProceeds decides whether a Ready pod may be restarted although a
// sibling is not Ready. It only says yes when EVERY non-Ready replica has been
// NotReady for at least DISRUPTIVE_MAX_WAIT (a replica this rolling just
// restarted is back well before that, so the one-at-a-time pacing stays), no
// replica is Pending, the controller's own ready count is short by exactly those
// stuck replicas (nothing unexplained is missing), and at least two replicas are
// Ready (the restart leaves one). The PDB budget is the caller's separate,
// earlier check on the same live read: a PDB allowing no disruption never gets
// here.
func (r *Reconciler) maxWaitProceeds(cy *cycle, ns string, p k8s.RunningPod, live *k8s.LiveDisruption) waitVerdict {
	maxWait := r.cfg.DisruptiveMaxWait
	if maxWait <= 0 {
		return waitHolds
	}
	wkey := k8s.WorkloadKey(ns, p.Owner)
	if cy.notRunning[wkey] > 0 {
		return waitHolds
	}
	now := r.now()
	stuck, ready := 0, 0
	r.mu.Lock()
	for _, s := range cy.pods {
		if s.Owner != p.Owner {
			continue
		}
		if s.Ready {
			ready++
			continue
		}
		if s.Name == p.Name {
			continue
		}
		since, ok := r.notReadySince[ns+"/"+s.Name]
		if !ok || now.Sub(since) < maxWait {
			r.mu.Unlock()
			return waitHolds
		}
		stuck++
	}
	r.mu.Unlock()
	if stuck == 0 {
		return waitHolds // blocked by something other than a stuck sibling
	}
	if live.Controller && live.Ready < live.Desired-stuck {
		return waitHolds // replicas missing that no stuck pod explains
	}
	if ready < 2 {
		return waitExpiredHeld
	}
	return waitProceeds
}

// desiredReplicas is the controller's desired replica count when it is a real
// one (Deployment/StatefulSet/DaemonSet); 0 (unknown) for groups, ksvc, Pods.
func desiredReplicas(live *k8s.LiveDisruption) int {
	if live.Controller {
		return live.Desired
	}
	return 0
}

// isRolling reports whether a disruptive rolling of workload/container key is
// in progress: known in memory, or rebuilt from the pods after a restart /
// failover (resumeRolling).
func (r *Reconciler) isRolling(key string, entry recommender.Entry, pods []k8s.RunningPod, owner k8s.Owner, container string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rolling[key] {
		return true
	}
	if resumeRolling(entry, pods, owner, container) {
		r.rolling[key] = true
		return true
	}
	return false
}

// rollingMarkSlack: AnnRolling and AnnLastApplied are written in one patch with
// the same time; both are second-resolution RFC3339.
const rollingMarkSlack = 2 * time.Second

// resumeRolling rebuilds the "rolling in progress" state from the pods: a pod
// of the workload whose LAST actuation of container was a rolling step
// (AnnRolling == AnnLastApplied) and which no longer wants a disruptive resize
// (it is already at the target) means the rolling started and is not finished —
// the next pod continues without the cooldown, still one Ready pod (or PDB
// budget) at a time behind the readiness gate. A marker left by a FINISHED
// rolling does not resume anything for a new recommendation: then every pod
// wants the change, the marked ones included. And it only matters inside the
// cooldown of that last step (past it, the cooldown no longer holds anyway).
func resumeRolling(entry recommender.Entry, pods []k8s.RunningPod, owner k8s.Owner, container string) bool {
	for _, p := range pods {
		if p.Owner != owner {
			continue
		}
		mark, ok := p.Rolling[container]
		if !ok {
			continue
		}
		if la := p.LastApplied[container]; la.IsZero() || la.Sub(mark) > rollingMarkSlack || mark.Sub(la) > rollingMarkSlack {
			continue
		}
		for _, c := range p.Containers {
			if c.Name != container {
				continue
			}
			if plan := PlanInPlace(entry, c); !plan.Wants() || !plan.Disruptive(c) {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) reconcileNS(ctx context.Context, ns string, pods []k8s.RunningPod, notRunning map[string]int) {
	cy := &cycle{pods: pods, notRunning: notRunning, rolled: map[string]bool{}, acted: map[string]bool{}, stamps: map[string]map[string]time.Time{},
		disrupted: map[string]map[string]bool{}, readyDisrupted: map[string]int{}, wantDisrupt: map[string]bool{},
		live: map[string]*k8s.LiveDisruption{}}
	defer r.endRolling(ns, cy)
	r.trackNotReady(ns, pods)
	for _, p := range pods {
		podRef := &k8s.EventRef{Kind: "Pod", Namespace: ns, Name: p.Name, UID: p.UID}
		for _, c := range p.Containers {
			// Regular init and ephemeral containers are never resized (Kubernetes
			// rejects it; an init container only runs at start-up). They stay in
			// p.Containers only for the pod-wide QoS check below.
			if !c.Class.Resizable() {
				continue
			}
			entry, ok := r.eng.Get(ns, p.Owner.Kind, p.Owner.Name, c.Name)
			if !ok || !entry.Mode.InPlace() { // per-container effective mode gates actuation
				continue
			}
			// Confidence gate: don't act on thin data — except an OOMKill, which is a
			// health emergency we grow into regardless.
			if !entry.Rec.Confident && !entry.Rec.OOMRisk {
				continue
			}
			rec := entry.Rec.Recommended

			// Apply=Rollout: patch the controller template once and let its native
			// updateStrategy roll (PDB / ordering / maxUnavailable respected by k8s).
			// Bare Pods have no controller, so they fall through to in-place.
			// If the cluster can't resize in place (no pods/resize subresource), a
			// managed workload also falls back to a rollout instead of failing.
			wantRollout := policy.Apply(entry.Apply) == policy.ApplyRollout
			if !wantRollout {
				supported, known := r.kc.InPlaceSupport(ctx)
				if !known {
					// Discovery failed (transient): never turn an InPlace workload into
					// a disruptive rollout on a guess — hold until it answers.
					r.skip(ctx, "inplace_support_unknown", &k8s.EventRef{Kind: "Pod", Namespace: ns, Name: p.Name, UID: p.UID},
						corev1.EventTypeWarning, "ResizeSkipped",
						"k8s-tailor: could not determine in-place resize support (API discovery failed) — holding; will retry")
					continue
				}
				if !supported && p.Owner.Kind != "Pod" {
					wantRollout = true // the cluster definitely lacks pods/resize
				}
			}
			if wantRollout && p.Owner.Kind != "Pod" {
				r.rollout(ctx, ns, p, c, entry, cy)
				continue
			}

			// HPA hold, direction gate, OOM override, drift tolerance and OOM limit
			// raise: the pure part of the decision (decide.go — the offline backtest
			// replays this exact code).
			plan := PlanInPlace(entry, c)
			cpuHeld := plan.CPUHeld

			// A previous resize that hasn't landed (pending/deferred/infeasible/in
			// progress): stacking another patch on top can't help — the kubelet is
			// already working on (or has refused) the spec we last asked for.
			if p.ResizeState != "" {
				if plan.Wants() {
					var ref *k8s.EventRef
					if strings.Contains(p.ResizeState, "Infeasible") {
						ref = podRef // only Infeasible is actionable by a human
					}
					r.skip(ctx, "resize_pending", ref, corev1.EventTypeWarning, "ResizeInfeasible",
						fmt.Sprintf("k8s-tailor: previous in-place resize of %s is %s — not stacking another; the node cannot fit the request", c.Name, p.ResizeState))
				}
				log.Printf("skip in-place resize %s/%s[%s]: previous resize %s", ns, p.Name, c.Name, p.ResizeState)
				continue
			}
			// QoS class is immutable: an in-place resize of a Guaranteed pod to
			// requests != limits is always rejected. Roll the template instead when the
			// policy allows it; otherwise leave the pod alone.
			// guaranteed: resizeBoth moves request and limit together instead, and
			// carries on with the ordinary guards below.
			if p.QOSClass == corev1.PodQOSGuaranteed && !plan.Guaranteed {
				if policy.Apply(entry.Apply) == policy.ApplyInPlacePreferred && p.Owner.Kind != "Pod" {
					r.rollout(ctx, ns, p, c, entry, cy)
				} else if plan.Wants() {
					r.skip(ctx, "guaranteed_qos", podRef, corev1.EventTypeNormal, "ResizeSkipped",
						fmt.Sprintf("k8s-tailor: %s is QoS Guaranteed — an in-place resize would change its QoS class; set apply: InPlacePreferred to roll the template instead", c.Name))
				}
				continue
			}
			if plan.GuaranteedMemShrinkHeld {
				r.skip(ctx, "guaranteed_mem_shrink", podRef, corev1.EventTypeNormal, "ResizeSkipped",
					fmt.Sprintf("k8s-tailor: %s is QoS Guaranteed — its memory is not shrunk in place (a lowered memory limit is best effort in the kubelet); CPU and memory growth still apply", c.Name))
			}
			if !plan.Wants() {
				continue
			}
			// BestEffort is immutable too: an in-place resize may not add requests
			// or limits to a pod that has none.
			if p.QOSClass == corev1.PodQOSBestEffort {
				r.skip(ctx, "besteffort_qos", podRef, corev1.EventTypeNormal, "ResizeSkipped",
					fmt.Sprintf("k8s-tailor: pod is QoS BestEffort — an in-place resize of %s would change its QoS class; set requests in the template (or apply: Rollout)", c.Name))
				continue
			}
			fkey := ns + "/" + p.Name + "/" + c.Name
			if r.failedRecently(fkey, p.ResizeFailed[c.Name].At) {
				r.skip(ctx, "failure_backoff", nil, "", "", "")
				continue
			}
			// Disruption guardrail: an in-place resize is non-disruptive UNLESS the
			// container opts into resizePolicy RestartContainer for the resource that
			// moves (memory or cpu) — then the kubelet restarts it. If a
			// PodDisruptionBudget covers the workload and allows no disruption we must
			// not violate it: under InPlacePreferred, fall back to a controller rollout
			// (sequenced by the k8s updateStrategy); otherwise hold the disruptive part
			// and apply only what doesn't restart the container.
			// A NotReady target is exempt: the PDB counts healthy pods, and restarting
			// one that already serves nothing (e.g. crash-looping on OOMKill) removes
			// none — otherwise an all-replicas-OOM workload could never grow.
			// The budget and the replica counts are read LIVE (not from the last
			// refresh, up to REFRESH_EVERY old) right before a Ready pod would be
			// restarted; a failed read holds instead of guessing.
			var live *k8s.LiveDisruption
			if plan.Disruptive(c) && p.Ready {
				l, err := r.liveDisruption(ctx, cy, ns, p)
				if err != nil {
					r.skip(ctx, "disruptive_live_unknown", nil, "", "", "")
					log.Printf("hold disruptive resize %s/%s[%s]: live disruption state unknown: %v", ns, p.Name, c.Name, err)
					continue
				}
				live = l
			}
			if live != nil && live.PDBProtected && live.PDBAllowed <= 0 {
				if policy.Apply(entry.Apply) == policy.ApplyInPlacePreferred && p.Owner.Kind != "Pod" {
					log.Printf("PDB %q blocks disruptive in-place resize of %s/%s[%s] — falling back to rollout",
						live.PDBName, ns, p.Name, c.Name)
					r.rollout(ctx, ns, p, c, entry, cy)
					continue
				}
				log.Printf("PDB %q blocks disruptive resize of %s/%s[%s] — applying the non-disruptive part only",
					live.PDBName, ns, p.Name, c.Name)
				r.skip(ctx, "pdb_blocked", podRef, corev1.EventTypeNormal, "ResizeSkipped",
					fmt.Sprintf("k8s-tailor: PodDisruptionBudget %q allows no disruption — the resize of %s that restarts the container (resizePolicy RestartContainer) is held", live.PDBName, c.Name))
				plan.holdDisruptive(c)
				if !plan.Wants() {
					continue
				}
			}
			disruptive := plan.Disruptive(c)
			targetCPU, targetMem := plan.TargetCPU, plan.TargetMem
			// Anti-thrash cooldown: react fast to increases, decay slowly on
			// decreases (so we never flap a workload's capacity every cycle).
			ckey := ns + "/" + p.Owner.Kind + "/" + p.Owner.Name + "/" + c.Name
			// A disruptive rolling in progress continues pod by pod without waiting the
			// cooldown (the Ready gate below paces it instead).
			if disruptive {
				cy.wantDisrupt[ckey] = true
			}
			if !cy.acted[ckey] && !(disruptive && r.isRolling(ckey, entry, cy.pods, p.Owner, c.Name)) && !r.cooled(ckey, plan.Up(c), entry, 0, lastAppliedOf(cy.pods, p.Owner.Kind, p.Owner.Name, c.Name)) {
				r.skip(ctx, "cooldown", nil, "", "", "")
				continue
			}
			// Limits ride along with any request that drifted; never shrunk in place,
			// never introduced (mem) — see Plan.Limits.
			cpuLim, memLim := "", ""
			l, m := plan.Limits(rec, c)
			if l > 0 {
				cpuLim = cpuStr(l)
			}
			if m > 0 {
				memLim = memStr(m)
			}
			// Pod-wide guards: the resize of ONE container must not flip a
			// Burstable pod to Guaranteed (QoS is immutable), nor break pod-level
			// resources (sum of container requests <= pod requests, every
			// container limit <= pod limit). Both would be rejected every tick.
			if p.QOSClass != corev1.PodQOSGuaranteed && becomesGuaranteed(p, c.Name, targetCPU, targetMem, l, m) {
				r.skip(ctx, "qos_change", podRef, corev1.EventTypeNormal, "ResizeSkipped",
					fmt.Sprintf("k8s-tailor: resizing %s would make every container requests==limits (Burstable→Guaranteed); QoS is immutable", c.Name))
				continue
			}
			if !podLevelFits(p, c.Name, targetCPU, targetMem, l, m) {
				r.skip(ctx, "pod_level_resources", podRef, corev1.EventTypeNormal, "ResizeSkipped",
					fmt.Sprintf("k8s-tailor: resizing %s would exceed the pod-level resources (spec.resources)", c.Name))
				continue
			}
			// Disruptive rolling: restart at most the budget's worth of Ready pods per
			// cycle, and only while every other replica is Ready (the previous restart
			// landed, nothing Pending, none missing). A NotReady target bypasses both:
			// restarting a pod that serves nothing costs no availability, and gating it
			// on its (equally NotReady) siblings deadlocks an all-replicas OOM loop.
			wkey := k8s.WorkloadKey(ns, p.Owner)
			if disruptive && !cy.disrupted[wkey][p.Name] && p.Ready {
				// live is set: disruptive && Ready (holdDisruptive only clears). The
				// controller's own ready count must cover the desired count too — it
				// sees a scale-up (or a replica gone NotReady) the snapshot missed.
				// Pods restarted earlier THIS cycle are already in readyDisrupted.
				st := RollingState{
					OthersReady:     othersReady(cy.pods, cy.notRunning[wkey], desiredReplicas(live), p.Owner, p.Name),
					ControllerShort: live.Controller && live.Ready < live.Desired,
					ReadyRestarted:  cy.readyDisrupted[wkey],
					PDBProtected:    live.PDBProtected, PDBAllowed: live.PDBAllowed,
				}
				verdict := RollingGate(st)
				bypass := false
				if verdict == RollingWait {
					switch r.maxWaitProceeds(cy, ns, p, live) {
					case waitHolds:
						r.skip(ctx, "disruptive_wait_ready", nil, "", "", "")
						log.Printf("hold disruptive resize %s/%s[%s]: another replica is not Ready (or missing) yet", ns, p.Name, c.Name)
						continue
					case waitExpiredHeld:
						metrics.DisruptiveWaitExpired.Inc("held")
						r.skip(ctx, "disruptive_wait_expired_held", &k8s.EventRef{Kind: p.Owner.Kind, Namespace: ns, Name: p.Owner.Name}, corev1.EventTypeWarning, "RollingHeld",
							fmt.Sprintf("k8s-tailor: a replica of %s has been NotReady for over %s and still blocks the disruptive resize of %s: restarting a Ready replica now would leave fewer than 2 Ready, so it stays held", p.Owner.Name, r.cfg.DisruptiveMaxWait, c.Name))
						continue
					case waitProceeds:
						bypass = true
						st.OthersReady, st.ControllerShort = true, false
						verdict = RollingGate(st)
					}
				}
				if verdict == RollingOverBudget {
					r.skip(ctx, "disruptive_budget", nil, "", "", "")
					continue
				}
				if bypass {
					metrics.DisruptiveWaitExpired.Inc("proceeded")
					msg := fmt.Sprintf("k8s-tailor: a replica of %s has been NotReady for over %s; the live PDB and replica counts allow it, so the disruptive resize of %s continues", p.Owner.Name, r.cfg.DisruptiveMaxWait, c.Name)
					r.kc.RecordEvent(ctx, p.Owner.Kind, ns, p.Owner.Name, "RollingProceeded", msg)
					log.Print(msg)
				}
			}
			if err := r.kc.ResizePodContainer(ctx, ns, p.Name, c.Name, c.Native, cpuStr(targetCPU), memStr(targetMem), cpuLim, memLim); err != nil {
				log.Printf("resize %s/%s[%s]: %v (backing off %s)", ns, p.Name, c.Name, err, failBackoff)
				r.markFailed(ctx, ns, p, c.Name, fkey, err)
				metrics.Resizes.Inc("inplace", "failed")
				r.kc.RecordEventFor(ctx, *podRef, corev1.EventTypeWarning, "ResizeFailed",
					fmt.Sprintf("k8s-tailor: in-place resize of %s rejected (backing off %s): %v", c.Name, failBackoff, err))
				continue
			}
			if disruptive {
				if cy.disrupted[wkey] == nil {
					cy.disrupted[wkey] = map[string]bool{}
				}
				if !cy.disrupted[wkey][p.Name] && p.Ready {
					cy.readyDisrupted[wkey]++
				}
				cy.disrupted[wkey][p.Name] = true
				r.mu.Lock()
				r.rolling[ckey] = true
				r.mu.Unlock()
			} else {
				// Siblings follow in the same cycle only for a non-disruptive resize.
				cy.acted[ckey] = true
			}
			r.markActed(ckey)
			metrics.Resizes.Inc("inplace", "applied")
			r.stampPod(ctx, ns, p, c.Name, disruptive)
			r.store.Record(actuation.Event{
				Time: actuation.Now(), Mode: "inplace", Namespace: ns, Kind: p.Owner.Kind,
				Workload: p.Owner.Name, Container: c.Name,
				FromCPU: c.CPURequest, ToCPU: targetCPU,
				FromMem: c.MemRequest, ToMem: targetMem,
			})
			msg := fmt.Sprintf("in-place resize %s: cpu %s→%s, mem %s→%s%s", c.Name,
				cpuStr(c.CPURequest), cpuStr(targetCPU), memStr(c.MemRequest), memStr(targetMem), heldNote(cpuHeld))
			// On the pod (kubectl describe pod) AND on the workload (the audit trail
			// for the unit a human reasons about).
			r.kc.RecordEventFor(ctx, *podRef, corev1.EventTypeNormal, "Rightsized", msg)
			if p.Owner.Kind != "Pod" {
				r.kc.RecordEvent(ctx, p.Owner.Kind, ns, p.Owner.Name, "Rightsized", msg)
			}
			log.Printf("in-place resize %s/%s[%s] -> cpu=%s mem=%s (was cpu=%s mem=%s)%s",
				ns, p.Owner.Name, c.Name, cpuStr(targetCPU), memStr(targetMem),
				cpuStr(c.CPURequest), memStr(c.MemRequest), heldNote(cpuHeld))
		}
	}
}

// endRolling forgets the disruptive rollings of ns that no pod wants any more.
func (r *Reconciler) endRolling(ns string, cy *cycle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.rolling {
		if strings.HasPrefix(k, ns+"/") && !cy.wantDisrupt[k] {
			delete(r.rolling, k)
		}
	}
}

// stampPod persists the actuation time on the pod (AnnLastApplied), merged with
// the stamps already there; a disruptive resize also marks the pod as a rolling
// step (AnnRolling, same time) and a success clears the container's persisted
// failure (AnnResizeFailed). The pods/resize subresource only accepts resources,
// so this is a separate metadata merge-patch. Best-effort: a failure only means a
// restart would forget this cooldown (or rolling step).
func (r *Reconciler) stampPod(ctx context.Context, ns string, p k8s.RunningPod, container string, disruptive bool) {
	now := r.now()
	merged := func(src map[string]time.Time) string {
		m := make(map[string]time.Time, len(src)+1)
		for k, v := range src {
			m[k] = v
		}
		m[container] = now
		return k8s.FormatLastApplied(m)
	}
	v := merged(p.LastApplied)
	anns := map[string]*string{k8s.AnnLastApplied: &v}
	if disruptive {
		ro := merged(p.Rolling)
		anns[k8s.AnnRolling] = &ro
	}
	if _, failed := p.ResizeFailed[container]; failed {
		rest := map[string]k8s.ResizeFailure{}
		for k, f := range p.ResizeFailed {
			if k != container {
				rest[k] = f
			}
		}
		if len(rest) == 0 {
			anns[k8s.AnnResizeFailed] = nil
		} else {
			fv := k8s.FormatResizeFailed(rest)
			anns[k8s.AnnResizeFailed] = &fv
		}
	}
	if err := r.kc.AnnotateWorkload(ctx, "Pod", ns, p.Name, anns); err != nil {
		log.Printf("stamp %s on pod %s/%s: %v", k8s.AnnLastApplied, ns, p.Name, err)
	}
}

// rollout patches the controller template (once per workload/container) when the
// template request has drifted, letting the native updateStrategy sequence pods.
//
// A container absent from the template (a sidecar injected at admission) is
// NEVER patched into it — a strategic merge on a missing name would append a
// bogus container. Its only durable path is the mesh's own resource annotations
// on the pod template (opt-in: proxyAnnotations), which the injector reads when
// it renders the proxy of every new pod.
func (r *Reconciler) rollout(ctx context.Context, ns string, p k8s.RunningPod, c k8s.Container, entry recommender.Entry, cy *cycle) {
	kind, name := p.Owner.Kind, p.Owner.Name
	key := ns + "/" + kind + "/" + name + "/" + c.Name
	if cy.rolled[key] {
		return
	}
	if !entry.Rec.Confident && !entry.Rec.OOMRisk {
		return
	}
	// Durable target: template container, ksvc, or mesh proxy annotations.
	viaProxy := entry.NotInTemplate
	if viaProxy && (recommender.ProxyAnnotationKeys(entry.Mesh) == nil || !entry.ProxyAnnotations ||
		entry.Rec.Knative.Service != "" || !templateWorkload(kind)) {
		cy.rolled[key] = true
		r.skip(ctx, "not_in_template", &k8s.EventRef{Kind: kind, Namespace: ns, Name: name}, corev1.EventTypeNormal, "ResizeSkipped",
			fmt.Sprintf("k8s-tailor: %s is injected at admission (not in the pod template) — no template patch; use in-place or enable proxyAnnotations for a supported mesh", c.Name))
		return
	}
	if !viaProxy && entry.Native && entry.Rec.Knative.Service != "" {
		cy.rolled[key] = true
		r.skip(ctx, "not_in_template", nil, "", "", "")
		return
	}
	plan, ok := PlanRollout(entry)
	if !ok {
		cy.rolled[key] = true
		return
	}
	cpuHeld, memHeld := plan.CPUHeld, plan.MemHeld
	tmplCPU, tmplMem := entry.Rec.Current.CPURequest, entry.Rec.Current.MemRequest
	targetCPU, targetMem := plan.TargetCPU, plan.TargetMem
	cpuLimitV, memLimitV := plan.CPULimit, plan.MemLimit
	// A rollout is disruptive — floor the cadence hard so we never churn it, even
	// if the recommendation keeps drifting between cycles.
	if !r.cooled(key, plan.Up, entry, MinRolloutInterval, lastAppliedOf(cy.pods, kind, name, c.Name)) {
		cy.rolled[key] = true // don't retry this cycle
		r.skip(ctx, "cooldown", nil, "", "", "")
		return
	}
	if viaProxy {
		// Any proxy* annotation replaces the mesh-wide default resources, limits
		// included: carry the live limits over or the proxy would lose them.
		if cpuLimitV == 0 && entry.Rec.Current.CPULimit > 0 {
			cpuLimitV = math.Max(entry.Rec.Current.CPULimit, targetCPU)
		}
		if memLimitV == 0 && entry.Rec.Current.MemLimit > 0 {
			memLimitV = math.Max(entry.Rec.Current.MemLimit, targetMem)
		}
	}
	cpuLimit, memLimit := "", ""
	if cpuLimitV > 0 {
		cpuLimit = cpuStr(cpuLimitV)
	}
	if memLimitV > 0 {
		memLimit = memStr(memLimitV)
	}

	// Durable cooldown: stamp the template so every rolled pod carries the
	// actuation time (merged with this cycle's other containers' stamps).
	wkey := ns + "/" + kind + "/" + name
	st := cy.stamps[wkey]
	if st == nil {
		st = map[string]time.Time{}
		for k, v := range p.LastApplied {
			st[k] = v
		}
		cy.stamps[wkey] = st
	}
	st[c.Name] = r.now()
	anns := map[string]string{k8s.AnnLastApplied: k8s.FormatLastApplied(st)}

	// Knative: the revision Deployment is immutable — edit the ksvc, which mints a
	// new revision. Everything else patches its controller template directly.
	var err error
	mode := "rollout"
	evKind, evName := kind, name
	switch {
	case viaProxy:
		mode = "proxy-annotations"
		err = r.kc.PatchWorkloadPodAnnotations(ctx, kind, ns, name,
			proxyAnnotations(entry.Mesh, cpuStr(targetCPU), memStr(targetMem), cpuLimit, memLimit, anns))
	case entry.Rec.Knative.Service != "":
		mode = "ksvc"
		evKind, evName = "Service", entry.Rec.Knative.Service // event on the ksvc
		err = r.kc.PatchKnativeService(ctx, ns, evName, c.Name, cpuStr(targetCPU), memStr(targetMem), cpuLimit, memLimit, anns)
	default:
		err = r.kc.PatchWorkloadResources(ctx, kind, ns, name, c.Name, entry.Native, cpuStr(targetCPU), memStr(targetMem), cpuLimit, memLimit, anns)
	}
	if errors.Is(err, k8s.ErrNoTemplateContainer) {
		// Nothing to patch (container not in the ksvc template): not a failure to
		// retry, and certainly not an actuation — no "applied" event/metric/history.
		delete(st, c.Name)
		cy.rolled[key] = true
		r.skip(ctx, "no_template_target", &k8s.EventRef{Kind: evKind, Namespace: ns, Name: evName},
			corev1.EventTypeNormal, "ResizeSkipped", fmt.Sprintf("k8s-tailor: %s is not in the %s template — nothing to patch", c.Name, mode))
		return
	}
	if err != nil {
		log.Printf("%s %s/%s[%s]: %v", mode, ns, name, c.Name, err)
		metrics.Resizes.Inc(mode, "failed")
		delete(st, c.Name)
		// Deduplicated: a deterministic rejection would otherwise emit every tick.
		r.skip(ctx, "patch_failed", &k8s.EventRef{Kind: evKind, Namespace: ns, Name: evName},
			corev1.EventTypeWarning, "RolloutFailed", fmt.Sprintf("k8s-tailor: %s patch of %s failed: %v", mode, c.Name, err))
		return
	}
	cy.rolled[key] = true
	r.markActed(key)
	metrics.Resizes.Inc(mode, "applied")
	r.store.Record(actuation.Event{
		Time: actuation.Now(), Mode: mode, Namespace: ns, Kind: kind,
		Workload: name, Container: c.Name,
		FromCPU: tmplCPU, ToCPU: targetCPU,
		FromMem: tmplMem, ToMem: targetMem,
	})
	r.kc.RecordEvent(ctx, evKind, ns, evName, "Rightsized",
		fmt.Sprintf("%s %s: cpu %s→%s, mem %s→%s%s", mode, c.Name,
			cpuStr(tmplCPU), cpuStr(targetCPU), memStr(tmplMem), memStr(targetMem), heldNote(cpuHeld || memHeld)))
	log.Printf("%s %s/%s[%s] -> cpu=%s mem=%s%s",
		mode, ns, name, c.Name, cpuStr(targetCPU), memStr(targetMem), heldNote(cpuHeld || memHeld))
}

// templateWorkload: kinds whose pod template we can patch.
func templateWorkload(kind string) bool {
	return kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet"
}

// proxyAnnotations builds the pod-template annotation patch that sizes a mesh
// proxy. Requests are always written. Limits are written when known and
// REMOVED (nil) otherwise: with Istio, the presence of any proxy* annotation
// replaces the mesh-wide default resources entirely, so the limits written here
// are exactly the ones the proxy will get (the caller passes the live limits,
// floored, so an existing limit is preserved).
func proxyAnnotations(mesh, cpu, mem, cpuLimit, memLimit string, extra map[string]string) map[string]*string {
	keys := recommender.ProxyAnnotationKeys(mesh)
	out := map[string]*string{}
	set := func(k, v string) {
		if v == "" {
			out[k] = nil
			return
		}
		vv := v
		out[k] = &vv
	}
	set(keys[0], cpu)
	set(keys[1], mem)
	set(keys[2], cpuLimit)
	set(keys[3], memLimit)
	for k, v := range extra {
		set(k, v)
	}
	return out
}

func heldNote(cpuHeld bool) string {
	if cpuHeld {
		return " (CPU held — HPA or not enough CPU data)"
	}
	return ""
}
