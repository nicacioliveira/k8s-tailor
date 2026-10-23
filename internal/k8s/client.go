// Package k8s reads workload specs and pod ownership (read-only) via client-go.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

type Client struct {
	cs      kubernetes.Interface
	dyn     dynamic.Interface
	exclude map[string]bool

	// In-place resize support (pods/resize discovery), cached with a TTL. A
	// discovery ERROR is "unknown" and retried after inPlaceRetry — never
	// cached as "unsupported" (P1-7).
	inPlaceMu    sync.Mutex
	inPlaceOK    bool
	inPlaceKnown bool
	inPlaceAt    time.Time // last discovery attempt
	clock        func() time.Time

	glMu      sync.RWMutex
	groupKeys []GroupKey // label keys that collapse workloads into one unit (TailorGroup)

	cls classifierHolder // container classification (sidecar name globs)
}

// GroupKey pairs a collapse label with the logical Kind the collapsed unit gets.
// Knative groups become kind "ksvc" (actuated by patching the Service); every
// other TailorGroup (e.g. agent-sandbox pods) becomes kind "group" — a synthetic
// unit whose members are ephemeral pods, actuated at admission via the webhook.
type GroupKey struct {
	Label      string
	Kind       string
	Namespaces []string // namespace globs this key applies to (empty = all)
	// Cohort (optional): within a group, restrict which "generation" of pods counts
	// toward usage. CohortLabel is the generation label (e.g. a Knative revision, a
	// Deployment pod-template-hash, an Argo Rollouts hash); CohortSelect "latest"
	// keeps only the newest cohort (by pod creation time), "" / "all" keeps every
	// cohort (the default). Generic — knows nothing about the specific controller.
	CohortLabel  string
	CohortSelect string
	// Source is the name of the TailorGroup CR the key comes from ("" for the
	// built-in Knative key): the object a "group" unit's state (OOM floors) is
	// persisted on, since the unit itself has no object.
	Source string
}

// knativeGroupKey is always included so Knative works out of the box.
var knativeGroupKey = GroupKey{Label: lblKnativeService, Kind: "ksvc"}

// nsMatch reports whether ns matches any of the globs (empty globs = all).
func nsMatch(globs []string, ns string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if g == ns {
			return true
		}
		if strings.ContainsAny(g, "*?[") {
			if ok, err := filepath.Match(g, ns); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// SetGroupKeys updates the collapse keys (from the TailorGroup rules). The
// Knative service key is always present (first) so Knative works out of the box;
// a user TailorGroup targeting the Knative service label doesn't add a duplicate
// — it MERGES its namespaces/cohort into the built-in key (keeping kind ksvc), so
// `cohort: {select: latest}` on the Knative group takes effect instead of being
// shadowed by the built-in.
func (c *Client) SetGroupKeys(keys []GroupKey) {
	kn := knativeGroupKey
	seen := map[string]bool{}
	var rest []GroupKey
	for _, k := range keys {
		if k.Label == "" {
			continue
		}
		if k.Label == lblKnativeService {
			kn.Namespaces = k.Namespaces
			kn.CohortLabel = k.CohortLabel
			kn.CohortSelect = k.CohortSelect
			continue
		}
		if seen[k.Label] {
			continue
		}
		if k.Kind == "" {
			k.Kind = "group"
		}
		seen[k.Label] = true
		rest = append(rest, k)
	}
	c.glMu.Lock()
	c.groupKeys = append([]GroupKey{kn}, rest...)
	c.glMu.Unlock()
}

// matchGroupKey returns the first group key whose namespace scope matches and
// whose label is present on the workload/pod, plus that label's value.
func (c *Client) matchGroupKey(namespace string, labels map[string]string) (GroupKey, string, bool) {
	c.glMu.RLock()
	keys := c.groupKeys
	c.glMu.RUnlock()
	if keys == nil {
		keys = []GroupKey{knativeGroupKey}
	}
	for _, k := range keys {
		if !nsMatch(k.Namespaces, namespace) {
			continue
		}
		// "namespace" is a synthetic collapse key: every workload in the
		// namespace folds into one unit, keyed by the namespace name. Use it
		// when pods share no common label but the namespace IS the logical
		// boundary (e.g. the per-env StatefulSets in a site namespace).
		if k.Label == "namespace" {
			return k, namespace, true
		}
		if v := labels[k.Label]; v != "" {
			return k, v, true
		}
	}
	return GroupKey{}, "", false
}

// groupValueOf returns the collapsed unit's (kind, group id) from a workload/pod's
// namespace + labels.
func (c *Client) groupValueOf(namespace string, labels map[string]string) (kind, value string, ok bool) {
	k, v, ok := c.matchGroupKey(namespace, labels)
	return k.Kind, v, ok
}

// WorkloadRef is a lightweight member listing (drill-down: what a group folds).
type WorkloadRef struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Replicas  int    `json:"replicas"`
	Unit      string `json:"unit"` // the collapsed unit this member belongs to
}

// GroupMembers lists the raw workloads a group rule folds — the members behind
// a collapsed unit (drill-down). groupBy "namespace" matches every workload in a
// matching namespace; a label key matches workloads carrying that label. Unit is
// the value the member collapses under (the namespace, or the label's value).
func (c *Client) GroupMembers(ctx context.Context, groupBy string, namespaces []string) []WorkloadRef {
	out := []WorkloadRef{}
	unitOf := func(ns string, labels map[string]string) (string, bool) {
		if !nsMatch(namespaces, ns) {
			return "", false
		}
		if groupBy == "namespace" {
			return ns, true
		}
		if v := labels[groupBy]; v != "" {
			return v, true
		}
		return "", false
	}
	if deps, err := c.cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{}); err == nil {
		for i := range deps.Items {
			d := &deps.Items[i]
			if c.exclude[d.Namespace] {
				continue
			}
			if u, ok := unitOf(d.Namespace, d.Labels); ok {
				r := 1
				if d.Spec.Replicas != nil {
					r = int(*d.Spec.Replicas)
				}
				out = append(out, WorkloadRef{Namespace: d.Namespace, Kind: "Deployment", Name: d.Name, Replicas: r, Unit: u})
			}
		}
	}
	if sts, err := c.cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{}); err == nil {
		for i := range sts.Items {
			s := &sts.Items[i]
			if c.exclude[s.Namespace] {
				continue
			}
			if u, ok := unitOf(s.Namespace, s.Labels); ok {
				r := 1
				if s.Spec.Replicas != nil {
					r = int(*s.Spec.Replicas)
				}
				out = append(out, WorkloadRef{Namespace: s.Namespace, Kind: "StatefulSet", Name: s.Name, Replicas: r, Unit: u})
			}
		}
	}
	if dss, err := c.cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{}); err == nil {
		for i := range dss.Items {
			d := &dss.Items[i]
			if c.exclude[d.Namespace] {
				continue
			}
			if u, ok := unitOf(d.Namespace, d.Labels); ok {
				out = append(out, WorkloadRef{Namespace: d.Namespace, Kind: "DaemonSet", Name: d.Name, Replicas: int(d.Status.DesiredNumberScheduled), Unit: u})
			}
		}
	}
	return out
}

