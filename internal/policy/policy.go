// Package policy loads TailorPolicy / TailorConfig CRs and resolves the
// effective profile for a workload. Config lives in etcd (CRDs) — no database.
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

type Mode string

const (
	ModeOff       Mode = "Off"
	ModeRecommend Mode = "Recommend"
	ModeWebhook   Mode = "Webhook"
	ModeInPlace   Mode = "InPlace"
	ModeBoth      Mode = "Both"
)

func (m Mode) Webhook() bool { return m == ModeWebhook || m == ModeBoth }
func (m Mode) InPlace() bool { return m == ModeInPlace || m == ModeBoth }

// Apply is the actuation METHOD (orthogonal to Mode, which decides whether to
// actuate at all). InPlace resizes running pods (no restart). Rollout patches the
// controller template and lets its native updateStrategy roll (respects PDB,
// maxUnavailable, StatefulSet ordering). InPlacePreferred tries in-place and
// falls back to a rollout only when the change needs a restart.
type Apply string

const (
	ApplyInPlace          Apply = "InPlace"
	ApplyRollout          Apply = "Rollout"
	ApplyInPlacePreferred Apply = "InPlacePreferred"
)

// Direction gates which way actuation may move a resource. Down = shrink-only
// (reclaim waste, never raise — the most conservative savings posture); Up =
// grow-only (only protect against under-provisioning, never reclaim); Both =
// unrestricted (default).
type Direction string

const (
	DirBoth Direction = "Both"
	DirDown Direction = "Down"
	DirUp   Direction = "Up"
)

// Gate clamps a per-resource target so it never moves against the direction:
// Down keeps the current value when the target would raise it; Up keeps it when
// the target would lower it. Both passes the target through unchanged.
func (d Direction) Gate(current, target float64) float64 {
	switch d {
	case DirDown:
		if target > current {
			return current
		}
	case DirUp:
		if target < current {
			return current
		}
	}
	return target
}

// Guaranteed says what the engine does with a container whose request equals
// its limit for CPU and memory (a Guaranteed pod). Skip (default) leaves it
// alone. ResizeBoth moves request and limit together so the pod stays
// Guaranteed: growth for CPU and memory, shrink for CPU only (a memory limit
// lowered in place is best effort in the kubelet, and request == limit near the
// peak turns a small error into an OOM, so memory never shrinks).
type Guaranteed string

const (
	GuaranteedSkip       Guaranteed = "skip"
	GuaranteedResizeBoth Guaranteed = "resizeBoth"
)

// ResizeBoth reports whether the opt-in is on.
func (g Guaranteed) ResizeBoth() bool { return g == GuaranteedResizeBoth }

// ResourceProfile: Min/Max in cores (cpu) or bytes (mem); 0 = unbounded.
type ResourceProfile struct {
	Quantile    float64
	Margin      float64
	Min         float64
	Max         float64
	LimitFactor float64
	// EnforceLimit (memory only, opt-in): derive a memory LIMIT for a container
	// that runs without one (request × limitFactor, never below the observed peak
	// × (1+margin)). Off by default: the engine never invents an OOM ceiling the
	// owner did not set.
	EnforceLimit bool
}

// Resolved is the effective policy for one workload.
type Resolved struct {
	Mode                  Mode
	Apply                 Apply
	CPU                   ResourceProfile
	Mem                   ResourceProfile
	HalfLife              time.Duration // decay half-life for the weighted quantile
	Season                string        // "" | "daily" | "weekly" | "both"
	AllowExceedLimit      bool          // may the recommendation grow requests/limits above the currently-set limit?
	Direction             Direction     // Both (default) | Down (shrink-only) | Up (grow-only)
	OOMOverridesDirection bool          // when true, an OOMKilled container may grow mem even under Down (safety > savings)
	Guaranteed            Guaranteed    // skip (default) | resizeBoth: what to do with a container whose request == limit
	// AutoQuantile (opt-in): raise the CPU quantile to 0.95 when the container's
	// burstiness index (p99 / p90 of the window) reaches BurstThreshold
	// (0 = the engine default, 3).
	AutoQuantile      bool
	BurstThreshold    float64
	HeadroomGuard     bool    // when true, floor the request at the (1-threshold) quantile so the headroom breach stays <= threshold
	HeadroomThreshold float64 // allowed breach share (default 0.02 when guard on)
	// Seasonality gate (seasonGate in the CR; 0 = the recommender defaults):
	// the minimum lag-one-season correlation of the hourly profile and the
	// minimum number of hourly pairs for the seasonal forecast to be trusted.
	SeasonMinCorrelation   float64
	SeasonMinPairs         int
	ScaleUpCooldown        time.Duration
	ScaleDownCooldown      time.Duration
	ScaleDownStabilization time.Duration
	PolicyName             string
	Profile                string
	Overridden             bool // a per-workload annotation pinned this (not inherited from a group/policy)
	Ignored                bool // tailor.k8s-tailor.dev/ignore=true: ABSOLUTE Off (no annotation/rule re-enables it)

	// Per-container refinement (see ResolveContainer). ContainerRules are the
	// candidate spec.containers rules of the matched policy/profiles, in
	// precedence order; the rest is set by ResolveContainer.
	ContainerRules   []ContainerRule
	ContainerClass   string // app | injected-sidecar | native-sidecar
	ContainerPolicy  string // where the container's effective policy came from (UI)
	Excluded         bool   // container excluded: no recommendation, no actuation
	ProxyAnnotations bool   // size an injected mesh proxy via pod-template annotations
}

