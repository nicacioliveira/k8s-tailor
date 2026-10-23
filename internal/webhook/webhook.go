// Package webhook implements the mutating admission webhook that applies
// recommended requests/limits to pods at creation time.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/reconciler"
)

// Webhook never writes the decision log itself: /mutate can be reached by
// anything that talks to :8443, and a forged AdmissionReview must not be able
// to fabricate "applied" decisions. The patch marks the pod (k8s.AnnAdmissionSized)
// and the leader's reconciler records the decision when it sees the pod in the
// API server.
type Webhook struct {
	cfg config.Config
	eng *recommender.Engine
	kc  *k8s.Client

	// resolveOwner looks up the pod's workload (an API GET for ReplicaSet-owned
	// pods). Indirected so tests can observe the context it receives.
	resolveOwner func(ctx context.Context, ns string, labels map[string]string, owners []metav1.OwnerReference) (k8s.Owner, bool)
}

// resolveTimeout bounds the owner lookup made inside the admission request. The
// API server's webhook timeout (timeoutSeconds, default 10s, failurePolicy
// Ignore) must never be the thing that cuts us off: give up early and admit the
// pod unmodified instead.
const resolveTimeout = 3 * time.Second

// maxReviewBytes bounds the AdmissionReview body read from the API server. A
// review carries the object (etcd caps it at ~1.5MiB) and, on UPDATE, the old
// object too; 8MiB is ample headroom and stops an unbounded read.
const maxReviewBytes = 8 << 20

func New(cfg config.Config, eng *recommender.Engine, kc *k8s.Client) *Webhook {
	return &Webhook{cfg: cfg, eng: eng, kc: kc, resolveOwner: kc.ResolvePodOwner}
}

// genuinePodCreate reports whether req is what the MutatingWebhookConfiguration
// sends us: a CREATE of a core/v1 Pod, no subresource. Anything else (a forged
// review, or a future config change that widens the rules) is admitted untouched.
func genuinePodCreate(req *admissionv1.AdmissionRequest) bool {
	return req.Operation == admissionv1.Create &&
		req.SubResource == "" &&
		req.Kind.Group == "" && req.Kind.Version == "v1" && req.Kind.Kind == "Pod" &&
		req.Resource.Group == "" && req.Resource.Resource == "pods"
}

type patchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func cpuStr(cores float64) string { return fmt.Sprintf("%dm", int64(cores*1000+0.5)) }
func memStr(bytes float64) string { return fmt.Sprintf("%dMi", int64(bytes/(1024*1024)+0.5)) }