// staleCohortPods returns the set of pod "namespace/name" that belong to a
// NON-active cohort (generation) of a group whose matched key restricts cohorts.
// Idle pods of a superseded generation would otherwise dilute the group's usage.
// Two selection modes:
//   - "latest": active cohort = the one holding the newest pod (by creation time).
//     Fully generic — never inspects any controller-specific resource.
//   - "activeByTraffic": active cohorts = the Knative revisions currently receiving
//     traffic (ksvc status.traffic percent>0). Handles canaries, where more than one
//     revision serves at once. Falls back to counting all when traffic is unknown.
//
// Pods without a cohort label, or in groups without a cohort key, are never stale.
func (c *Client) staleCohortPods(ctx context.Context, items []corev1.Pod) map[string]bool {
	c.glMu.RLock()
	needTraffic := false
	for _, k := range c.groupKeys {
		if k.CohortSelect == "activeByTraffic" {
			needTraffic = true
			break
		}
	}
	c.glMu.RUnlock()
	var trafficRevs map[string]map[string]bool // "ns/service" -> set of revisions with traffic
	if needTraffic {
		trafficRevs = c.knativeActiveRevisions(ctx)
	}

	// latest-mode: track newest pod creation per (group, cohort).
	newest := map[string]map[string]metav1.Time{}
	type podRef struct {
		podKey, gvKey, cohort, mode string
	}
	var refs []podRef
	for i := range items {
		p := &items[i]
		k, gv, ok := c.matchGroupKey(p.Namespace, p.Labels)
		if !ok || k.CohortLabel == "" || (k.CohortSelect != "latest" && k.CohortSelect != "activeByTraffic") {
			continue
		}
		cohort := p.Labels[k.CohortLabel]
		if cohort == "" {
			continue // can't classify → count it (never stale)
		}
		gvKey := p.Namespace + "/" + gv
		refs = append(refs, podRef{podKey: p.Namespace + "/" + p.Name, gvKey: gvKey, cohort: cohort, mode: k.CohortSelect})
		if k.CohortSelect == "latest" {
			if newest[gvKey] == nil {
				newest[gvKey] = map[string]metav1.Time{}
			}
			if t, seen := newest[gvKey][cohort]; !seen || p.CreationTimestamp.After(t.Time) {
				newest[gvKey][cohort] = p.CreationTimestamp
			}
		}
	}
	// latest active cohort per group = the one holding the newest pod.
	activeLatest := map[string]string{}
	for gvKey, cohorts := range newest {
		var bestCohort string
		var bestTime metav1.Time
		for cohort, t := range cohorts {
			if bestCohort == "" || t.After(bestTime.Time) {
				bestCohort, bestTime = cohort, t
			}
		}
		activeLatest[gvKey] = bestCohort
	}
	stale := map[string]bool{}
	for _, r := range refs {
		switch r.mode {
		case "activeByTraffic":
			set := trafficRevs[r.gvKey]
			if len(set) == 0 {
				continue // traffic unknown (no status yet) → count it, safe
			}
			if !set[r.cohort] {
				stale[r.podKey] = true
			}
		default: // latest
			if activeLatest[r.gvKey] != "" && activeLatest[r.gvKey] != r.cohort {
				stale[r.podKey] = true
			}
		}
	}
	return stale
}

// knativeActiveRevisions maps "namespace/service" -> set of revision names that are
// currently receiving traffic, read from each ksvc's status.traffic (percent>0).
// Used by the activeByTraffic cohort mode. Empty/absent status → the service is
// simply omitted (callers then count all of its pods).
func (c *Client) knativeActiveRevisions(ctx context.Context) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	list, err := c.dyn.Resource(ksvcGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for i := range list.Items {
		k := &list.Items[i]
		traffic, found, _ := unstructured.NestedSlice(k.Object, "status", "traffic")
		if !found {
			continue
		}
		set := map[string]bool{}
		for _, t := range traffic {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			rev, _ := tm["revisionName"].(string)
			if rev == "" {
				continue
			}
			pct := 0.0
			switch v := tm["percent"].(type) {
			case int64:
				pct = float64(v)
			case float64:
				pct = v
			}
			if pct > 0 {
				set[rev] = true
			}
		}
		if len(set) > 0 {
			out[k.GetNamespace()+"/"+k.GetName()] = set
		}
	}
	return out
}

// Knative Service GVR — durable rightsizing of a Knative workload targets the
// ksvc (which mints a new Revision), never the revision's immutable Deployment.
var ksvcGVR = schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}

// Knative labels Knative stamps on the per-revision Deployment/pods.
const (
	lblKnativeService  = "serving.knative.dev/service"
	lblKnativeRevision = "serving.knative.dev/revision"
)

// RESTConfig builds the client-go rest config. In-cluster uses the SA; local
// dev forces an explicit context (default rancher-desktop) so we never talk to
// whatever the current kubeconfig context happens to be (which may be prod).
func RESTConfig(cfg config.Config) (*rest.Config, error) {
	if cfg.Source == "in-cluster" {
		return rest.InClusterConfig()
	}
	ctxName := os.Getenv("KUBE_CONTEXT")
	if ctxName == "" {
		ctxName = "rancher-desktop"
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName},
	).ClientConfig()
}

func New(cfg config.Config) (*Client, error) {
	rc, err := RESTConfig(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &Client{cs: cs, dyn: dyn, exclude: cfg.ExcludeNamespaces}, nil
}

// NewFromClients builds a Client over already-constructed typed + dynamic
// clients. Used by tests (client-go fakes) and by callers that manage their own
// rest.Config.
func NewFromClients(cs kubernetes.Interface, dyn dynamic.Interface, exclude map[string]bool) *Client {
	return &Client{cs: cs, dyn: dyn, exclude: exclude}
}

type Container struct {
	Name                 string
	CPURequest, CPULimit float64
	MemRequest, MemLimit float64
	// For a RUNNING pod, the four values above are what the kubelet actually
	// effected (status.containerStatuses[].resources) when reported; the spec
	// (desired) requests are kept here. Zero for workload templates.
	SpecCPURequest, SpecMemRequest float64
	MemRestartOnResize             bool // resizePolicy: memory=RestartContainer → resize is disruptive
	CPURestartOnResize             bool // resizePolicy: cpu=RestartContainer → resize is disruptive

	// Class is the container's role in the pod (app, injected-sidecar,
	// native-sidecar, init, ephemeral). See containers.go.
	Class ContainerClass
	// Native: the container lives in spec.initContainers with restartPolicy
	// Always (Kubernetes native sidecar) — patches target initContainers.
	Native bool
	// InTemplate: the container exists in the controller's pod template, so a
	// rollout may patch it. False for containers seen only on running pods
	// (injected by a webhook) — those are NEVER patched into the template.
	InTemplate bool
	// Mesh: "istio" | "linkerd" when the mesh injector's marker is on the pod.
	Mesh string
}

// MemCeilings is the cluster-derived upper bound of a memory request (opt-in
// tailor.rbac.clusterLookups, needs nodes and limitranges list): NodeMax is the
// largest allocatable memory among nodes that could still receive a pod (Ready,
// not cordoned; taints and affinities are not evaluated), LimitRangeMax the
// lowest Container-type `max.memory` per namespace.
type MemCeilings struct {
	NodeMax       float64
	LimitRangeMax map[string]float64
}

// For returns the ceiling of namespace ns: the lower of the node and LimitRange
// bounds that are known, 0 when neither is.
func (m MemCeilings) For(ns string) float64 {
	c := m.NodeMax
	if lr := m.LimitRangeMax[ns]; lr > 0 && (c <= 0 || lr < c) {
		c = lr
	}
	return c
}

// MemoryCeilings lists nodes and LimitRanges. It errors when either list is
// forbidden, so a missing RBAC rule shows up in the log instead of silently
// capping nothing.
func (c *Client) MemoryCeilings(ctx context.Context) (MemCeilings, error) {
	out := MemCeilings{LimitRangeMax: map[string]float64{}}
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return MemCeilings{}, fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable || !nodeReady(n) {
			continue
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok && q.AsApproximateFloat64() > out.NodeMax {
			out.NodeMax = q.AsApproximateFloat64()
		}
	}
	lrs, err := c.cs.CoreV1().LimitRanges("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return MemCeilings{}, fmt.Errorf("list limitranges: %w", err)
	}
	for i := range lrs.Items {
		lr := &lrs.Items[i]
		for _, it := range lr.Spec.Limits {
			if it.Type != corev1.LimitTypeContainer {
				continue
			}
			q, ok := it.Max[corev1.ResourceMemory]
			if !ok {
				continue
			}
			v := q.AsApproximateFloat64()
			if cur, has := out.LimitRangeMax[lr.Namespace]; !has || v < cur {
				out.LimitRangeMax[lr.Namespace] = v
			}
		}
	}
	return out, nil
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// GuaranteedShape reports whether the container has CPU and memory limits equal
// to its requests (an unset request defaults to its limit, as the API server
// does). A pod made only of such containers is QoS Guaranteed.
func (c Container) GuaranteedShape() bool {
	if c.CPULimit <= 0 || c.MemLimit <= 0 {
		return false
	}
	rc, rm := c.CPURequest, c.MemRequest
	if rc <= 0 {
		rc = c.CPULimit
	}
	if rm <= 0 {
		rm = c.MemLimit
	}
	return nearlyEqual(rc, c.CPULimit) && nearlyEqual(rm, c.MemLimit)
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	m := a
	if b > m {
		m = b
	}
	return d <= 1e-6*m+1e-9
}

type Workload struct {
	Namespace   string
	Kind        string
	Name        string
	Replicas    int
	Labels      map[string]string
	Annotations map[string]string
	Containers  []Container

	// PodDisruptionBudget coverage (from the workload's pod-template labels).
	PDBName      string
	PDBProtected bool
	PDBAllowed   int // status.disruptionsAllowed; 0 = a disruption would violate the budget

	// Autoscaler coexistence — we must not fight a horizontal autoscaler.
	Autoscaler Autoscaler
	// Knative — the revision Deployment is immutable; durable resize targets the ksvc.
	Knative KnativeRef
	// GroupSource: for a "group" unit, the TailorGroup CR whose rule folded it
	// (GroupKey.Source); empty otherwise.
	GroupSource string
}

// Autoscaler describes a horizontal autoscaler that targets this workload.
type Autoscaler struct {
	Kind       string `json:"kind"`       // "HPA" | "KPA" | ""
	Name       string `json:"name"`       // HPA name (KPA = the revision)
	ManagesCPU bool   `json:"managesCpu"` // HPA scales on CPU utilization → we must NOT touch CPU requests
	ManagesMem bool   `json:"managesMem"` // HPA scales on memory utilization → hold memory too
	Min        int    `json:"min"`
	Max        int    `json:"max"`
}

// KnativeRef marks a Knative-managed workload and points at its owning ksvc.
type KnativeRef struct {
	Service  string `json:"service"`  // ksvc name ("" = not Knative)
	Revision string `json:"revision"` // current revision
}

// pdbIndex holds compiled PodDisruptionBudget selectors per namespace so a
// workload's pod-template labels can be matched to its budget.
type pdbIndex map[string][]pdbEntry

type pdbEntry struct {
	name    string
	sel     labels.Selector
	allowed int
}

func (idx pdbIndex) match(ns string, lbls map[string]string) (string, bool, int) {
	for _, e := range idx[ns] {
		if e.sel != nil && !e.sel.Empty() && e.sel.Matches(labels.Set(lbls)) {
			return e.name, true, e.allowed
		}
	}
	return "", false, 0
}

func (c *Client) loadPDBs(ctx context.Context) pdbIndex {
	idx := pdbIndex{}
	list, err := c.cs.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return idx // PDBs are advisory here; absence just means "unprotected"
	}
	for i := range list.Items {
		p := &list.Items[i]
		if c.exclude[p.Namespace] || p.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(p.Spec.Selector)
		if err != nil {
			continue
		}
		idx[p.Namespace] = append(idx[p.Namespace], pdbEntry{
			name: p.Name, sel: sel, allowed: int(p.Status.DisruptionsAllowed),
		})
	}
	return idx
}

// hpaIndex maps "namespace/kind/name" of a scale target to its HPA.
type hpaIndex map[string]Autoscaler

func (c *Client) loadHPAs(ctx context.Context) hpaIndex {
	idx := hpaIndex{}
	list, err := c.cs.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return idx // autoscaling/v2 absent or no access — treat as no HPA
	}
	for i := range list.Items {
		h := &list.Items[i]
		if c.exclude[h.Namespace] {
			continue
		}
		ref := h.Spec.ScaleTargetRef
		managesCPU, managesMem := false, false
		for _, m := range h.Spec.Metrics {
			if m.Resource != nil {
				switch m.Resource.Name {
				case corev1.ResourceCPU:
					managesCPU = true
				case corev1.ResourceMemory:
					managesMem = true
				}
			}
		}
		min := 1
		if h.Spec.MinReplicas != nil {
			min = int(*h.Spec.MinReplicas)
		}
		idx[h.Namespace+"/"+ref.Kind+"/"+ref.Name] = Autoscaler{
			Kind: "HPA", Name: h.Name, ManagesCPU: managesCPU, ManagesMem: managesMem, Min: min, Max: int(h.Spec.MaxReplicas),
		}
	}
	return idx
}

