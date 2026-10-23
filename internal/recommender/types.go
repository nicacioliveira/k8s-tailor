package recommender

import "github.com/nicacioliveira/k8s-tailor/internal/k8s"

// ContainerState is the current resource state of one workload container.
type ContainerState struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Container string `json:"container"`
	Replicas  int    `json:"replicas"`

	CPURequest float64 `json:"cpuRequest"` // cores; 0 = unset
	CPULimit   float64 `json:"cpuLimit"`
	MemRequest float64 `json:"memRequest"` // bytes; 0 = unset
	MemLimit   float64 `json:"memLimit"`
}

type Resources struct {
	CPURequest float64 `json:"cpuRequest"`
	CPULimit   float64 `json:"cpuLimit,omitempty"`
	MemRequest float64 `json:"memRequest"`
	MemLimit   float64 `json:"memLimit"`
}

type Signals struct {
	CPUQuantile      float64  `json:"cpuQuantile"`
	CPUPeak          float64  `json:"cpuPeak"`
	CPUForecast      *float64 `json:"cpuForecast"` // seasonal forecast (cores), nil if n/a
	MemQuantile      float64  `json:"memQuantile"`
	MemPeak          float64  `json:"memPeak"`
	MemForecastBytes *float64 `json:"memForecastBytes"` // seasonal forecast (bytes), nil if n/a
	MemTrendBytes    *float64 `json:"memTrendBytes"`
	Samples          int      `json:"samples"`
	// Headroom breach (SLI share_gt_over_time): fraction of window samples above the
	// recommended request. ~0 = rec safely covers usage; high = too tight.
	HeadroomBreachCPU float64 `json:"headroomBreachCpu"`
	HeadroomBreachMem float64 `json:"headroomBreachMem"`
	// Raw requests before scale-down stabilization; Stabilized = the published
	// request was held up by the stabilization window.
	RawCPURequest float64 `json:"rawCpuRequest"`
	RawMemRequest float64 `json:"rawMemRequest"`
	Stabilized    bool    `json:"stabilized"`
	// Seasonality actually used by the forecast after the consistency gate
	// ("off" when the data is not periodic enough).
	CPUSeason string `json:"cpuSeason"`
	MemSeason string `json:"memSeason"`
	// Censored CPU: usage measured under a CPU limit it keeps hitting is a lower
	// bound of the demand. CPUPinnedShare is the share of the last hour's samples
	// at >= 90% of the live CPU limit; CPUThrottledShare the mean CFS throttled-
	// period ratio over the last hour (nil = the cAdvisor series are not in the
	// metrics backend: no signal, NOT "not throttled"). CPUCensored = either
	// crossed its threshold: the limit is raised a step (Recommended.CPULimit)
	// and the request lifted to at least the live limit.
	CPUPinnedShare    float64  `json:"cpuPinnedShare"`
	CPUThrottledShare *float64 `json:"cpuThrottledShare"`
	CPUCensored       bool     `json:"cpuCensored"`
	// Burstiness is the p99 / p90 ratio of the CPU series over the window (nil =
	// fewer than 12 h of distinct samples): how far the tail sits above the level the
	// CPU request is sized from. ~1.2 for a daily cycle, ~7 for short bursts on a
	// calm baseline. CPUQuantileUsed is the quantile the request was sized from;
	// with autoQuantile on and the index at or above the threshold it is 0.95
	// (BurstQuantileRaised). BurstReason says in words why, always.
	Burstiness          *float64 `json:"burstiness"`
	CPUQuantileUsed     float64  `json:"cpuQuantileUsed"`
	BurstQuantileRaised bool     `json:"burstQuantileRaised"`
	BurstReason         string   `json:"burstReason"`
	// CPUGreedy: the container kept using whatever CPU limit it was given across
	// CPUCensorSteps raises (CPU_CENSOR_MAX_STEPS, default 3), so its demand has no
	// end that can be measured. The limit is parked at the last step and not raised
	// again until usage falls below 80% of it (CPUGreedyRelease). CPUCensorSteps is
	// how many raises the live limit already is above its anchor.
	CPUGreedy        bool `json:"cpuGreedy"`
	CPUCensorSteps   int  `json:"cpuCensorSteps"`
	CPUGreedyRelease bool `json:"cpuGreedyRelease,omitempty"`
	// MemCeiling is the schedulable memory ceiling that applied (the lower of the
	// largest eligible node's allocatable memory and the namespace LimitRange max;
	// 0 = no cluster lookup) and MemCeilingCapped whether the memory request was
	// lowered to it. Only with tailor.rbac.clusterLookups.
	MemCeiling       float64 `json:"memCeiling,omitempty"`
	MemCeilingCapped bool    `json:"memCeilingCapped,omitempty"`
}