func (h *Webhook) Handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReviewBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "review too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "invalid review", http.StatusBadRequest)
		return
	}
	req := review.Request

	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}

	ctx, cancel := context.WithTimeout(r.Context(), resolveTimeout)
	defer cancel()
	var patch []patchOp
	if genuinePodCreate(req) {
		patch = h.buildPatch(ctx, req)
	}
	if len(patch) > 0 {
		pb, _ := json.Marshal(patch)
		pt := admissionv1.PatchTypeJSONPatch
		resp.Patch = pb
		resp.PatchType = &pt
	}

	out := admissionv1.AdmissionReview{TypeMeta: review.TypeMeta, Response: resp}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Webhook) buildPatch(ctx context.Context, req *admissionv1.AdmissionRequest) []patchOp {
	// Global kill switch (TailorConfig.spec.paused). Scope is decided entirely
	// by the resolved policy mode per container below — activation lives in the CRs.
	if h.eng.Paused() {
		return nil
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return nil
	}

	owner, ok := h.resolveOwner(ctx, req.Namespace, pod.Labels, pod.OwnerReferences)
	if !ok {
		return nil
	}
	// No refresh has completed since the engine started: there is no
	// recommendation to apply yet. The first refresh reads the full history from
	// the metrics backend, so this lasts one refresh (seconds), not MinSamples.
	// Admit unmodified, but visibly.
	if !h.eng.Ready() {
		metrics.WebhookSkips.Inc("warming_up")
		log.Printf("webhook: %s/%s admitted unmodified: engine warming up (no refresh completed yet)", req.Namespace, owner.Name)
		return nil
	}

	// QoS class is decided at creation: a Guaranteed template (requests ==
	// limits for CPU and memory on every container) given smaller requests would
	// be born Burstable — a different eviction class than its owner declared.
	// Same rule as the reconciler: such a container is admitted unmodified, but
	// visibly, once per container we would have sized — unless its policy says
	// guaranteed: resizeBoth, in which case request and limit are set together
	// below and the pod stays Guaranteed.
	guaranteed := guaranteedPod(&pod)

	// Size app containers and native sidecars (initContainers with restartPolicy
	// Always — e.g. istio-proxy on Istio >= 1.27). Regular init containers are
	// never sized. Paths are computed on THIS invocation's object, so a container
	// another webhook adds/removes between (re)invocations is handled naturally,
	// and a container already at its recommendation yields no op (idempotent —
	// required by reinvocationPolicy: IfNeeded).
	var ops []patchOp
	// Decisions of this invocation, merged over a previous invocation's marker
	// (reinvocationPolicy: IfNeeded patches the same pod more than once).
	decisions, _ := k8s.ParseAdmissionSized(pod.Annotations[k8s.AnnAdmissionSized])
	if decisions == nil {
		decisions = map[string]k8s.AdmissionDecision{}
	}
	type target struct {
		list string
		i    int
		c    *corev1.Container
	}
	var targets []target
	for i := range pod.Spec.Containers {
		targets = append(targets, target{"containers", i, &pod.Spec.Containers[i]})
	}
	for i := range pod.Spec.InitContainers {
		ic := &pod.Spec.InitContainers[i]
		if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			targets = append(targets, target{"initContainers", i, ic})
		}
	}
	for _, tg := range targets {
		c := tg.c
		entry, ok := h.eng.Admission(req.Namespace, owner.Kind, owner.Name, c.Name)
		if !ok {
			continue
		}
		if !entry.Mode.Webhook() || !entry.Class.Resizable() { // per-container effective mode
			continue
		}
		// Pod-level resources decide the QoS from spec.resources, not from the
		// containers: request == limit per container says nothing there.
		if guaranteed && (!entry.GuaranteedResize || pod.Spec.Resources != nil) {
			metrics.WebhookSkips.Inc("guaranteed_qos")
			continue
		}
		// Same gate as the reconciler: don't act on thin data — except an OOMKill.
		if !entry.Rec.Confident && !entry.Rec.OOMRisk {
			metrics.WebhookSkips.Inc("not_confident")
			continue
		}
		rec := entry.Rec.Recommended

		// The pod's own resources are the baseline. The decision itself — HPA
		// holds (CPU/memory), confidence per resource, direction gate, OOM
		// override, drift tolerance, OOM limit raise and limit rules — is the
		// reconciler's PlanInPlace / Plan.Limits, so admission and live resize can
		// never disagree.
		cur := k8s.Container{Name: c.Name}
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cur.CPURequest = q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			cur.MemRequest = q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			cur.CPULimit = q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			cur.MemLimit = q.AsApproximateFloat64()
		}
		fromCPU, fromMem := cur.CPURequest, cur.MemRequest
		plan := reconciler.PlanInPlace(entry, cur)
		if guaranteed && !plan.Guaranteed {
			metrics.WebhookSkips.Inc("guaranteed_qos")
			continue
		}
		if plan.GuaranteedMemShrinkHeld {
			metrics.WebhookSkips.Inc("guaranteed_mem_shrink")
		}
		if plan.TargetCPU <= 0 {
			plan.CPUDrift = false // never write a zero request
		}
		if plan.TargetMem <= 0 {
			plan.MemDrift, plan.MemLimitRaise = false, false
		}
		if !plan.Wants() {
			continue // held, gated or within tolerance — nothing to do
		}
		cpuLim, memLim := plan.Limits(rec, cur)

		// Start from the pod's EXISTING resources and override ONLY the cpu/mem that
		// actually moved. A JSON-Patch replace of the whole /resources object would
		// silently drop ephemeral-storage, hugepages, extended resources (e.g.
		// nvidia.com/gpu) and DRA resources.claims; copying first preserves them, and
		// a held resource keeps its template request+limit untouched.
		val := map[string]interface{}{}
		if b, err := json.Marshal(c.Resources); err == nil {
			_ = json.Unmarshal(b, &val)
		}
		requests, _ := val["requests"].(map[string]interface{})
		if requests == nil {
			requests = map[string]interface{}{}
		}
		limits, _ := val["limits"].(map[string]interface{})
		if limits == nil {
			limits = map[string]interface{}{}
		}
		gCPU, gMem := fromCPU, fromMem
		if plan.CPUDrift {
			gCPU = plan.TargetCPU
			requests["cpu"] = cpuStr(gCPU)
		}
		if plan.MemDrift {
			gMem = plan.TargetMem
			requests["memory"] = memStr(gMem)
		}
		if cpuLim > 0 {
			limits["cpu"] = cpuStr(cpuLim)
		}
		if memLim > 0 {
			limits["memory"] = memStr(memLim)
		}
		val["requests"] = requests
		if len(limits) > 0 {
			val["limits"] = limits
		}
		ops = append(ops, patchOp{
			Op:    "add",
			Path:  fmt.Sprintf("/spec/%s/%d/resources", tg.list, tg.i),
			Value: val,
		})
		// Track the mutated resources on our local copy (pod-level check below).
		if b, err := json.Marshal(val); err == nil {
			var rr corev1.ResourceRequirements
			if json.Unmarshal(b, &rr) == nil {
				c.Resources = rr
			}
		}
		decisions[c.Name] = k8s.AdmissionDecision{FromCPU: fromCPU, ToCPU: gCPU, FromMem: fromMem, ToMem: gMem}
		log.Printf("webhook: mutating %s/%s container %s -> cpu=%s mem=%s (dir=%s)",
			req.Namespace, owner.Name, c.Name, cpuStr(gCPU), memStr(gMem), entry.Direction)
	}
	// Pod-level resources (spec.resources): the sum of the containers' requests
	// must stay <= the pod's and every container limit <= the pod's limit, or the
	// mutated pod is INVALID and its CREATE fails (failurePolicy: Ignore does not
	// cover a bad patch). All-or-nothing: drop our patch rather than break it.
	if len(ops) > 0 && !podLevelOK(&pod) {
		log.Printf("webhook: %s/%s: recommendation exceeds pod-level resources — admitting unmodified", req.Namespace, owner.Name)
		return nil
	}
	if len(ops) == 0 {
		return nil
	}
	// Mark the pod; nothing is recorded here. The reconciler records the decision
	// once the pod exists (so a dry-run, a CREATE rejected later in admission, or
	// a forged review never reaches the decision log or tailor_mutations_total).
	if b, err := json.Marshal(decisions); err == nil {
		if pod.Annotations == nil {
			ops = append(ops, patchOp{Op: "add", Path: "/metadata/annotations",
				Value: map[string]string{k8s.AnnAdmissionSized: string(b)}})
		} else {
			ops = append(ops, patchOp{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscape(k8s.AnnAdmissionSized),
				Value: string(b)})
		}
	}
	return ops
}