// knativeOf reads the ksvc/revision off a workload's labels (Knative stamps them
// on the per-revision Deployment). Empty Service => not Knative.
func knativeOf(lbls map[string]string) KnativeRef {
	return KnativeRef{Service: lbls[lblKnativeService], Revision: lbls[lblKnativeRevision]}
}

// ListWorkloads returns Deployments/StatefulSets/DaemonSets plus unmanaged
// (owner-less) Pods in scope, each tagged with PDB coverage, autoscaler
// coexistence (HPA/KPA), and Knative ownership.
func (c *Client) ListWorkloads(ctx context.Context) ([]Workload, error) {
	var out []Workload
	cls := c.Classifier()
	pdbs := c.loadPDBs(ctx)
	hpas := c.loadHPAs(ctx)
	withMeta := func(w Workload, tmplLabels map[string]string) Workload {
		w.PDBName, w.PDBProtected, w.PDBAllowed = pdbs.match(w.Namespace, tmplLabels)
		// Knative: the per-revision Deployment carries the service/revision labels.
		// A ksvc workload sets Knative.Service explicitly — preserve it.
		if w.Knative.Service == "" {
			w.Knative = knativeOf(w.Labels)
		}
		if w.Knative.Service == "" {
			w.Knative = knativeOf(tmplLabels)
		}
		// Autoscaler: an explicit HPA on the workload, else KPA for Knative.
		if a, ok := hpas[w.Namespace+"/"+w.Kind+"/"+w.Name]; ok {
			w.Autoscaler = a
		} else if w.Knative.Service != "" {
			// KPA scales on concurrency/RPS, not resource utilization → it does NOT
			// conflict with vertical request changes (ManagesCPU stays false).
			w.Autoscaler = Autoscaler{Kind: "KPA", Name: w.Knative.Revision}
		}
		return w
	}

	deps, err := c.cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		if c.exclude[d.Namespace] {
			continue
		}
		// Skip grouped (e.g. Knative per-revision) Deployments — the group unit
		// represents them (emitted below). Avoids one row per revision.
		if _, _, ok := c.groupValueOf(d.Namespace, d.Labels); ok {
			continue
		}
		replicas := 1
		if d.Spec.Replicas != nil {
			replicas = int(*d.Spec.Replicas)
		}
		out = append(out, withMeta(Workload{
			Namespace: d.Namespace, Kind: "Deployment", Name: d.Name, Replicas: replicas,
			Labels: d.Labels, Annotations: d.Annotations, Containers: cls.TemplateContainers(&d.Spec.Template.Spec, d.Spec.Template.Annotations, d.Spec.Template.Labels),
		}, d.Spec.Template.Labels))
	}

	sts, err := c.cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range sts.Items {
		s := &sts.Items[i]
		if c.exclude[s.Namespace] {
			continue
		}
		// Grouped (TailorGroup) StatefulSets are folded into the group unit, like
		// Deployments: their pods are attributed to the group (Snapshot/Ownership),
		// so a row of their own would be a duplicate without usage.
		if _, _, ok := c.groupValueOf(s.Namespace, s.Labels); ok {
			continue
		}
		replicas := 1
		if s.Spec.Replicas != nil {
			replicas = int(*s.Spec.Replicas)
		}
		out = append(out, withMeta(Workload{
			Namespace: s.Namespace, Kind: "StatefulSet", Name: s.Name, Replicas: replicas,
			Labels: s.Labels, Annotations: s.Annotations, Containers: cls.TemplateContainers(&s.Spec.Template.Spec, s.Spec.Template.Annotations, s.Spec.Template.Labels),
		}, s.Spec.Template.Labels))
	}

	ds, err := c.cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range ds.Items {
		d := &ds.Items[i]
		if c.exclude[d.Namespace] {
			continue
		}
		if _, _, ok := c.groupValueOf(d.Namespace, d.Labels); ok {
			continue // folded into its group unit (see StatefulSets above)
		}
		replicas := int(d.Status.DesiredNumberScheduled)
		if replicas == 0 {
			replicas = 1
		}
		out = append(out, withMeta(Workload{
			Namespace: d.Namespace, Kind: "DaemonSet", Name: d.Name, Replicas: replicas,
			Labels: d.Labels, Annotations: d.Annotations, Containers: cls.TemplateContainers(&d.Spec.Template.Spec, d.Spec.Template.Annotations, d.Spec.Template.Labels),
		}, d.Spec.Template.Labels))
	}

	// Pods: two cases beyond the standard controllers above.
	//   1. Grouped ephemeral pods owned by a CUSTOM controller (e.g. an
	//      agent-sandbox Sandbox) — a TailorGroup collapses every member into ONE
	//      synthetic unit (kind "group"). Individual pods churn; the group persists,
	//      so it accumulates the usage history and its rec sizes each NEW pod at
	//      admission (webhook). Knative pods (kind "ksvc") are skipped here — the
	//      ksvc resource listing below owns them.
	//   2. Truly unmanaged pods: no controller owns them. A webhook can't help
	//      (nothing recreates them) and they're lost on delete, so the only safe
	//      actuation is in-place resize.
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	stalePods := c.staleCohortPods(ctx, pods.Items)
	groupUnits := map[string]*Workload{} // ns/gv -> synthetic group unit
	for i := range pods.Items {
		p := &pods.Items[i]
		if c.exclude[p.Namespace] {
			continue
		}
		if stalePods[p.Namespace+"/"+p.Name] {
			continue // old cohort — represented by the active cohort's unit
		}
		if gk, gv, ok := c.matchGroupKey(p.Namespace, p.Labels); ok {
			kind := gk.Kind
			if kind == "ksvc" {
				continue // Knative — emitted from the ksvc resource listing below
			}
			key := p.Namespace + "/" + gv
			u, seen := groupUnits[key]
			if !seen {
				u = &Workload{
					Namespace: p.Namespace, Kind: kind, Name: gv, Replicas: 0,
					Labels: p.Labels, Annotations: p.Annotations,
					Containers:  sizable(cls.PodContainers(p)),
					GroupSource: gk.Source,
				}
				groupUnits[key] = u
			}
			u.Replicas++
			continue
		}
		if len(p.OwnerReferences) > 0 {
			continue // owned by a standard controller (emitted above)
		}
		out = append(out, withMeta(Workload{
			Namespace: p.Namespace, Kind: "Pod", Name: p.Name, Replicas: 1,
			Labels: p.Labels, Annotations: p.Annotations, Containers: sizable(cls.PodContainers(p)),
		}, p.Labels))
	}
	for _, u := range groupUnits {
		// Route group units through withMeta too, so a PDB/HPA that selects the pooled
		// pods by label is honored (the disruptive-mem guardrail can then fire).
		out = append(out, withMeta(*u, u.Labels))
	}

	// Knative ksvcs: one logical workload per Service (kind "ksvc"). Its container
	// template is the sizing target; the operator mints a new revision on patch.
	// Pods of every revision roll up here via the Snapshot ownership remap.
	if ksvcs, err := c.dyn.Resource(ksvcGVR).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range ksvcs.Items {
			k := &ksvcs.Items[i]
			ns := k.GetNamespace()
			if c.exclude[ns] {
				continue
			}
			cs, found, _ := unstructured.NestedSlice(k.Object, "spec", "template", "spec", "containers")
			if !found {
				continue
			}
			out = append(out, withMeta(Workload{
				Namespace: ns, Kind: "ksvc", Name: k.GetName(), Replicas: 1,
				Labels: k.GetLabels(), Annotations: k.GetAnnotations(),
				Containers: ksvcContainers(cs, cls),
				Knative:    KnativeRef{Service: k.GetName()},
			}, nil))
		}
	}
	return out, nil
}