var (
	policyGVR  = schema.GroupVersionResource{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}
	configGVR  = schema.GroupVersionResource{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}
	profileGVR = schema.GroupVersionResource{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}
	groupGVR   = schema.GroupVersionResource{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}
)

// KnativeServiceLabel is the built-in default grouping key: Knative stamps it on
// every revision's pods, so grouping by it collapses a service's revisions.
const KnativeServiceLabel = "serving.knative.dev/service"

// GroupRule collapses workloads sharing a label into one logical unit.
type GroupRule struct {
	GroupBy    string   // label key whose value names the group
	Namespaces []string // namespace globs (empty = all)
	Actuation  string   // "ksvc" | "direct" | "webhook"
	// Cohort (optional): restrict which generation of pods counts toward usage.
	// CohortBy is a generation label (Knative revision, pod-template-hash, ...);
	// CohortSelect "latest" keeps only the newest generation, "" / "all" keeps all.
	CohortBy     string
	CohortSelect string
	// Name is the TailorGroup CR the rule comes from ("" = built-in rule) and
	// Annotations its metadata.annotations as of the last refresh (the engine
	// persists group units' state there, see UpdateGroupAnnotation).
	Name        string
	Annotations map[string]string
}

// scaling is the reusable model (shared by policy inline + TailorProfile).
type scaling struct {
	CPU     resSpec `json:"cpu"`
	Memory  resSpec `json:"memory"`
	ScaleUp struct {
		Cooldown string `json:"cooldown"`
	} `json:"scaleUp"`
	ScaleDown struct {
		Cooldown      string `json:"cooldown"`
		Stabilization string `json:"stabilization"`
	} `json:"scaleDown"`
	// Containers: per-container overrides (first match wins). See containers.go.
	Containers []ContainerRule `json:"containers,omitempty"`
}

type window struct {
	Days  []string `json:"days"`
	Start string   `json:"start"` // HH:MM (recurring daily window)
	End   string   `json:"end"`   // HH:MM
	From  string   `json:"from"`  // YYYY-MM-DD (optional one-off campaign start, e.g. a seasonal event)
	Until string   `json:"until"` // YYYY-MM-DD (optional campaign end)
}
type scheduleSpec struct {
	Timezone    string   `json:"timezone"`
	OutsideMode string   `json:"outsideMode"`
	Windows     []window `json:"windows"`
}

type policySpec struct {
	Priority int    `json:"priority"`
	Mode     string `json:"mode"`
	Selector struct {
		Namespaces  []string          `json:"namespaces"`
		MatchLabels map[string]string `json:"matchLabels"`
	} `json:"selector"`
	ProfileRef            string       `json:"profileRef"`
	Apply                 string       `json:"apply"`                 // InPlace | Rollout | InPlacePreferred
	HalfLife              string       `json:"halfLife"`              // decay half-life (e.g. "24h", "3d")
	Season                string       `json:"season"`                // "" | daily | weekly | both
	AllowExceedLimit      bool         `json:"allowExceedLimit"`      // allow growing above the current limit
	Direction             string       `json:"direction"`             // Both | Down (shrink-only) | Up (grow-only)
	OOMOverridesDirection bool         `json:"oomOverridesDirection"` // OOM recovery may grow mem even under Down
	AutoQuantile          *bool        `json:"autoQuantile"`          // raise the CPU quantile to 0.95 for bursty containers
	BurstThreshold        float64      `json:"burstThreshold"`        // burstiness (p99/p90) that triggers it; 0 = engine default
	Guaranteed            string       `json:"guaranteed"`            // skip | resizeBoth (containers whose request == limit)
	HeadroomGuard         bool         `json:"headroomGuard"`         // floor the request so the headroom breach stays bounded
	HeadroomThreshold     float64      `json:"headroomThreshold"`     // allowed breach share (default 0.02)
	SeasonGate            seasonGate   `json:"seasonGate"`            // seasonality gate tuning (0 = defaults)
	Schedule              scheduleSpec `json:"schedule"`
	scaling
	name string
}
type resSpec struct {
	Quantile    float64 `json:"quantile"`
	Margin      float64 `json:"margin"`
	Min         string  `json:"min"`
	Max         string  `json:"max"`
	LimitFactor float64 `json:"limitFactor"`
	// EnforceLimit: memory only (see ResourceProfile.EnforceLimit). A pointer
	// so a later layer (policy inline, container rule) can switch it back off.
	EnforceLimit *bool `json:"enforceLimit,omitempty"`
}

// seasonGate tunes the seasonality gate (recommender effectiveSeason).
type seasonGate struct {
	MinCorrelation float64 `json:"minCorrelation"`
	MinPairs       int     `json:"minPairs"`
}