// guaranteedPod reports whether the pod will be QoS Guaranteed, computed the way
// the API server does (k8s.io/kubernetes pkg/apis/core/helper/qos): with
// pod-level resources (spec.resources) set, from those; otherwise every
// container — app, init (regular and sidecar) — must have CPU and memory limits,
// and requests equal to them (an unset request defaults to its limit).
func guaranteedPod(p *corev1.Pod) bool {
	eq := func(r corev1.ResourceRequirements) bool {
		for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			l, ok := r.Limits[n]
			if !ok || l.IsZero() {
				return false
			}
			if q, ok := r.Requests[n]; ok && q.Cmp(l) != 0 {
				return false
			}
		}
		return true
	}
	if p.Spec.Resources != nil && (len(p.Spec.Resources.Limits) > 0 || len(p.Spec.Resources.Requests) > 0) {
		return eq(*p.Spec.Resources)
	}
	all := append(append([]corev1.Container{}, p.Spec.Containers...), p.Spec.InitContainers...)
	if len(all) == 0 {
		return false
	}
	for _, c := range all {
		if !eq(c.Resources) {
			return false
		}
	}
	return true
}

// jsonPointerEscape escapes a map key for a JSON-Patch path (RFC 6901).
func jsonPointerEscape(k string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
}

// podLevelOK validates container resources against pod-level spec.resources.
func podLevelOK(p *corev1.Pod) bool {
	pr := p.Spec.Resources
	if pr == nil {
		return true
	}
	long := append([]corev1.Container{}, p.Spec.Containers...)
	for _, ic := range p.Spec.InitContainers {
		if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			long = append(long, ic)
		}
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		var sum float64
		for _, c := range long {
			if q, ok := c.Resources.Requests[res]; ok {
				sum += q.AsApproximateFloat64()
			}
			if pl, ok := pr.Limits[res]; ok {
				if q, ok := c.Resources.Limits[res]; ok && q.Cmp(pl) > 0 {
					return false
				}
			}
		}
		if pq, ok := pr.Requests[res]; ok && sum > pq.AsApproximateFloat64()*(1+1e-9) {
			return false
		}
	}
	return true
}