func parseCPU(s string) float64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return float64(q.MilliValue()) / 1000
}

func parseMem(s string) float64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return float64(q.Value())
}

// ksvcContainers reads container name + requests/limits from a ksvc template's
// (unstructured) container list. Requests default to zero when unset.
func ksvcContainers(cs []any, cls *Classifier) []Container {
	out := make([]Container, 0, len(cs))
	for _, ci := range cs {
		cm, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		name, _ := cm["name"].(string)
		if name == "" && len(cs) == 1 {
			name = KnativeUserContainer // what Knative names it in the revision
		}
		if name == "" {
			continue
		}
		ct := Container{Name: name, InTemplate: true}
		if req, ok, _ := unstructured.NestedStringMap(cm, "resources", "requests"); ok {
			ct.CPURequest = parseCPU(req["cpu"])
			ct.MemRequest = parseMem(req["memory"])
		}
		// Limits too: the limit floor, health cap and "never introduce a limit"
		// rules all depend on the current limit.
		if lim, ok, _ := unstructured.NestedStringMap(cm, "resources", "limits"); ok {
			ct.CPULimit = parseCPU(lim["cpu"])
			ct.MemLimit = parseMem(lim["memory"])
		}
		cls.classify(&ct, false, false, nil, nil)
		out = append(out, ct)
	}
	return out
}

// Owner identifies the workload that owns a pod.
type Owner struct {
	Kind string
	Name string
}

// Ownership maps podName -> owning workload within a namespace.
func (c *Client) Ownership(ctx context.Context, namespace string) (map[string]Owner, error) {
	pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rss, err := c.cs.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rsToDeploy := map[string]string{}
	for i := range rss.Items {
		rs := &rss.Items[i]
		for _, o := range rs.OwnerReferences {
			if o.Kind == "Deployment" {
				rsToDeploy[rs.Name] = o.Name
			}
		}
	}
	stale := c.staleCohortPods(ctx, pods.Items)
	out := map[string]Owner{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if c.exclude[p.Namespace] {
			continue // excluded namespace: never attributed (matches ListWorkloads/Snapshot)
		}
		if stale[p.Namespace+"/"+p.Name] {
			continue // old cohort — excluded from the group's usage (matches Snapshot)
		}
		// Collapse grouped (e.g. Knative revision or sandbox) pods to their unit —
		// same remap as Snapshot, so the detail chart's usage query matches the
		// group workload.
		if kind, gv, ok := c.groupValueOf(p.Namespace, p.Labels); ok {
			out[p.Name] = Owner{Kind: kind, Name: gv}
			continue
		}
		if len(p.OwnerReferences) == 0 {
			// Unmanaged pod — it is its own "workload".
			out[p.Name] = Owner{Kind: "Pod", Name: p.Name}
			continue
		}
		o := p.OwnerReferences[0]
		switch o.Kind {
		case "ReplicaSet":
			if dep, ok := rsToDeploy[o.Name]; ok {
				out[p.Name] = Owner{Kind: "Deployment", Name: dep}
			}
		case "StatefulSet":
			out[p.Name] = Owner{Kind: "StatefulSet", Name: o.Name}
		case "DaemonSet":
			out[p.Name] = Owner{Kind: "DaemonSet", Name: o.Name}
		}
	}
	return out, nil
}

func resolveOwner(p *corev1.Pod, rsToDeploy map[string]string) (Owner, bool) {
	if len(p.OwnerReferences) == 0 {
		return Owner{Kind: "Pod", Name: p.Name}, true // unmanaged pod is its own workload
	}
	o := p.OwnerReferences[0]
	switch o.Kind {
	case "ReplicaSet":
		if dep, ok := rsToDeploy[p.Namespace+"/"+o.Name]; ok {
			return Owner{Kind: "Deployment", Name: dep}, true
		}
	case "StatefulSet":
		return Owner{Kind: "StatefulSet", Name: o.Name}, true
	case "DaemonSet":
		return Owner{Kind: "DaemonSet", Name: o.Name}, true
	}
	return Owner{}, false
}

func oomTerminated(t *corev1.ContainerStateTerminated) bool {
	// Only the explicit OOMKilled reason — exit 137 alone is any SIGKILL.
	return t != nil && t.Reason == "OOMKilled"
}

// ClusterSnapshot is a single cluster-wide read of pods + replicasets, indexed so
// per-namespace loops never each hit the API server. One pod list + one RS list
// replaces O(namespaces) lists — the key scale lever for large fleets (clusters with hundreds of
// namespaces, where per-namespace listing would hammer the API server).
type ClusterSnapshot struct {
	Ownership map[string]Owner          // "ns/pod" -> owning workload
	Running   map[string][]RunningPod   // namespace -> running pods (with requests)
	OOM       map[string]OOMObservation // "ns/kind/workload/container" -> recent OOMKill
	// NotRunning counts, per workload ("ns/kind/name"), its live pods that are
	// neither Running nor terminal (Pending, Unknown — unscheduled, pulling, in
	// init). They serve nothing; a disruptive rolling must not restart a Ready
	// replica while one exists (Running holds only Running pods).
	NotRunning map[string]int
	// Stale: live pods left out of the snapshot as an old generation of a
	// latest-cohort group ("ns/pod"). Their usage must stay out of the unit too.
	Stale map[string]bool
	// Admitted lists live pods the webhook sized at admission
	// (AnnAdmissionSized), in any phase, for the reconciler to record.
	Admitted []AdmittedPod

	// Name-based attribution of pods that no longer exist (see OwnerOf):
	// rsToDeploy is every listed ReplicaSet ("ns/rs" -> Deployment), with or
	// without pods (Deployments keep revisionHistoryLimit old ReplicaSets);
	// daemonSets is every DaemonSet ("ns/name") owning a live pod.
	rsToDeploy map[string]string
	daemonSets map[string]bool
}