// Bounds of seasonGate, shared with the CRD schema (tailorpolicies.yaml).
const (
	maxBurstThreshold = 1000.0
	minSeasonPairs    = 2
	maxSeasonPairs    = 10000
)

// GlobalConfig is the cluster-wide, install-time configuration held in the
// TailorConfig singleton CR (no DB): which workload kinds to observe and
// which namespaces to skip. Seeded on first install, editable via kubectl/UI.
type GlobalConfig struct {
	ObservedKinds     []string
	ExcludeNamespaces []string
	ObserveSelector   map[string]string // only observe workloads whose labels match all (empty = all)
	SidecarNames      []string          // extra container-name globs classified as injected sidecars
}

// Store caches policies + config, refreshed periodically from the API server.
type Store struct {
	dyn      dynamic.Interface
	fallback config.Config

	mu       sync.RWMutex
	policies []policySpec
	profiles map[string]scaling
	paused   bool
	defMode  Mode
	global   GlobalConfig
	groups   []GroupRule
	crdReady bool
}

// Groups returns the effective grouping rules. When no TailorGroup CR exists, a
// built-in rule collapses Knative revisions by serving.knative.dev/service
// (ksvc actuation) so it works out of the box.
func (s *Store) Groups() []GroupRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.groups) == 0 {
		return []GroupRule{{GroupBy: KnativeServiceLabel, Actuation: "ksvc"}}
	}
	out := make([]GroupRule, len(s.groups))
	copy(out, s.groups)
	return out
}

var defaultObservedKinds = []string{"Deployment", "StatefulSet", "DaemonSet", "Pod"}

// Global returns the effective global config (defaults when the CR is absent).
func (s *Store) Global() GlobalConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.global
	if len(g.ObservedKinds) == 0 {
		g.ObservedKinds = defaultObservedKinds
	}
	if len(g.ExcludeNamespaces) == 0 {
		for ns := range s.fallback.ExcludeNamespaces {
			g.ExcludeNamespaces = append(g.ExcludeNamespaces, ns)
		}
	}
	// The engine's own namespace is excluded no matter what the CR says (a CR
	// seeded by an older chart, or edited by hand, cannot make it resize itself).
	if self := s.fallback.SelfNamespace; self != "" && !containsStr(g.ExcludeNamespaces, self) {
		g.ExcludeNamespaces = append(append([]string(nil), g.ExcludeNamespaces...), self)
	}
	return g
}

func NewStore(cfg config.Config) (*Store, error) {
	rc, err := k8s.RESTConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &Store{dyn: dyn, fallback: cfg, defMode: ModeRecommend}, nil
}

// NewStoreWithClient builds a Store over an existing dynamic client (tests use
// the client-go dynamic fake). Call Refresh to load the CRs.
func NewStoreWithClient(dyn dynamic.Interface, cfg config.Config) *Store {
	return &Store{dyn: dyn, fallback: cfg, defMode: ModeRecommend}
}

// Refresh reloads policies / profiles / groups / config from the API server once.
func (s *Store) Refresh(ctx context.Context) { s.refresh(ctx) }

func (s *Store) Paused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused
}

// SetPaused writes the global kill switch to the TailorConfig "cluster" CR
// (creating it if absent) and updates the local cache immediately.
func (s *Store) SetPaused(ctx context.Context, paused bool) error {
	res := s.dyn.Resource(configGVR)
	obj, err := res.Get(ctx, "cluster", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "tailor.k8s-tailor.dev/v1alpha1",
			"kind":       "TailorConfig",
			"metadata":   map[string]any{"name": "cluster"},
			"spec":       map[string]any{"paused": paused, "defaultMode": string(s.defMode)},
		}}
		_, err = res.Create(ctx, u, metav1.CreateOptions{})
	} else if err == nil {
		_ = unstructured.SetNestedField(obj.Object, paused, "spec", "paused")
		_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err == nil {
		s.mu.Lock()
		s.paused = paused
		s.mu.Unlock()
	}
	return err
}

// PolicyView is a UI-friendly projection of a TailorPolicy.
type PolicyView struct {
	Name                  string            `json:"name"`
	Priority              int               `json:"priority"`
	Mode                  string            `json:"mode"`
	Apply                 string            `json:"apply"`
	ProfileRef            string            `json:"profileRef"`
	Namespaces            []string          `json:"namespaces"`
	MatchLabels           map[string]string `json:"matchLabels"`
	CPUMargin             float64           `json:"cpuMargin"`
	MemMargin             float64           `json:"memMargin"`
	CPUMin                string            `json:"cpuMin"`
	CPUMax                string            `json:"cpuMax"`
	MemMin                string            `json:"memMin"`
	MemMax                string            `json:"memMax"`
	CPULimitFactor        float64           `json:"cpuLimitFactor"`
	MemLimitFactor        float64           `json:"memLimitFactor"`
	HalfLife              string            `json:"halfLife"`
	Season                string            `json:"season"`
	AllowExceedLimit      bool              `json:"allowExceedLimit"`
	Direction             string            `json:"direction"`
	OOMOverridesDirection bool              `json:"oomOverridesDirection"`
	Scheduled             bool              `json:"scheduled"`
	Schedule              ScheduleView      `json:"schedule"`
	// Containers: per-container rules (spec.containers). nil on upsert = keep
	// whatever the CR already has (rules set with kubectl survive a UI edit).
	Containers []ContainerRule `json:"containers,omitempty"`

	// Present lists the JSON keys the caller actually sent (filled by the HTTP
	// handler). UpsertPolicy then MERGES: only present fields change, everything
	// else in the CR spec (headroomGuard, quantiles, cooldowns, fields a newer
	// CRD added) is preserved. nil = the caller sent a full view: every view field
	// is written (an empty one is cleared), fields outside the view still survive.
	Present map[string]bool `json:"-"`
}