// Recommendation is the output for one workload container.
type Recommendation struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Container string `json:"container"`
	Replicas  int    `json:"replicas"`

	Current     Resources `json:"current"` // the workload SPEC/template requests
	Applied     Resources `json:"applied"` // what the LIVE pods actually run with (in-place resizes land here, not the template)
	Recommended Resources `json:"recommended"`
	SpecDrift   bool      `json:"specDrift"` // template != live pods (an in-place resize hasn't been persisted to the spec)
	Signals     Signals   `json:"signals"`

	Mode       string `json:"mode"`       // effective mode
	Apply      string `json:"apply"`      // actuation method: InPlace | Rollout | InPlacePreferred
	Direction  string `json:"direction"`  // Both | Down (shrink-only) | Up (grow-only)
	Policy     string `json:"policy"`     // resolved policy/profile name
	Profile    string `json:"profile"`    // per-workload profile annotation (dropdown value)
	Automated  bool   `json:"automated"`  // effective mode actuates (webhook/in-place)
	Overridden bool   `json:"overridden"` // pinned by a per-workload annotation (not inherited from a group)

	// Workload-kind awareness — how the tailor treats this type, and the
	// PodDisruptionBudget guardrail it respects when actuation is disruptive.
	Managed      bool   `json:"managed"`      // false for unmanaged (owner-less) Pods
	Strategy     string `json:"strategy"`     // per-kind actuation strategy (human-readable)
	PDBName      string `json:"pdbName"`      // covering PodDisruptionBudget, if any
	PDBProtected bool   `json:"pdbProtected"` // a PDB selects this workload
	PDBAllowed   int    `json:"pdbAllowed"`   // status.disruptionsAllowed (0 = disruption blocked)

	// Autoscaler coexistence.
	Autoscaler   k8s.Autoscaler `json:"autoscaler"`
	Knative      k8s.KnativeRef `json:"knative"`
	CPUHeldByHPA bool           `json:"cpuHeldByHpa"` // HPA scales on CPU → we recommend but never actuate CPU
	MemHeldByHPA bool           `json:"memHeldByHpa"` // HPA scales on memory → hold memory too

	// Health signals.
	Confident bool `json:"confident"` // enough samples to actuate at least one resource (cpuConfident || memConfident)
	// Per-resource confidence: each resource is actuated only on its OWN coverage
	// (a container with memory data but no CPU series must not have its CPU moved).
	CPUConfident bool `json:"cpuConfident"`
	MemConfident bool `json:"memConfident"`
	OOMRisk      bool `json:"oomRisk"`     // container was OOMKilled — memory grown to recover
	LimitCapped  bool `json:"limitCapped"` // wanted more memory but capped at the current limit (needs allowExceedLimit)

	WasteUSDWeek float64 `json:"wasteUsdWeek"`

	// Container role and effective per-container policy (docs/CONTAINERS.md).
	ContainerClass   string `json:"containerClass"`   // app | injected-sidecar | native-sidecar
	NativeSidecar    bool   `json:"nativeSidecar"`    // lives in initContainers with restartPolicy Always
	InTemplate       bool   `json:"inTemplate"`       // exists in the workload template (a rollout may patch it)
	Mesh             string `json:"mesh"`             // istio | linkerd when the mesh injected it
	ContainerPolicy  string `json:"containerPolicy"`  // origin: policy | default:injected-sidecar | policy:<n> containers[i] | profile:<n> containers[i] | annotation
	ProxyAnnotations bool   `json:"proxyAnnotations"` // durable path writes the mesh's proxy resource annotations
	ActuationPath    string `json:"actuationPath"`    // none | webhook | in-place | template | ksvc | proxy-annotations:<mesh> | none:not-in-template (joined by +)
}

// strategyFor describes how the tailor actuates a workload, accounting for
// its kind AND any autoscaler it must coexist with.
func strategyFor(w k8s.Workload) string {
	base := "in-place · rolling-safe"
	switch w.Kind {
	case "DaemonSet":
		base = "in-place · per-node, never evicted"
	case "StatefulSet":
		base = "in-place · identity & volumes preserved"
	case "Pod":
		base = "in-place only · unmanaged, lost if deleted"
	case "group":
		base = "webhook at admission · ephemeral pool, sized on create"
	}
	if w.Knative.Service != "" {
		// The revision Deployment is immutable — durable resize edits the ksvc.
		base = "ksvc rollout · new revision (revision Deployment is immutable)"
	}
	switch w.Autoscaler.Kind {
	case "HPA":
		if w.Autoscaler.ManagesCPU {
			base += " · CPU held (HPA scales on CPU)"
		} else {
			base += " · HPA (memory/CPU safe)"
		}
	case "KPA":
		base += " · KPA scales on concurrency (vertical-safe)"
	}
	return base
}

// ResourceConfidence returns the per-resource confidence. A Recommendation built
// without the per-resource fields (both false — legacy callers, tests) falls back
// to the aggregate Confident for both.
func (r Recommendation) ResourceConfidence() (cpu, mem bool) {
	if !r.CPUConfident && !r.MemConfident {
		return r.Confident, r.Confident
	}
	return r.CPUConfident, r.MemConfident
}