// generatedSuffix reports whether s is the 5-character random suffix the API
// server appends for metadata.generateName (k8s.io/apimachinery
// util/rand.String: lowercase consonants and digits without vowels, 0, 1, 3).
func generatedSuffix(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("bcdfghjklmnpqrstvwxz2456789", r) {
			return false
		}
	}
	return true
}

// OwnerOf attributes a pod name seen in the metrics to its workload. A live pod
// resolves through Ownership. A pod that no longer exists (deleted, evicted,
// replaced by a rollout) still has its usage in the metrics backend under its
// old name: dropping it would throw the workload's history away on every pod
// replacement, so a fresh pod (or a restarted engine looking at fresh pods)
// would sit unconfident for MinSamples x 5 min with the data right there.
// Such a pod is attributed by its generated name only when that name is
// unambiguous: "<ReplicaSet>-<suffix>" for a ReplicaSet that still exists and is
// owned by a Deployment, or "<DaemonSet>-<suffix>" for a DaemonSet that owns a
// live pod in the namespace. StatefulSet pods keep their names (they are live
// again once recreated); bare pods and grouped units (ksvc/group, attributed by
// labels) are never guessed.
func (s *ClusterSnapshot) OwnerOf(ns, pod string) (Owner, bool) {
	if o, ok := s.Ownership[ns+"/"+pod]; ok {
		return o, true
	}
	i := strings.LastIndexByte(pod, '-')
	if i <= 0 || !generatedSuffix(pod[i+1:]) {
		return Owner{}, false
	}
	prefix := pod[:i]
	if dep, ok := s.rsToDeploy[ns+"/"+prefix]; ok {
		return Owner{Kind: "Deployment", Name: dep}, true
	}
	if s.daemonSets[ns+"/"+prefix] {
		return Owner{Kind: "DaemonSet", Name: prefix}, true
	}
	return Owner{}, false
}

// WorkloadKey is the NotRunning key of a workload.
func WorkloadKey(ns string, o Owner) string { return ns + "/" + o.Kind + "/" + o.Name }

// OOMObservation records a single OOMKill of a workload container: the memory
// value that proved insufficient (the container's memory LIMIT at the time; for
// a container without a limit, only a lower bound — its request) and when the
// kill happened. The recommender folds these into a durable per-workload floor;
// it raises an existing limit, and never introduces one on an unbounded container.
type OOMObservation struct {
	MemBytes float64
	At       time.Time
}

func (c *Client) Snapshot(ctx context.Context) (*ClusterSnapshot, error) {
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rss, err := c.cs.AppsV1().ReplicaSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rsToDeploy := map[string]string{}
	for i := range rss.Items {
		rs := &rss.Items[i]
		for _, o := range rs.OwnerReferences {
			if o.Kind == "Deployment" {
				rsToDeploy[rs.Namespace+"/"+rs.Name] = o.Name
			}
		}
	}
	stale := c.staleCohortPods(ctx, pods.Items)
	cls := c.Classifier()
	snap := &ClusterSnapshot{Ownership: map[string]Owner{}, Running: map[string][]RunningPod{}, OOM: map[string]OOMObservation{}, NotRunning: map[string]int{},
		rsToDeploy: rsToDeploy, daemonSets: map[string]bool{}, Stale: stale}
	for i := range pods.Items {
		p := &pods.Items[i]
		if c.exclude[p.Namespace] {
			continue
		}
		// Non-active cohort (old generation) of a latest-cohort group → its idle
		// usage must not dilute the group; drop it entirely from the snapshot.
		if stale[p.Namespace+"/"+p.Name] {
			continue
		}
		owner, ok := resolveOwner(p, rsToDeploy)
		// Collapse grouped workloads to ONE logical unit (kind "ksvc" for Knative,
		// "group" for a TailorGroup like agent-sandbox; name = the group value).
		// Every member's pods roll up here, so usage aggregates and actuation targets
		// the group — a site with dozens of daily revisions, or a churning pool of
		// sandbox pods, is one row. Group collapse takes precedence and stands in even
		// when the pod's own controller is a CUSTOM kind resolveOwner can't map (the
		// Sandbox CR), which is exactly how ephemeral sandbox pods get observed.
		if kind, gv, gok := c.groupValueOf(p.Namespace, p.Labels); gok {
			owner = Owner{Kind: kind, Name: gv}
			ok = true
		}
		if !ok {
			continue
		}
		snap.Ownership[p.Namespace+"/"+p.Name] = owner
		if owner.Kind == "DaemonSet" {
			snap.daemonSets[p.Namespace+"/"+owner.Name] = true
		}
		if v, has := p.Annotations[AnnAdmissionSized]; has {
			if d, ok := ParseAdmissionSized(v); ok {
				snap.Admitted = append(snap.Admitted, AdmittedPod{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID),
					Owner: owner, Created: p.CreationTimestamp.Time, Decisions: d, Containers: cls.PodContainers(p)})
			}
		}
		switch p.Status.Phase {
		case corev1.PodRunning:
			snap.Running[p.Namespace] = append(snap.Running[p.Namespace], runningPodOf(p, owner, cls))
		case corev1.PodSucceeded, corev1.PodFailed:
			// terminal: not a replica any more
		default: // Pending, Unknown, unset
			if p.DeletionTimestamp == nil { // a deleting pod's replacement is counted instead
				snap.NotRunning[WorkloadKey(p.Namespace, owner)]++
			}
		}
		if len(p.Status.ContainerStatuses) > 0 || len(p.Status.InitContainerStatuses) > 0 {
			// Spec mem limit/request per container — the value the container hit when
			// it OOMKilled is what we protect against next time. Native sidecars
			// (initContainers, restartPolicy Always) report in initContainerStatuses.
			specMem := map[string][2]float64{}
			for _, sc := range cls.PodContainers(p) {
				if sc.Class.Resizable() {
					specMem[sc.Name] = [2]float64{sc.MemLimit, sc.MemRequest}
				}
			}
			statuses := append(append([]corev1.ContainerStatus{}, p.Status.ContainerStatuses...), p.Status.InitContainerStatuses...)
			for _, cs := range statuses {
				if _, sized := specMem[cs.Name]; !sized {
					continue // regular init container: never sized, nothing to learn
				}
				// Prefer the last-termination OOM (survives one restart) over a
				// currently-terminated one; either counts.
				term := cs.LastTerminationState.Terminated
				if !oomTerminated(term) {
					term = cs.State.Terminated
				}
				if !oomTerminated(term) {
					continue
				}
				m := specMem[cs.Name]
				val := m[0] // the limit that was hit
				if val <= 0 {
					// Unbounded container (node-pressure OOM): there is no limit that
					// "proved insufficient". Its request is only a lower bound — the
					// recommender reserves max(this × growth, observed peak × growth)
					// via the REQUEST (learned once per kill, capped) and never derives a
					// limit from this value.
					val = m[1]
				}
				if val <= 0 {
					val = 1 // OOMKilled with neither request nor limit: still a signal
				}
				at := term.FinishedAt.Time
				if at.IsZero() {
					at = time.Now()
				}
				key := p.Namespace + "/" + owner.Kind + "/" + owner.Name + "/" + cs.Name
				// A workload's pods can disagree — keep the largest value and latest time.
				if prev, ok := snap.OOM[key]; ok {
					if prev.MemBytes > val {
						val = prev.MemBytes
					}
					if prev.At.After(at) {
						at = prev.At
					}
				}
				snap.OOM[key] = OOMObservation{MemBytes: val, At: at}
			}
		}
	}
	return snap, nil
}

// RunningPod is a live pod with its current per-container requests.
type RunningPod struct {
	Name       string
	UID        string
	Owner      Owner
	Containers []Container
	// LastApplied is the durable per-container actuation timestamp read back from
	// the pod's AnnLastApplied annotation (container -> time). It lets a restarted
	// (or newly elected) engine rebuild the anti-thrash cooldown instead of
	// starting from a clean slate.
	LastApplied map[string]time.Time
	// QOSClass is the pod's (immutable) QoS class. An in-place resize may not
	// change it, so a Guaranteed pod can't take requests != limits.
	QOSClass corev1.PodQOSClass
	// ResizeState is non-empty while a previous in-place resize hasn't landed:
	// "Pending/Deferred", "Pending/Infeasible", "InProgress", or the legacy
	// status.resize value (Proposed/InProgress/Deferred/Infeasible, <=1.32).
	ResizeState string
	// PodResources is the pod-level spec.resources (PodLevelResources) when set:
	// the sum of container requests must stay <= its requests and every container
	// limit <= its limits, so an in-place resize has to respect it.
	PodResources *Container
	// Ready mirrors the pod's Ready condition. A disruptive (container-restarting)
	// resize only advances to the next replica once every replica is Ready again.
	Ready bool
	// Labels are the pod's labels: a PodDisruptionBudget selects pods, so the
	// live budget check before a disruptive restart matches on them.
	Labels map[string]string
	// ResizeFailed (AnnResizeFailed) and Rolling (AnnRolling, same format as
	// LastApplied): persisted reconciler state, see state.go.
	ResizeFailed map[string]ResizeFailure
	Rolling      map[string]time.Time
}