// ScheduleView / WindowView are the UI-editable projection of the CR schedule.
type ScheduleView struct {
	Timezone    string       `json:"timezone"`
	OutsideMode string       `json:"outsideMode"`
	Windows     []WindowView `json:"windows"`
}
type WindowView struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
	From  string   `json:"from"`
	Until string   `json:"until"`
}

// ListPolicies returns the cached policies for the UI (Groups panel).
func (s *Store) ListPolicies() []PolicyView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PolicyView, 0, len(s.policies))
	for _, p := range s.policies {
		out = append(out, PolicyView{
			Name: p.name, Priority: p.Priority, Mode: p.Mode, Apply: p.Apply, ProfileRef: p.ProfileRef,
			Namespaces: p.Selector.Namespaces, MatchLabels: p.Selector.MatchLabels,
			CPUMargin: p.CPU.Margin, MemMargin: p.Memory.Margin,
			CPUMin: p.CPU.Min, CPUMax: p.CPU.Max, MemMin: p.Memory.Min, MemMax: p.Memory.Max,
			CPULimitFactor: p.CPU.LimitFactor, MemLimitFactor: p.Memory.LimitFactor,
			HalfLife: p.HalfLife, Season: p.Season, AllowExceedLimit: p.AllowExceedLimit, Direction: p.Direction, OOMOverridesDirection: p.OOMOverridesDirection,
			Scheduled:  len(p.Schedule.Windows) > 0,
			Schedule:   scheduleView(p.Schedule),
			Containers: p.Containers,
		})
	}
	return out
}

func scheduleView(s scheduleSpec) ScheduleView {
	out := ScheduleView{Timezone: s.Timezone, OutsideMode: s.OutsideMode}
	for _, w := range s.Windows {
		out.Windows = append(out.Windows, WindowView{Days: w.Days, Start: w.Start, End: w.End, From: w.From, Until: w.Until})
	}
	return out
}

// UpsertPolicy creates or updates a TailorPolicy from UI/MCP input. Updates
// are a MERGE onto the live spec (see PolicyView.Present): a UI edit can never
// wipe fields set through Git/kubectl that the view does not carry.
func (s *Store) UpsertPolicy(ctx context.Context, in PolicyView) error {
	if err := ValidatePolicyView(in); err != nil {
		return err
	}
	res := s.dyn.Resource(policyGVR)
	obj, err := res.Get(ctx, in.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		spec := map[string]any{}
		applyView(spec, in)
		if _, ok := spec["mode"]; !ok {
			spec["mode"] = "Recommend"
		}
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "tailor.k8s-tailor.dev/v1alpha1",
			"kind":       "TailorPolicy",
			"metadata":   map[string]any{"name": in.Name},
			"spec":       spec,
		}}
		_, err = res.Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec") // deep copy
	if spec == nil {
		spec = map[string]any{}
	}
	applyView(spec, in)
	_ = unstructured.SetNestedMap(obj.Object, spec, "spec")
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// applyView writes the view-owned fields onto spec. A field is written only when
// the caller sent it (in.Present) — or always for a full view (Present nil). A
// zero value DELETES the key (the CRD default / engine fallback applies), so
// clearing a field in the UI works and a round-trip leaves the spec unchanged.
func applyView(spec map[string]any, in PolicyView) {
	sent := func(key string) bool { return in.Present == nil || in.Present[key] }
	put := func(key string, val any, zero bool, path ...string) {
		if !sent(key) {
			return
		}
		if zero {
			deleteNested(spec, path...)
			return
		}
		_ = unstructured.SetNestedField(spec, val, path...)
	}
	put("priority", int64(in.Priority), in.Priority == 0, "priority")
	put("mode", in.Mode, in.Mode == "", "mode")
	put("apply", in.Apply, in.Apply == "", "apply")
	put("profileRef", in.ProfileRef, in.ProfileRef == "", "profileRef")
	put("halfLife", in.HalfLife, in.HalfLife == "", "halfLife")
	put("season", in.Season, in.Season == "", "season")
	put("allowExceedLimit", true, !in.AllowExceedLimit, "allowExceedLimit")
	put("direction", in.Direction, in.Direction == "" || in.Direction == "Both", "direction")
	put("oomOverridesDirection", true, !in.OOMOverridesDirection, "oomOverridesDirection")
	put("namespaces", toAnySlice(in.Namespaces), len(in.Namespaces) == 0, "selector", "namespaces")
	put("matchLabels", toAnyMap(in.MatchLabels), len(in.MatchLabels) == 0, "selector", "matchLabels")
	put("cpuMargin", in.CPUMargin, in.CPUMargin == 0, "cpu", "margin")
	put("memMargin", in.MemMargin, in.MemMargin == 0, "memory", "margin")
	put("cpuMin", in.CPUMin, in.CPUMin == "", "cpu", "min")
	put("cpuMax", in.CPUMax, in.CPUMax == "", "cpu", "max")
	put("memMin", in.MemMin, in.MemMin == "", "memory", "min")
	put("memMax", in.MemMax, in.MemMax == "", "memory", "max")
	put("cpuLimitFactor", in.CPULimitFactor, in.CPULimitFactor == 0, "cpu", "limitFactor")
	put("memLimitFactor", in.MemLimitFactor, in.MemLimitFactor == 0, "memory", "limitFactor")
	if sent("schedule") {
		if len(in.Schedule.Windows) == 0 {
			deleteNested(spec, "schedule")
		} else {
			wins := make([]any, 0, len(in.Schedule.Windows))
			for _, w := range in.Schedule.Windows {
				m := map[string]any{}
				if len(w.Days) > 0 {
					m["days"] = toAnySlice(w.Days)
				}
				for k, v := range map[string]string{"start": w.Start, "end": w.End, "from": w.From, "until": w.Until} {
					if v != "" {
						m[k] = v
					}
				}
				wins = append(wins, m)
			}
			spec["schedule"] = map[string]any{
				"timezone":    nonEmpty(in.Schedule.Timezone, "UTC"),
				"outsideMode": nonEmpty(in.Schedule.OutsideMode, "Off"),
				"windows":     wins,
			}
		}
	}
	// containers: a full view with nil Containers keeps the CR's rules (kubectl/
	// GitOps-managed); an explicit [] (or a sent empty list) clears them.
	if (in.Present == nil && in.Containers != nil) || (in.Present != nil && in.Present["containers"]) {
		if len(in.Containers) == 0 {
			deleteNested(spec, "containers")
		} else {
			var rules []any
			raw, _ := json.Marshal(in.Containers)
			_ = json.Unmarshal(raw, &rules)
			spec["containers"] = pruneZero(rules)
		}
	}
}

// deleteNested removes path from m and drops parents left empty by the removal.
func deleteNested(m map[string]any, path ...string) {
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	child, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	deleteNested(child, path[1:]...)
	if len(child) == 0 {
		delete(m, path[0])
	}
}

// pruneZero drops zero values (0, "", false, empty maps/slices) that Go struct
// marshalling adds, so a containers round-trip writes back what was read.
func pruneZero(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			e = pruneZero(e)
			if isZero(e) {
				delete(x, k)
			} else {
				x[k] = e
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = pruneZero(x[i])
		}
		return x
	}
	return v
}

func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case int64:
		return x == 0
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// DeletePolicy removes a TailorPolicy.
func (s *Store) DeletePolicy(ctx context.Context, name string) error {
	return s.dyn.Resource(policyGVR).Delete(ctx, name, metav1.DeleteOptions{})
}

// GroupView is a UI/MCP-friendly projection of a TailorGroup collapse rule.
type GroupView struct {
	Name         string   `json:"name"`
	GroupBy      string   `json:"groupBy"`
	Namespaces   []string `json:"namespaces"`
	Actuation    string   `json:"actuation"`
	CohortBy     string   `json:"cohortBy"`
	CohortSelect string   `json:"cohortSelect"`
}

// ListGroups returns the TailorGroup collapse rules, read live from the API so
// the UI/MCP always sees current names (grouping rules aren't cached by name).
func (s *Store) ListGroups(ctx context.Context) []GroupView {
	out := []GroupView{}
	gl, err := s.dyn.Resource(groupGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for i := range gl.Items {
		spec, _ := gl.Items[i].Object["spec"].(map[string]any)
		gv := GroupView{Name: gl.Items[i].GetName(), Namespaces: toStrSlice(spec["namespaces"])}
		gv.GroupBy, _ = spec["groupBy"].(string)
		gv.Actuation, _ = spec["actuation"].(string)
		if co, ok := spec["cohort"].(map[string]any); ok {
			gv.CohortBy, _ = co["by"].(string)
			gv.CohortSelect, _ = co["select"].(string)
		}
		out = append(out, gv)
	}
	return out
}

// UpsertGroup creates or updates a TailorGroup collapse rule from UI/MCP input.
// A TailorGroup carries no Mode — it only collapses workloads into a unit;
// actuation is decided by a TailorPolicy / annotation, never here.
func (s *Store) UpsertGroup(ctx context.Context, in GroupView) error {
	spec := map[string]any{"groupBy": in.GroupBy}
	if len(in.Namespaces) > 0 {
		spec["namespaces"] = toAnySlice(in.Namespaces)
	}
	if in.Actuation != "" {
		spec["actuation"] = in.Actuation
	}
	if in.CohortBy != "" {
		spec["cohort"] = map[string]any{"by": in.CohortBy, "select": nonEmpty(in.CohortSelect, "latest")}
	}
	res := s.dyn.Resource(groupGVR)
	obj, err := res.Get(ctx, in.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "tailor.k8s-tailor.dev/v1alpha1",
			"kind":       "TailorGroup",
			"metadata":   map[string]any{"name": in.Name},
			"spec":       spec,
		}}
		_, err = res.Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	_ = unstructured.SetNestedMap(obj.Object, spec, "spec")
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// GroupAnnotation returns annotation key of the TailorGroup CR name as of the
// last refresh ("" when unknown).
func (s *Store) GroupAnnotation(name, key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.groups {
		if g.Name == name {
			return g.Annotations[key]
		}
	}
	return ""
}

// UpdateGroupAnnotation rewrites annotation key on the TailorGroup CR name from
// its LIVE value (get → mutate → update; the ClusterRole has get/update on
// tailorgroups): mutate returns the new value, "" to remove the key, or
// ok=false to leave the object untouched. The cached copy is updated too.
func (s *Store) UpdateGroupAnnotation(ctx context.Context, name, key string, mutate func(old string) (string, bool)) error {
	res := s.dyn.Resource(groupGVR)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	anns := obj.GetAnnotations()
	if anns == nil {
		anns = map[string]string{}
	}
	v, ok := mutate(anns[key])
	if !ok {
		return nil
	}
	if v == "" {
		delete(anns, key)
	} else {
		anns[key] = v
	}
	obj.SetAnnotations(anns)
	if _, err := res.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.groups {
		if s.groups[i].Name == name {
			cp := make(map[string]string, len(anns))
			for k, x := range anns {
				cp[k] = x
			}
			s.groups[i].Annotations = cp
		}
	}
	s.mu.Unlock()
	return nil
}

// DeleteGroup removes a TailorGroup.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	return s.dyn.Resource(groupGVR).Delete(ctx, name, metav1.DeleteOptions{})
}