// runningPodOf projects a pod into a RunningPod, including the resize/QoS state
// the reconciler needs to avoid stacking or impossible resizes.
func runningPodOf(p *corev1.Pod, owner Owner, cls *Classifier) RunningPod {
	rp := RunningPod{
		Name: p.Name, UID: string(p.UID), Owner: owner, Containers: liveContainersOf(p, cls),
		QOSClass: p.Status.QOSClass, ResizeState: resizeState(p),
		LastApplied:  ParseLastApplied(p.Annotations[AnnLastApplied]),
		Labels:       p.Labels,
		ResizeFailed: ParseResizeFailed(p.Annotations[AnnResizeFailed]),
		Rolling:      ParseLastApplied(p.Annotations[AnnRolling]),
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			rp.Ready = c.Status == corev1.ConditionTrue
		}
	}
	if p.Spec.Resources != nil {
		pr := containersOf([]corev1.Container{{Name: "", Resources: *p.Spec.Resources}})[0]
		rp.PodResources = &pr
	}
	return rp
}

// liveContainersOf returns the pod's containers with the resources the kubelet
// has actually EFFECTED (status.containerStatuses[].resources, k8s >=1.33 with
// in-place resize) when reported, falling back to the spec otherwise. With a
// resize Pending/Infeasible the spec already carries the DESIRED values, so
// reading the spec would report a resize as applied when it is not. The spec
// requests are kept in SpecCPURequest/SpecMemRequest.
//
// Every container of the pod is returned — init (Class init) and native
// sidecars included — so pod-wide checks (QoS) see the whole pod; callers that
// actuate must filter on Class.Resizable().
func liveContainersOf(p *corev1.Pod, cls *Classifier) []Container {
	out := cls.PodContainers(p)
	status := map[string]*corev1.ResourceRequirements{}
	for _, list := range [][]corev1.ContainerStatus{p.Status.ContainerStatuses, p.Status.InitContainerStatuses} {
		for i := range list {
			if list[i].Resources != nil {
				status[list[i].Name] = list[i].Resources
			}
		}
	}
	for i := range out {
		c := &out[i]
		c.SpecCPURequest, c.SpecMemRequest = c.CPURequest, c.MemRequest
		r, ok := status[c.Name]
		if !ok {
			continue
		}
		if q, ok := r.Requests[corev1.ResourceCPU]; ok {
			c.CPURequest = q.AsApproximateFloat64()
		}
		if q, ok := r.Requests[corev1.ResourceMemory]; ok {
			c.MemRequest = q.AsApproximateFloat64()
		}
		if q, ok := r.Limits[corev1.ResourceCPU]; ok {
			c.CPULimit = q.AsApproximateFloat64()
		}
		if q, ok := r.Limits[corev1.ResourceMemory]; ok {
			c.MemLimit = q.AsApproximateFloat64()
		}
	}
	return out
}

// AnnLastApplied is the durable record of the last actuation per container, as
// a JSON object {"<container>": "<RFC3339>"}. Written on the pod after an
// in-place resize and on the pod TEMPLATE on a rollout (so the new pods carry
// it). Read back on every snapshot to rebuild the reconciler cooldown.
const AnnLastApplied = "tailor.k8s-tailor.dev/last-applied"

// AnnAdmissionSized marks a pod the mutating webhook sized at CREATE:
// {"<container>": {"fromCpu":…, "toCpu":…, "fromMem":…, "toMem":…}} (cores,
// bytes). The webhook records nothing itself (a forged AdmissionReview would
// otherwise write the decision log): the leader's reconciler records the
// decision when it observes the pod in the API server (Snapshot.Admitted).
const AnnAdmissionSized = "tailor.k8s-tailor.dev/admission-sized"

// AdmissionDecision is one container's admission-time sizing.
type AdmissionDecision struct {
	FromCPU float64 `json:"fromCpu"`
	ToCPU   float64 `json:"toCpu"`
	FromMem float64 `json:"fromMem"`
	ToMem   float64 `json:"toMem"`
}

// ParseAdmissionSized decodes AnnAdmissionSized; ok is false when malformed.
func ParseAdmissionSized(v string) (map[string]AdmissionDecision, bool) {
	var m map[string]AdmissionDecision
	if err := json.Unmarshal([]byte(v), &m); err != nil || len(m) == 0 {
		return nil, false
	}
	return m, true
}

// AdmittedPod is a live pod carrying AnnAdmissionSized, with its spec
// containers (the requests the pod was actually created with).
type AdmittedPod struct {
	Namespace, Name, UID string
	Owner                Owner
	Created              time.Time
	Decisions            map[string]AdmissionDecision
	Containers           []Container
}

// AnnOOMFloor persists the learned OOM memory floors of a workload's containers
// on the workload object (metadata — never the pod template, so writing it does
// not roll anything): {"<container>": {"mem": <bytes>, "at": "<RFC3339>"}}.
// The recommender rebuilds its floors from it after a restart / leader failover,
// when the OOMKilled pod (and its lastState) is usually gone.
const AnnOOMFloor = "tailor.k8s-tailor.dev/oom-floor"

// OOMFloor is one persisted floor.
type OOMFloor struct {
	Mem float64   `json:"mem"`
	At  time.Time `json:"at"`
}

// ParseOOMFloors decodes an AnnOOMFloor value (malformed → empty).
func ParseOOMFloors(v string) map[string]OOMFloor {
	out := map[string]OOMFloor{}
	if v == "" {
		return out
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return map[string]OOMFloor{}
	}
	return out
}

// FormatOOMFloors encodes floors as an AnnOOMFloor value.
func FormatOOMFloors(m map[string]OOMFloor) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// ParseLastApplied decodes an AnnLastApplied value. Malformed input (or a
// malformed entry) is ignored — the annotation is advisory.
func ParseLastApplied(v string) map[string]time.Time {
	out := map[string]time.Time{}
	if v == "" {
		return out
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return out
	}
	for c, ts := range raw {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			out[c] = t
		}
	}
	return out
}

// FormatLastApplied encodes a container -> time map as an AnnLastApplied value.
func FormatLastApplied(m map[string]time.Time) string {
	raw := make(map[string]string, len(m))
	for c, t := range m {
		raw[c] = t.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(raw)
	return string(b)
}

// resizeState reports an outstanding in-place resize. k8s >=1.33 exposes it as
// pod conditions (PodResizePending with reason Deferred|Infeasible,
// PodResizeInProgress); <=1.32 as the deprecated status.resize field.
func resizeState(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch string(c.Type) {
		case "PodResizePending":
			if c.Reason != "" {
				return "Pending/" + c.Reason
			}
			return "Pending"
		case "PodResizeInProgress":
			return "InProgress"
		}
	}
	return string(p.Status.Resize)
}

// ListRunningPods returns running pods in a namespace with resolved owner + requests.
func (c *Client) ListRunningPods(ctx context.Context, namespace string) ([]RunningPod, error) {
	pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	own, err := c.Ownership(ctx, namespace)
	if err != nil {
		return nil, err
	}
	var out []RunningPod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		o, ok := own[p.Name]
		if !ok {
			continue
		}
		out = append(out, runningPodOf(p, o, c.Classifier()))
	}
	return out, nil
}

// In-place support cache: a definite answer is re-checked every inPlaceTTL (an
// upgrade/downgrade of the control plane is picked up); a discovery error is
// retried after inPlaceRetry.
const (
	inPlaceTTL   = 10 * time.Minute
	inPlaceRetry = 30 * time.Second
)

func (c *Client) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// InPlaceSupport reports whether the cluster can resize running pods in place —
// the `pods/resize` subresource exists (InPlacePodVerticalScaling, GA in 1.33+).
// known=false means discovery failed (transient API error): the caller must
// HOLD, not fall back to a disruptive rollout.
func (c *Client) InPlaceSupport(ctx context.Context) (supported, known bool) {
	c.inPlaceMu.Lock()
	defer c.inPlaceMu.Unlock()
	now := c.now()
	wait := inPlaceRetry
	if c.inPlaceKnown {
		wait = inPlaceTTL
	}
	if !c.inPlaceAt.IsZero() && now.Sub(c.inPlaceAt) < wait {
		return c.inPlaceOK, c.inPlaceKnown
	}
	c.inPlaceAt = now
	rl, err := c.cs.Discovery().ServerResourcesForGroupVersion("v1")
	if err != nil {
		// Keep a previous definite answer through a transient error.
		return c.inPlaceOK, c.inPlaceKnown
	}
	c.inPlaceOK, c.inPlaceKnown = false, true
	for _, r := range rl.APIResources {
		if r.Name == "pods/resize" {
			c.inPlaceOK = true
			break
		}
	}
	return c.inPlaceOK, c.inPlaceKnown
}

// InPlaceSupported is InPlaceSupport collapsed to a bool (false while unknown).
func (c *Client) InPlaceSupported(ctx context.Context) bool {
	ok, _ := c.InPlaceSupport(ctx)
	return ok
}

// RecordEvent emits a Normal Kubernetes Event on the workload object, so every
// applied change shows up in `kubectl get events` — the idiomatic, auditable
// trail (like HPA/VPA). Best-effort; failures never block actuation.
func (c *Client) RecordEvent(ctx context.Context, kind, namespace, name, reason, message string) {
	c.RecordEventFor(ctx, EventRef{Kind: kind, Namespace: namespace, Name: name}, corev1.EventTypeNormal, reason, message)
}

// EventRef identifies the object an Event is about. UID is optional but makes
// the event show in `kubectl describe` (which filters on involvedObject.uid).
type EventRef struct {
	Kind, Namespace, Name, UID string
}

// RecordEventFor emits an Event of the given type (Normal|Warning) on ref.
func (c *Client) RecordEventFor(ctx context.Context, ref EventRef, evType, reason, message string) {
	if ref.Kind == "ksvc" { // the logical Knative unit IS the Knative Service
		ref.Kind = "Service"
	}
	api := map[string]string{
		"Deployment": "apps/v1", "StatefulSet": "apps/v1", "DaemonSet": "apps/v1",
		"Pod": "v1", "Service": "serving.knative.dev/v1",
	}[ref.Kind]
	if api == "" {
		// No real object behind the kind (a TailorGroup unit is synthetic): an
		// Event would point at a nonexistent apps/v1 object. Skip it — the pod
		// carries its own Event.
		return
	}
	message = truncateMessage(message, 1024) // the API server rejects messages over 1KiB
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "k8s-tailor.", Namespace: ref.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name,
			APIVersion: api, UID: types.UID(ref.UID)},
		Reason: reason, Message: message, Type: evType,
		Source:         corev1.EventSource{Component: "k8s-tailor"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1, Action: reason,
		ReportingController: "k8s-tailor.dev/engine", ReportingInstance: hostname(),
	}
	_, _ = c.cs.CoreV1().Events(ref.Namespace).Create(ctx, ev, metav1.CreateOptions{})
}

// truncateMessage caps msg at max bytes, ending in "...", cutting on a rune
// boundary so a multi-byte UTF-8 character is never split.
func truncateMessage(msg string, max int) string {
	if len(msg) <= max {
		return msg
	}
	cut := max - 3
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "..."
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// Clientset exposes the typed client (leader election needs it).
func (c *Client) Clientset() kubernetes.Interface { return c.cs }

// OOMKilled reports which workload containers have been OOMKilled recently
// (current or last termination), keyed by "kind/workload/container". The
// recommender uses this to grow memory so the container stays healthy — the
// whole point of a tailor is to never make a workload worse.
func (c *Client) OOMKilled(ctx context.Context, namespace string) (map[string]bool, error) {
	pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	own, err := c.Ownership(ctx, namespace)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	// Only the explicit OOMKilled reason — exit code 137 alone is any SIGKILL
	// (node restart, eviction, kubectl delete), not necessarily out-of-memory.
	oom := func(t *corev1.ContainerStateTerminated) bool {
		return t != nil && t.Reason == "OOMKilled"
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		o, ok := own[p.Name]
		if !ok {
			continue
		}
		// Native sidecars (initContainers with restartPolicy Always) report in
		// initContainerStatuses; a regular init container is never sized.
		native := map[string]bool{}
		for _, ic := range p.Spec.InitContainers {
			if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
				native[ic.Name] = true
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			if oom(cs.State.Terminated) || oom(cs.LastTerminationState.Terminated) {
				out[o.Kind+"/"+o.Name+"/"+cs.Name] = true
			}
		}
		for _, cs := range p.Status.InitContainerStatuses {
			if native[cs.Name] && (oom(cs.State.Terminated) || oom(cs.LastTerminationState.Terminated)) {
				out[o.Kind+"/"+o.Name+"/"+cs.Name] = true
			}
		}
	}
	return out, nil
}

// ResizePodContainer patches a running pod's container requests (and limits, when
// given) in place (Kubernetes >=1.33 `resize` subresource) — no restart, no
// eviction. Limits MUST be patched alongside a request that grows: otherwise a
// request raised above the old limit is rejected (requests > limits). Empty
// cpuLimit/memLimit leaves that limit untouched.
//
// native targets spec.initContainers (a native sidecar, restartPolicy Always —
// resizable in place per the Kubernetes docs); otherwise spec.containers.
func (c *Client) ResizePodContainer(ctx context.Context, namespace, pod, container string, native bool, cpu, mem, cpuLimit, memLimit string) error {
	resources := patchResources(cpu, mem, cpuLimit, memLimit)
	body := map[string]any{"spec": map[string]any{containerListKey(native): []any{
		map[string]any{"name": container, "resources": resources},
	}}}
	patch, _ := json.Marshal(body)
	_, err := c.cs.CoreV1().Pods(namespace).Patch(
		ctx, pod, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize",
	)
	return err
}

// PatchWorkloadResources updates one container's requests (and optional limits)
// in the controller's POD TEMPLATE. This triggers the controller's NATIVE rollout
// — which already honors the Deployment maxUnavailable/maxSurge, StatefulSet
// ordering, DaemonSet updateStrategy and any PodDisruptionBudget — so we don't
// orchestrate eviction ourselves. Used for Apply=Rollout / the fall-back path.
//
// podAnnotations (optional) are merged into the pod TEMPLATE's annotations in
// the same patch — used to stamp AnnLastApplied so the rolled pods carry it.
//
// The container MUST exist in the template (Container.InTemplate): a strategic
// merge on a missing name would append a bogus container. native targets
// spec.template.spec.initContainers (a native sidecar declared in the template).
func (c *Client) PatchWorkloadResources(ctx context.Context, kind, namespace, name, container string, native bool, cpu, mem, cpuLimit, memLimit string, podAnnotations map[string]string) error {
	resources := patchResources(cpu, mem, cpuLimit, memLimit)
	tmpl := map[string]any{"spec": map[string]any{
		containerListKey(native): []any{map[string]any{"name": container, "resources": resources}},
	}}
	if len(podAnnotations) > 0 {
		tmpl["metadata"] = map[string]any{"annotations": podAnnotations}
	}
	body := map[string]any{"spec": map[string]any{"template": tmpl}}
	patch, _ := json.Marshal(body)
	var err error
	switch kind {
	case "Deployment":
		_, err = c.cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = c.cs.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "DaemonSet":
		_, err = c.cs.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	default:
		return fmt.Errorf("rollout unsupported for kind %q", kind)
	}
	return err
}

// patchResources is the resources fragment of a STRATEGIC merge patch: requests
// always, limits only when given. Strategic merge merges these maps key by key,
// so other resources (GPUs, ephemeral-storage, an untouched limit) survive.
func patchResources(cpu, mem, cpuLimit, memLimit string) map[string]any {
	limits := map[string]any{}
	if cpuLimit != "" {
		limits["cpu"] = cpuLimit
	}
	if memLimit != "" {
		limits["memory"] = memLimit
	}
	resources := map[string]any{"requests": map[string]any{"cpu": cpu, "memory": mem}}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
	return resources
}

// containerListKey is the pod-spec list a container lives in.
func containerListKey(native bool) string {
	if native {
		return "initContainers"
	}
	return "containers"
}

// PatchWorkloadPodAnnotations merges annotations into a controller's POD
// TEMPLATE metadata (a nil value removes the key). This is how a sidecar that a
// mesh injects at admission (absent from the template) is sized durably: the
// injector reads its resource annotations (e.g. sidecar.istio.io/proxyCPU) from
// the pod and renders the proxy with them. Triggers the native rollout.
func (c *Client) PatchWorkloadPodAnnotations(ctx context.Context, kind, namespace, name string, anns map[string]*string) error {
	m := map[string]any{}
	for k, v := range anns {
		if v == nil {
			m[k] = nil
		} else {
			m[k] = *v
		}
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"metadata": map[string]any{"annotations": m},
	}}})
	var err error
	switch kind {
	case "Deployment":
		_, err = c.cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = c.cs.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "DaemonSet":
		_, err = c.cs.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		return fmt.Errorf("pod-template annotations unsupported for kind %q", kind)
	}
	return err
}