func resMap(margin float64, min, max string, limitFactor float64) map[string]any {
	m := map[string]any{}
	if margin > 0 {
		m["margin"] = margin
	}
	if min != "" {
		m["min"] = min
	}
	if max != "" {
		m["max"] = max
	}
	if limitFactor > 0 {
		m["limitFactor"] = limitFactor
	}
	return m
}
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
func toAnyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Run refreshes on a ticker until ctx is done.
func (s *Store) Run(ctx context.Context, every time.Duration) {
	s.EnsureConfig(ctx) // seed the singleton config CR on first install
	s.refresh(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

func (s *Store) refresh(ctx context.Context) {
	pl, err := s.dyn.Resource(policyGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		// CRDs not installed yet, or no access — fall back to env defaults silently once.
		s.mu.Lock()
		s.crdReady = false
		s.mu.Unlock()
		return
	}
	policies := make([]policySpec, 0, len(pl.Items))
	for i := range pl.Items {
		raw, _ := json.Marshal(pl.Items[i].Object["spec"])
		var ps policySpec
		if json.Unmarshal(raw, &ps) == nil {
			ps.name = pl.Items[i].GetName()
			policies = append(policies, ps)
		}
	}
	sort.SliceStable(policies, func(i, j int) bool { return policies[i].Priority > policies[j].Priority })

	profiles := map[string]scaling{}
	if prl, err := s.dyn.Resource(profileGVR).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range prl.Items {
			raw, _ := json.Marshal(prl.Items[i].Object["spec"])
			var sp scaling
			if json.Unmarshal(raw, &sp) == nil {
				profiles[prl.Items[i].GetName()] = sp
			}
		}
	}

	var groups []GroupRule
	if gl, err := s.dyn.Resource(groupGVR).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range gl.Items {
			spec, _ := gl.Items[i].Object["spec"].(map[string]any)
			gb, _ := spec["groupBy"].(string)
			if gb == "" {
				continue
			}
			act, _ := spec["actuation"].(string)
			if act == "" {
				act = "ksvc"
			}
			gr := GroupRule{GroupBy: gb, Namespaces: toStrSlice(spec["namespaces"]), Actuation: act,
				Name: gl.Items[i].GetName(), Annotations: gl.Items[i].GetAnnotations()}
			if co, ok := spec["cohort"].(map[string]any); ok {
				gr.CohortBy, _ = co["by"].(string)
				gr.CohortSelect, _ = co["select"].(string)
			}
			groups = append(groups, gr)
		}
	}

	paused := false
	defMode := ModeRecommend
	var global GlobalConfig
	if cfgObj, err := s.dyn.Resource(configGVR).Get(ctx, "cluster", metav1.GetOptions{}); err == nil {
		spec, _ := cfgObj.Object["spec"].(map[string]any)
		if v, ok := spec["paused"].(bool); ok {
			paused = v
		}
		if v, ok := spec["defaultMode"].(string); ok && v != "" {
			defMode = Mode(v)
		}
		global.ObservedKinds = toStrSlice(spec["observedKinds"])
		global.ExcludeNamespaces = toStrSlice(spec["excludeNamespaces"])
		global.ObserveSelector = toStrMap(spec["observeSelector"])
		global.SidecarNames = toStrSlice(spec["sidecarNames"])
	}

	s.mu.Lock()
	s.policies = policies
	s.profiles = profiles
	s.paused = paused
	s.defMode = defMode
	s.global = global
	s.groups = groups
	s.crdReady = true
	s.mu.Unlock()
}

func toStrMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, x := range m {
		if s, ok := x.(string); ok {
			out[k] = s
		}
	}
	return out
}

func toStrSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// EnsureConfig creates the TailorConfig singleton on first install, seeded
// with sensible defaults (observed kinds + excluded namespaces), so the whole
// configuration lives in the cluster as a CR from day one.
func (s *Store) EnsureConfig(ctx context.Context) {
	res := s.dyn.Resource(configGVR)
	if _, err := res.Get(ctx, "cluster", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return // exists (or transient error) — don't clobber
	}
	exclude := make([]any, 0, len(s.fallback.ExcludeNamespaces))
	for ns := range s.fallback.ExcludeNamespaces {
		exclude = append(exclude, ns)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailor.k8s-tailor.dev/v1alpha1",
		"kind":       "TailorConfig",
		"metadata":   map[string]any{"name": "cluster"},
		"spec": map[string]any{
			"paused":            false,
			"defaultMode":       string(s.defMode),
			"observedKinds":     toAnySlice(defaultObservedKinds),
			"excludeNamespaces": exclude,
		},
	}}
	_, _ = res.Create(ctx, u, metav1.CreateOptions{})
}

// Annotation overrides (highest precedence — per-workload escape hatches,
// driven by the per-row toggle + policy dropdown in the UI).
const (
	AnnMode    = "tailor.k8s-tailor.dev/mode"    // Off|Recommend|Webhook|InPlace|Both
	AnnIgnore  = "tailor.k8s-tailor.dev/ignore"  // "true" => Off
	AnnProfile = "tailor.k8s-tailor.dev/profile" // name of a TailorProfile to apply
)

// ProfileNames lists available TailorProfiles (for the UI policy dropdown).
func (s *Store) ProfileNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.profiles))
	for n := range s.profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve returns the effective policy for a workload (namespace + labels + annotations).
func (s *Store) Resolve(namespace string, labels, annotations map[string]string) Resolved {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// baseline from env fallback (backward compatible when no CRDs)
	r := Resolved{
		Mode:                   s.defMode,
		Apply:                  ApplyInPlace,
		CPU:                    ResourceProfile{Quantile: s.fallback.CPUQuant, Margin: s.fallback.Margin, Min: s.fallback.MinCPUCores, LimitFactor: s.fallback.CPULimitFactor},
		Mem:                    ResourceProfile{Quantile: s.fallback.MemQuant, Margin: s.fallback.Margin, Min: s.fallback.MinMemBytes, LimitFactor: s.fallback.MemLimitFactor},
		HalfLife:               s.fallback.HalfLife,
		Season:                 "daily",
		Direction:              DirBoth,
		Guaranteed:             GuaranteedSkip,
		AutoQuantile:           s.fallback.AutoQuantile,
		BurstThreshold:         s.fallback.BurstThreshold,
		ScaleUpCooldown:        30 * time.Second,
		ScaleDownCooldown:      time.Hour,
		ScaleDownStabilization: 15 * time.Minute,
	}

	for _, p := range s.policies { // already sorted by priority desc
		if !matches(p, namespace, labels) {
			continue
		}
		r.PolicyName = p.name
		if p.Mode != "" {
			r.Mode = Mode(p.Mode)
		}
		if p.Apply != "" {
			r.Apply = Apply(p.Apply)
		}
		if d := parseDur(p.HalfLife); d > 0 {
			r.HalfLife = d
		}
		if p.Season != "" {
			r.Season = p.Season
		}
		r.AllowExceedLimit = p.AllowExceedLimit
		if p.Direction != "" {
			r.Direction = Direction(p.Direction)
		}
		r.OOMOverridesDirection = p.OOMOverridesDirection
		if p.AutoQuantile != nil {
			r.AutoQuantile = *p.AutoQuantile
		}
		if b := p.BurstThreshold; b > 1 && b <= maxBurstThreshold {
			r.BurstThreshold = b
		}
		if p.Guaranteed != "" {
			r.Guaranteed = Guaranteed(p.Guaranteed)
		}
		r.HeadroomGuard = p.HeadroomGuard
		if p.HeadroomThreshold > 0 {
			r.HeadroomThreshold = p.HeadroomThreshold
		}
		// Out-of-bounds values (the CRD rejects them; a CR written before the
		// schema had the field may carry anything) fall back to the defaults.
		if c := p.SeasonGate.MinCorrelation; c > 0 && c <= 1 {
			r.SeasonMinCorrelation = c
		}
		if n := p.SeasonGate.MinPairs; n >= minSeasonPairs && n <= maxSeasonPairs {
			r.SeasonMinPairs = n
		}
		// reusable scaling model first (TailorProfile), then policy inline overrides it
		r.ContainerRules = withOrigin(p.Containers, "policy", p.name)
		if p.ProfileRef != "" {
			if pr, ok := s.profiles[p.ProfileRef]; ok {
				applyScaling(&r, pr)
				r.PolicyName = p.name + " (profile " + p.ProfileRef + ")"
				r.ContainerRules = append(r.ContainerRules, withOrigin(pr.Containers, "profile", p.ProfileRef)...)
			}
		}
		applyScaling(&r, p.scaling)
		// schedule: outside the windows, fall back to outsideMode (default Off)
		if len(p.Schedule.Windows) > 0 && !scheduleActive(p.Schedule, time.Now()) {
			out := Mode(p.Schedule.OutsideMode)
			if out == "" {
				out = ModeOff
			}
			r.Mode = out
			r.PolicyName = p.name + " (off-schedule)"
		}
		break // highest-priority match wins
	}

	// Per-workload annotation overrides win over any policy (set by the per-row
	// Policy dropdown + Automated toggle in the UI).
	if p := annotations[AnnProfile]; p != "" {
		if pr, ok := s.profiles[p]; ok {
			applyScaling(&r, pr)
			r.Profile = p
			r.PolicyName = "workload:" + p
			r.ContainerRules = append(withOrigin(pr.Containers, "profile", p), r.ContainerRules...)
		}
	}
	if m := annotations[AnnMode]; m != "" {
		r.Mode = Mode(m)
		r.Overridden = true
	}
	// ignore is absolute: applied LAST so a mode annotation cannot override it
	// (and ResolveContainer re-applies it after per-container rules).
	if annotations[AnnIgnore] == "true" {
		r.Mode = ModeOff
		r.PolicyName = "annotation:ignore"
		r.Overridden = true
		r.Ignored = true
	}
	return r
}