// KnativeUserContainer is the name Knative gives the (single) unnamed container
// of a ksvc template in its revision pods.
const KnativeUserContainer = "user-container"

// ErrNoTemplateContainer: the container to size does not exist in the ksvc pod
// template (e.g. queue-proxy, injected at runtime, or a renamed container).
// Nothing was patched — callers must not record the actuation as applied.
var ErrNoTemplateContainer = errors.New("container not in the ksvc template")

// ksvcTemplateIndex resolves container to its index in the ksvc template: the
// exact name, else the single UNNAMED container when container is the name
// Knative gives it in the revision (user-container). Never another container:
// patching e.g. the user container with queue-proxy's sizes would be wrong.
func ksvcTemplateIndex(containers []any, container string) int {
	for i, c := range containers {
		if cm, ok := c.(map[string]any); ok {
			if n, _ := cm["name"].(string); n == container {
				return i
			}
		}
	}
	if len(containers) == 1 && container == KnativeUserContainer {
		if cm, ok := containers[0].(map[string]any); ok {
			if n, _ := cm["name"].(string); n == "" {
				return 0
			}
		}
	}
	return -1
}

// PatchKnativeService sets a container's resources on the ksvc pod template. A
// Knative Revision's Deployment is IMMUTABLE — patching it is reverted — so the
// durable path edits the Service, which mints a new Revision (Knative then rolls
// it in with its own traffic/rollout semantics). get→modify→update on the
// unstructured ksvc; the container may be unnamed (Knative allows a single one).
func (c *Client) PatchKnativeService(ctx context.Context, namespace, name, container, cpu, mem, cpuLimit, memLimit string, podAnnotations map[string]string) error {
	res := c.dyn.Resource(ksvcGVR).Namespace(namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return fmt.Errorf("ksvc %s/%s: no containers in template", namespace, name)
	}
	// Only size a container that actually exists in the ksvc template. Knative
	// injects queue-proxy at runtime (not in the template), so its recommendation
	// has no target here — never fall through to patching the wrong container, and
	// never report success: the caller would record an actuation that changed nothing.
	idx := ksvcTemplateIndex(containers, container)
	if idx < 0 {
		return fmt.Errorf("ksvc %s/%s[%s]: %w", namespace, name, container, ErrNoTemplateContainer)
	}
	cm, _ := containers[idx].(map[string]any)
	if cm == nil {
		cm = map[string]any{}
	}
	// MERGE into the existing resources: only cpu/memory requests (and a limit
	// when one is passed) move. Every other key — existing limits, GPUs and other
	// extended resources, ephemeral-storage, claims — is preserved.
	resources, _ := cm["resources"].(map[string]any)
	if resources == nil {
		resources = map[string]any{}
	}
	requests, _ := resources["requests"].(map[string]any)
	if requests == nil {
		requests = map[string]any{}
	}
	requests["cpu"], requests["memory"] = cpu, mem
	resources["requests"] = requests
	if cpuLimit != "" || memLimit != "" {
		limits, _ := resources["limits"].(map[string]any)
		if limits == nil {
			limits = map[string]any{}
		}
		if cpuLimit != "" {
			limits["cpu"] = cpuLimit
		}
		if memLimit != "" {
			limits["memory"] = memLimit
		}
		resources["limits"] = limits
	}
	cm["resources"] = resources
	containers[idx] = cm
	if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return err
	}
	// Knative rejects a template change that keeps the same revision name
	// ("changes without a name change"). Drop the pinned name so the operator
	// mints the next revision automatically with the resized template.
	unstructured.RemoveNestedField(obj.Object, "spec", "template", "metadata", "name")
	if len(podAnnotations) > 0 {
		anns, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
		if anns == nil {
			anns = map[string]string{}
		}
		for k, v := range podAnnotations {
			anns[k] = v
		}
		if err := unstructured.SetNestedStringMap(obj.Object, anns, "spec", "template", "metadata", "annotations"); err != nil {
			return err
		}
	}
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// AnnotateWorkload merge-patches a workload's annotations. A nil value removes
// the key. Used by the per-row Policy dropdown + Automated toggle.
func (c *Client) AnnotateWorkload(ctx context.Context, kind, namespace, name string, anns map[string]*string) error {
	m := map[string]any{}
	for k, v := range anns {
		if v == nil {
			m[k] = nil
		} else {
			m[k] = *v
		}
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": m}})
	var err error
	switch kind {
	case "Deployment":
		_, err = c.cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = c.cs.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "DaemonSet":
		_, err = c.cs.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "Pod":
		_, err = c.cs.CoreV1().Pods(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		return fmt.Errorf("%w %q", ErrUnsupportedKind, kind)
	}
	return err
}

// ErrUnsupportedKind: AnnotateWorkload only writes Deployments, StatefulSets,
// DaemonSets and bare Pods (a ksvc or a TailorGroup unit has no such object).
var ErrUnsupportedKind = errors.New("unsupported kind")

// ResolvePodOwner resolves a single pod's workload at admission time, fetching
// the ReplicaSet to find its Deployment when needed.
func (c *Client) ResolvePodOwner(ctx context.Context, namespace string, labels map[string]string, owners []metav1.OwnerReference) (Owner, bool) {
	// Grouped/ephemeral pods (a Knative revision, or a sandbox class sharing a
	// label) resolve to their GROUP, so the webhook finds the group's learned
	// recommendation and sizes each new pod at creation — even when the pod's own
	// name/owner is unique per session (no per-pod history to key on).
	if kind, gv, ok := c.groupValueOf(namespace, labels); ok {
		return Owner{Kind: kind, Name: gv}, true
	}
	if len(owners) == 0 {
		return Owner{}, false
	}
	o := owners[0]
	switch o.Kind {
	case "ReplicaSet":
		rs, err := c.cs.AppsV1().ReplicaSets(namespace).Get(ctx, o.Name, metav1.GetOptions{})
		if err != nil {
			return Owner{}, false
		}
		for _, ro := range rs.OwnerReferences {
			if ro.Kind == "Deployment" {
				return Owner{Kind: "Deployment", Name: ro.Name}, true
			}
		}
	case "StatefulSet":
		return Owner{Kind: "StatefulSet", Name: o.Name}, true
	case "DaemonSet":
		return Owner{Kind: "DaemonSet", Name: o.Name}, true
	}
	return Owner{}, false
}

// ReadConfigMapData returns data[key] (binaryData or data) of a ConfigMap;
// found=false when the ConfigMap or the key does not exist.
func (c *Client) ReadConfigMapData(ctx context.Context, ns, name, key string) ([]byte, bool, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get ConfigMap %s/%s: %w", ns, name, err)
	}
	if b, ok := cm.BinaryData[key]; ok {
		return b, true, nil
	}
	if s, ok := cm.Data[key]; ok {
		return []byte(s), true, nil
	}
	return nil, false, nil
}

// WriteConfigMapData stores data under binaryData[key], creating the ConfigMap when
// absent. annotations are merged onto the object.
func (c *Client) WriteConfigMapData(ctx context.Context, ns, name, key string, data []byte, annotations map[string]string) error {
	cms := c.cs.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations,
				Labels: map[string]string{"app.kubernetes.io/name": "k8s-tailor"}},
			BinaryData: map[string][]byte{key: data},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create ConfigMap %s/%s: %w", ns, name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ConfigMap %s/%s: %w", ns, name, err)
	}
	if cm.BinaryData == nil {
		cm.BinaryData = map[string][]byte{}
	}
	cm.BinaryData[key] = data
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		cm.Annotations[k] = v
	}
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ConfigMap %s/%s: %w", ns, name, err)
	}
	return nil
}