func matches(p policySpec, namespace string, labels map[string]string) bool {
	if len(p.Selector.Namespaces) > 0 {
		found := false
		for _, ns := range p.Selector.Namespaces {
			// Exact, or shell-glob pattern (e.g. "apps-*") so a policy can target
			// a whole family of namespaces without listing each one.
			if ns == namespace {
				found = true
				break
			}
			if strings.ContainsAny(ns, "*?[") {
				if ok, err := filepath.Match(ns, namespace); err == nil && ok {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	for k, v := range p.Selector.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// applyScaling merges a scaling model (profile or policy inline) onto r.
func applyScaling(r *Resolved, sc scaling) {
	applyRes(&r.CPU, sc.CPU, false)
	applyRes(&r.Mem, sc.Memory, true)
	if d := parseDur(sc.ScaleUp.Cooldown); d > 0 {
		r.ScaleUpCooldown = d
	}
	if d := parseDur(sc.ScaleDown.Cooldown); d > 0 {
		r.ScaleDownCooldown = d
	}
	if d := parseDur(sc.ScaleDown.Stabilization); d > 0 {
		r.ScaleDownStabilization = d
	}
}

// scheduleActive reports whether now falls inside any window (day + HH:MM range).
func scheduleActive(sched scheduleSpec, now time.Time) bool {
	loc, err := time.LoadLocation(sched.Timezone)
	if err != nil || sched.Timezone == "" {
		loc = time.UTC
	}
	t := now.In(loc)
	day := t.Weekday().String()[:3] // Mon,Tue,...
	cur := t.Hour()*60 + t.Minute()
	for _, w := range sched.Windows {
		// Optional one-off date range (campaign, e.g. a seasonal event). Inclusive of
		// the whole "until" day.
		if w.From != "" {
			if from, err := time.ParseInLocation("2006-01-02", w.From, loc); err == nil && t.Before(from) {
				continue
			}
		}
		if w.Until != "" {
			if until, err := time.ParseInLocation("2006-01-02", w.Until, loc); err == nil && t.After(until.Add(24*time.Hour)) {
				continue
			}
		}
		if len(w.Days) > 0 && !containsStr(w.Days, day) {
			continue
		}
		// No HH:MM range → active for the whole day (common for a campaign window).
		if w.Start == "" && w.End == "" {
			return true
		}
		if s := parseHM(w.Start); s >= 0 && s <= cur && cur < parseHM(w.End) {
			return true
		}
	}
	return false
}

func parseHM(s string) int {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return -1
	}
	return h*60 + m
}

func containsStr(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

func applyRes(dst *ResourceProfile, src resSpec, mem bool) {
	if src.Quantile > 0 {
		dst.Quantile = src.Quantile
	}
	if src.Margin > 0 {
		dst.Margin = src.Margin
	}
	if src.LimitFactor > 0 {
		dst.LimitFactor = src.LimitFactor
	}
	if q := parseQty(src.Min, mem); q > 0 {
		dst.Min = q
	}
	if q := parseQty(src.Max, mem); q > 0 {
		dst.Max = q
	}
	if mem && src.EnforceLimit != nil {
		dst.EnforceLimit = *src.EnforceLimit
	}
}

func parseQty(s string, mem bool) float64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	_ = mem
	return q.AsApproximateFloat64()
}

func parseDur(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := config.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}
