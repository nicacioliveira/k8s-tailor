package recommender

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

const gb = 1024 * 1024 * 1024
const stepSec = 300

// Entry is the stored result for one workload container.
type Entry struct {
	Rec                   Recommendation   `json:"rec"`
	CPUTrend              []Sample         `json:"cpuTrend"`
	MemTrend              []Sample         `json:"memTrend"`
	Mode                  policy.Mode      `json:"mode"`
	Apply                 policy.Apply     `json:"apply"`
	Direction             policy.Direction `json:"direction"`
	OOMOverridesDirection bool             `json:"oomOverridesDirection"`
	PolicyName            string           `json:"policyName"`
	ScaleUpCooldown       time.Duration    `json:"-"`
	ScaleDownCooldown     time.Duration    `json:"-"`
	// EnforceMemLimit (policy memory.enforceLimit, opt-in): actuation may ADD the
	// recommended memory limit to a container running without one. Off = a
	// memory limit is never introduced.
	EnforceMemLimit bool `json:"enforceMemLimit"`
	// GuaranteedResize (policy guaranteed: resizeBoth, opt-in): a container whose
	// request equals its limit is resized with both moving together, so the pod
	// stays Guaranteed. Off = such a pod is left alone.
	GuaranteedResize bool `json:"guaranteedResize"`

	// Container role and the actuation paths it allows (see docs/CONTAINERS.md).
	Class  k8s.ContainerClass `json:"class"`
	Native bool               `json:"native"` // lives in initContainers (restartPolicy Always)
	// NotInTemplate: the container is absent from the controller template
	// (injected at admission) — a rollout must never patch it there. Zero value =
	// in the template, so entries built elsewhere keep the legacy behavior.
	NotInTemplate    bool   `json:"notInTemplate"`
	Mesh             string `json:"mesh"`             // istio | linkerd when injected by a mesh
	ProxyAnnotations bool   `json:"proxyAnnotations"` // durable path = mesh resource annotations on the pod template
}

type Engine struct {
	cfg   config.Config
	vm    *vm.Client
	k8s   *k8s.Client
	pol   *policy.Store
	price *pricing.Provider

	mu    sync.RWMutex
	list  []Recommendation
	byKey map[string]Entry
	// containers: "ns/kind/name" -> container name -> class, for every container
	// seen in the last refresh (template + observed on running pods, including
	// excluded and non-resizable ones). The per-container write API validates
	// against it (SetContainer).
	containers map[string]map[string]k8s.ContainerClass
	// meta: "ns/kind/name" -> the workload's labels + annotations as of the last
	// refresh (updated by the UI writes), so an in-memory reflection of a write
	// resolves the effective per-container mode exactly as the refresh will
	// (ignore is absolute; per-container mode/exclude beat the workload mode).
	meta map[string]workloadMeta

	// Durable OOM memory floors: "ns/kind/workload/container" -> learned floor for
	// the mem LIMIT (already × growth factor). Accumulated across refreshes so it
	// survives pod replacement (the live OOM signal is ephemeral); pruned after
	// OOMFloorTTL. See learnOOM.
	oomMu    sync.Mutex
	oomFloor map[string]oomFloorEntry

	// Refresh serialization + coalescing of UI-triggered refreshes (SetWorkload):
	// refreshMu makes refreshes strictly sequential (the periodic loop and any
	// triggered one never overlap); trigPending coalesces a burst of triggers into
	// at most ONE queued refresh behind the running one.
	refreshMu   sync.Mutex
	trigMu      sync.Mutex
	trigPending bool
	refreshImpl func(ctx context.Context) error // nil = e.refresh (tests override)

	// now is the engine's clock (nil = time.Now): the end of the query window,
	// the seasonal-forecast target, the OOM-floor TTL and the stabilization window
	// all read it, so an offline replay (internal/backtest) can run the real code
	// in simulated time. See SetClock.
	now func() time.Time

	// Scale-down stabilization history (raw recommendations per container).
	stab stabilizer

	// Warm cache (see warm.go): entries loaded from the ConfigMap at start, served to
	// the webhook only (Admission), dropped by the first real refresh or at warmUntil.
	// warmSum / warmSavedAt remember the last write (guarded by warmMu).
	warmEntries map[string]Entry
	warmUntil   time.Time
	warmMu      sync.Mutex
	warmSum     string
	warmSavedAt time.Time

	// seasonCache: namespace -> the SEASON_WINDOW usage fetched for its weekly
	// policies, reused for SEASON_REFRESH (the weekly profile moves slowly and the
	// query is the expensive one).
	seasonMu    sync.Mutex
	seasonCache map[string]seasonEntry

	// censorAnchor: "ns/kind/workload/container" -> the CPU limit the current run
	// of censored-CPU raises is counted from (see RecommendInput.CPUCensorAnchor).
	// In memory: after a restart a run is counted from the live limit again.
	censorMu     sync.Mutex
	censorAnchor map[string]float64

	// podUnits remembers, for pods seen alive as part of a ksvc / group unit, which
	// unit they belonged to: such a pod no longer carries the labels that name its
	// unit once it is deleted, so its series would otherwise be dropped. Entries
	// expire a usage window after the pod was last seen. In memory only.
	unitMu   sync.Mutex
	podUnits map[string]podUnit

	// Cluster lookups (cfg.ClusterLookups): the memory ceilings are re-read at most
	// every ceilingsTTL, and a failure is logged once per outage.
	ceilMu     sync.Mutex
	ceilAt     time.Time
	ceilings   k8s.MemCeilings
	ceilFailed bool

	// warm is set once the first refresh succeeded (or Seed ran): before that
	// the engine holds no recommendation at all, which the webhook reports as
	// warming_up instead of muting silently.
	warm atomic.Bool
}

// censorAnchorOf returns the anchor of a container's current run of censored
// raises (0 = none).
func (e *Engine) censorAnchorOf(key string) float64 {
	e.censorMu.Lock()
	defer e.censorMu.Unlock()
	return e.censorAnchor[key]
}

// trackCensor maintains the anchor after a recommendation: a container seen
// censored without one is anchored at its live limit (the next raise is step 1);
// one released (usage under 80% of the limit again) forgets it, so a later
// episode is counted afresh. live is the CPU limit the pods carry (template limit
// when unknown).
func (e *Engine) trackCensor(key string, rec Recommendation, live float64) {
	e.censorMu.Lock()
	defer e.censorMu.Unlock()
	switch {
	case rec.Signals.CPUGreedyRelease:
		delete(e.censorAnchor, key)
	case rec.Signals.CPUCensored && e.censorAnchor[key] == 0 && live > 0:
		if e.censorAnchor == nil {
			e.censorAnchor = map[string]float64{}
		}
		e.censorAnchor[key] = live
	}
}

type seasonEntry struct {
	at       time.Time
	cpu, mem map[string]map[string][]Sample // workloadKey -> container -> samples
}

// weeklySeason reports whether a season setting needs week-lag history.
func weeklySeason(season string) bool { return season == "weekly" || season == "both" }

// seasonSeries returns the CPU and memory usage of ns over SEASON_WINDOW ending
// at end, or nil when it is off, not longer than WINDOW, or the query fails (the
// engine then keeps judging seasonality on the WINDOW series, as before). The
// result is cached per namespace for SEASON_REFRESH.
func (e *Engine) seasonSeries(ctx context.Context, ns string, end time.Time, ownerOf func(ns, pod string) (k8s.Owner, bool)) (cpu, mem map[string]map[string][]Sample) {
	if e.cfg.SeasonWindow <= e.cfg.Window || e.vm == nil {
		return nil, nil
	}
	e.seasonMu.Lock()
	defer e.seasonMu.Unlock()
	if c, ok := e.seasonCache[ns]; ok && end.Sub(c.at) < e.cfg.SeasonRefresh && !end.Before(c.at) {
		return c.cpu, c.mem
	}
	cpuExpr, memExpr := e.usageExprs(ns)
	start := end.Add(-e.cfg.SeasonWindow)
	cs, err := e.vm.QueryRange(ctx, cpuExpr, start, end, time.Time{}, stepSec)
	var ms []vm.Series
	if err == nil {
		ms, err = e.vm.QueryRange(ctx, memExpr, start, end, time.Time{}, stepSec)
	}
	if err != nil {
		log.Printf("refresh: ns %s: %s history for weekly seasonality unavailable, using the %s window: %v", ns, e.cfg.SeasonWindow, e.cfg.Window, err)
		return nil, nil
	}
	if e.seasonCache == nil {
		e.seasonCache = map[string]seasonEntry{}
	}
	ent := seasonEntry{at: end, cpu: foldSeries(ns, cs, ownerOf), mem: foldSeries(ns, ms, ownerOf)}
	e.seasonCache[ns] = ent
	return ent.cpu, ent.mem
}

type podUnit struct {
	owner k8s.Owner
	seen  time.Time
}

// rememberUnits records the unit of every live grouped pod of snap and forgets
// the entries older than the usage window and the pods snap dropped as a stale
// cohort. Deployment, StatefulSet and DaemonSet pods are not cached: a deleted
// one is attributed by its generated name (k8s.ClusterSnapshot.OwnerOf).
func (e *Engine) rememberUnits(snap *k8s.ClusterSnapshot) {
	now := e.clock()
	e.unitMu.Lock()
	defer e.unitMu.Unlock()
	if e.podUnits == nil {
		e.podUnits = map[string]podUnit{}
	}
	for k, o := range snap.Ownership {
		if o.Kind == "ksvc" || o.Kind == "group" {
			e.podUnits[k] = podUnit{owner: o, seen: now}
		}
	}
	for k, u := range e.podUnits {
		if snap.Stale[k] || now.Sub(u.seen) > e.cfg.Window {
			delete(e.podUnits, k)
		}
	}
}

func (e *Engine) podUnitCount() int {
	e.unitMu.Lock()
	defer e.unitMu.Unlock()
	return len(e.podUnits)
}

// ownerOf attributes a pod name from the metrics to its unit: the snapshot first
// (live pods, generated names), then the cache of pods that were alive earlier.
func (e *Engine) ownerOf(snap *k8s.ClusterSnapshot) func(ns, pod string) (k8s.Owner, bool) {
	return func(ns, pod string) (k8s.Owner, bool) {
		if o, ok := snap.OwnerOf(ns, pod); ok {
			return o, true
		}
		e.unitMu.Lock()
		defer e.unitMu.Unlock()
		u, ok := e.podUnits[ns+"/"+pod]
		return u.owner, ok
	}
}

// ceilingsTTL is how long the node/LimitRange read is reused: allocatable and
// LimitRanges change rarely, and a refresh runs every REFRESH_EVERY.
const ceilingsTTL = 5 * time.Minute

// memCeilings returns the cluster-derived memory ceilings, or none when
// CLUSTER_LOOKUPS is off or the read fails (missing RBAC, API error): the engine
// then sizes exactly as it does without the opt-in.
func (e *Engine) memCeilings(ctx context.Context) k8s.MemCeilings {
	if !e.cfg.ClusterLookups || e.k8s == nil {
		return k8s.MemCeilings{}
	}
	e.ceilMu.Lock()
	defer e.ceilMu.Unlock()
	now := e.clock()
	if !e.ceilAt.IsZero() && now.Sub(e.ceilAt) < ceilingsTTL {
		return e.ceilings
	}
	c, err := e.k8s.MemoryCeilings(ctx)
	if err != nil {
		if !e.ceilFailed {
			log.Printf("refresh: cluster lookups failed, memory ceilings not applied (is tailor.rbac.clusterLookups on?): %v", err)
		}
		e.ceilFailed = true
		return k8s.MemCeilings{}
	}
	e.ceilFailed = false
	e.ceilAt, e.ceilings = now, c
	return c
}

// Warm reports whether at least one refresh completed since the engine
// started. Recommendations are rebuilt from the metrics backend's history on
// that first refresh, so a restart does not wait for new samples.
func (e *Engine) Warm() bool { return e.warm.Load() }

// SetClock replaces the engine clock (nil restores time.Now). Intended for
// offline replay and tests; not safe to call concurrently with Refresh.
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

func (e *Engine) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

type oomFloorEntry struct {
	Mem float64   // learned floor for the mem limit, bytes
	At  time.Time // last OOM observation feeding this floor
}

func NewEngine(cfg config.Config, vmc *vm.Client, kc *k8s.Client, pol *policy.Store, price *pricing.Provider) *Engine {
	return &Engine{cfg: cfg, vm: vmc, k8s: kc, pol: pol, price: price, byKey: map[string]Entry{}, oomFloor: map[string]oomFloorEntry{}}
}

// learnOOM folds this refresh's live OOM observations into the durable floor map
// and prunes floors older than the TTL. A floor only ever rises within its TTL
// (an OOM is a proof that memory was insufficient — we don't un-learn it while
// it's fresh); after TTL with no new OOM it is forgotten so a fixed workload
// resumes normal sizing.
func (e *Engine) learnOOM(obs map[string]k8s.OOMObservation) map[string]bool {
	now := e.clock()
	e.oomMu.Lock()
	defer e.oomMu.Unlock()
	changed := map[string]bool{}
	for key, o := range obs {
		if o.MemBytes <= 0 {
			continue
		}
		cur, ok := e.oomFloor[key]
		// The SAME kill (same termination time) is learned once: after the floor
		// raised the limit in place, the pod's lastState still says OOMKilled and
		// the snapshot reports the RAISED limit — refolding it would compound the
		// floor ×growth on every refresh.
		if ok && !o.At.After(cur.At) {
			continue
		}
		floor := o.MemBytes * e.cfg.OOMGrowthFactor
		fresh := ok && now.Sub(cur.At) < e.cfg.OOMFloorTTL
		if fresh && cur.Mem > floor {
			floor = cur.Mem // never lower an active floor
		}
		at := o.At
		if ok && cur.At.After(at) {
			at = cur.At
		}
		next := oomFloorEntry{Mem: floor, At: at}
		if !ok || next.Mem != cur.Mem || !next.At.Equal(cur.At) {
			changed[key] = true
		}
		e.oomFloor[key] = next
	}
	for key, f := range e.oomFloor {
		if now.Sub(f.At) >= e.cfg.OOMFloorTTL {
			delete(e.oomFloor, key)
		}
	}
	return changed
}

// seedOOM rebuilds floors persisted on the workloads (AnnOOMFloor) that this
// process doesn't hold yet — after a restart or leader failover the OOMKilled
// pod is usually gone, and the live signal with it. Expired floors are ignored;
// a persisted floor only ever raises (or refreshes) an in-memory one.
func (e *Engine) seedOOM(workloads []k8s.Workload) {
	now := e.clock()
	e.oomMu.Lock()
	defer e.oomMu.Unlock()
	for _, w := range workloads {
		for c, f := range e.persistedFloors(w) {
			if f.Mem <= 0 || now.Sub(f.At) >= e.cfg.OOMFloorTTL {
				continue
			}
			key := w.Namespace + "/" + w.Kind + "/" + w.Name + "/" + c
			cur, ok := e.oomFloor[key]
			if !ok || f.Mem > cur.Mem || (f.Mem == cur.Mem && f.At.After(cur.At)) {
				e.oomFloor[key] = oomFloorEntry{Mem: f.Mem, At: f.At}
			}
		}
	}
}

// persistedFloors returns the floors persisted for workload w (container ->
// floor): the workload's own AnnOOMFloor (controllers, and a ksvc's Service
// metadata), or, for a "group" unit, its entries in the AnnGroupOOMFloor of the
// TailorGroup CR that folded it. A bare Pod is never persisted (see persistOOM).
func (e *Engine) persistedFloors(w k8s.Workload) map[string]k8s.OOMFloor {
	if w.Kind == "group" {
		if w.GroupSource == "" || e.pol == nil {
			return nil
		}
		prefix := w.Namespace + "/" + w.Name + "/"
		out := map[string]k8s.OOMFloor{}
		for k, f := range k8s.ParseOOMFloors(e.pol.GroupAnnotation(w.GroupSource, k8s.AnnGroupOOMFloor)) {
			if strings.HasPrefix(k, prefix) {
				out[strings.TrimPrefix(k, prefix)] = f
			}
		}
		return out
	}
	if v := w.Annotations[k8s.AnnOOMFloor]; v != "" {
		return k8s.ParseOOMFloors(v)
	}
	return nil
}

// persistOOM writes the floors of every workload with a changed floor back to
// its AnnOOMFloor annotation (merged: all its live floors), only for workloads
// that actuate — a recommend-only workload is never written to. Best-effort.
// Where it lands depends on the kind: a controller's metadata
// (Deployment/StatefulSet/DaemonSet), a Knative Service's metadata (ksvc: not
// its template, so no new revision), or the TailorGroup CR of a "group" unit
// (AnnGroupOOMFloor, keyed namespace/unit/container; other units' entries are
// kept, expired ones pruned). A bare Pod is not persisted: the floor protects
// the next pod of a controller, and a bare Pod has none (once it is gone there
// is nothing left to size).
func (e *Engine) persistOOM(ctx context.Context, workloads []k8s.Workload, changed map[string]bool, actuates func(k8s.Workload) bool) {
	if len(changed) == 0 || e.k8s == nil {
		return
	}
	for _, w := range workloads {
		switch w.Kind {
		case "Deployment", "StatefulSet", "DaemonSet", "ksvc":
		case "group":
			if w.GroupSource == "" || e.pol == nil {
				continue // no TailorGroup object to write to
			}
		default:
			continue
		}
		prefix := w.Namespace + "/" + w.Kind + "/" + w.Name + "/"
		hit := false
		for k := range changed {
			if strings.HasPrefix(k, prefix) {
				hit = true
				break
			}
		}
		if !hit || !actuates(w) {
			continue
		}
		floors := map[string]k8s.OOMFloor{}
		e.oomMu.Lock()
		for k, f := range e.oomFloor {
			if strings.HasPrefix(k, prefix) {
				floors[strings.TrimPrefix(k, prefix)] = k8s.OOMFloor{Mem: f.Mem, At: f.At.UTC()}
			}
		}
		e.oomMu.Unlock()
		var err error
		switch w.Kind {
		case "ksvc":
			v := k8s.FormatOOMFloors(floors)
			err = e.k8s.AnnotateKnativeService(ctx, w.Namespace, w.Name, map[string]*string{k8s.AnnOOMFloor: &v})
		case "group":
			err = e.persistGroupFloors(ctx, w, floors)
		default:
			v := k8s.FormatOOMFloors(floors)
			err = e.k8s.AnnotateWorkload(ctx, w.Kind, w.Namespace, w.Name, map[string]*string{k8s.AnnOOMFloor: &v})
		}
		if err != nil {
			log.Printf("persist OOM floor on %s/%s/%s: %v", w.Namespace, w.Kind, w.Name, err)
		}
	}
}

// persistGroupFloors merges a group unit's floors into the AnnGroupOOMFloor of
// its TailorGroup CR: this unit's entries are replaced, other units' kept,
// expired entries (past OOMFloorTTL) dropped so the annotation stays bounded.
func (e *Engine) persistGroupFloors(ctx context.Context, w k8s.Workload, floors map[string]k8s.OOMFloor) error {
	now := e.clock()
	prefix := w.Namespace + "/" + w.Name + "/"
	return e.pol.UpdateGroupAnnotation(ctx, w.GroupSource, k8s.AnnGroupOOMFloor, func(old string) (string, bool) {
		all := k8s.ParseOOMFloors(old)
		for k, f := range all {
			if strings.HasPrefix(k, prefix) || now.Sub(f.At) >= e.cfg.OOMFloorTTL {
				delete(all, k)
			}
		}
		for c, f := range floors {
			all[prefix+c] = f
		}
		next := ""
		if len(all) > 0 {
			next = k8s.FormatOOMFloors(all)
		}
		return next, next != old
	})
}

// oomFloorFor returns the active learned mem-limit floor for a container, or 0
// when none is active (never seen, or expired past the TTL).
func (e *Engine) oomFloorFor(key string) float64 {
	e.oomMu.Lock()
	defer e.oomMu.Unlock()
	f, ok := e.oomFloor[key]
	if !ok || e.clock().Sub(f.At) >= e.cfg.OOMFloorTTL {
		return 0
	}
	return f.Mem
}

// Paused reports the global kill switch (TailorConfig.spec.paused).
func (e *Engine) Paused() bool { return e.pol.Paused() }

// SetPaused writes the kill switch to the TailorConfig CR.
func (e *Engine) SetPaused(ctx context.Context, paused bool) error {
	return e.pol.SetPaused(ctx, paused)
}

// Policy groups (managed from the UI, stored as TailorPolicy CRs).
func (e *Engine) ListPolicies() []policy.PolicyView { return e.pol.ListPolicies() }
func (e *Engine) UpsertPolicy(ctx context.Context, in policy.PolicyView) error {
	return e.pol.UpsertPolicy(ctx, in)
}
func (e *Engine) DeletePolicy(ctx context.Context, name string) error {
	return e.pol.DeletePolicy(ctx, name)
}
func (e *Engine) ListGroups(ctx context.Context) []policy.GroupView { return e.pol.ListGroups(ctx) }
func (e *Engine) UpsertGroup(ctx context.Context, in policy.GroupView) error {
	return e.pol.UpsertGroup(ctx, in)
}
func (e *Engine) DeleteGroup(ctx context.Context, name string) error {
	return e.pol.DeleteGroup(ctx, name)
}

// GroupMembers lists the raw workloads a named group rule folds (drill-down).
func (e *Engine) GroupMembers(ctx context.Context, name string) []k8s.WorkloadRef {
	for _, g := range e.pol.ListGroups(ctx) {
		if g.Name == name {
			return e.k8s.GroupMembers(ctx, g.GroupBy, g.Namespaces)
		}
	}
	return []k8s.WorkloadRef{}
}

// ProfileNames lists reusable profiles (UI policy dropdown).
func (e *Engine) ProfileNames() []string { return e.pol.ProfileNames() }

// SetWorkload writes per-workload overrides as annotations (the per-row toggle +
// policy dropdown). nil = leave unchanged; empty string = clear the annotation.
func (e *Engine) SetWorkload(ctx context.Context, ns, kind, name string, mode, profile *string) error {
	anns := map[string]*string{}
	if mode != nil {
		v, err := policy.ParseModeOverride(*mode)
		if err != nil {
			return err
		}
		anns[policy.AnnMode] = v // nil = remove ("" or "inherit")
	}
	if profile != nil {
		if *profile == "" {
			anns[policy.AnnProfile] = nil
		} else {
			v := *profile
			anns[policy.AnnProfile] = &v
		}
	}
	if len(anns) == 0 {
		return nil
	}
	if err := e.annotate(ctx, kind, ns, name, anns); err != nil {
		return err
	}
	// Reflect the change immediately in the in-memory store so the UI updates on
	// the next refetch (instead of waiting up to a full refresh cycle). The mode is
	// re-resolved through the policy (reflectLocked): an injected sidecar keeps its
	// capped mode, an ignored workload stays Off, a container pinned by its own
	// annotation keeps it. The profile name is reflected too — its numbers get
	// recomputed by the async Refresh below, but the dropdown must not flicker back.
	e.mu.Lock()
	e.reflectLocked(ns, kind, name, anns)
	if profile != nil {
		for i := range e.list {
			r := &e.list[i]
			if r.Namespace == ns && r.Kind == kind && r.Workload == name {
				r.Profile = *profile
			}
		}
	}
	e.mu.Unlock()
	// A profile change needs a full recompute (new numbers) — kick it in the background.
	e.TriggerRefresh()
	return nil
}

type workloadMeta struct {
	labels, annotations map[string]string
}

// reflectLocked applies an annotation write (nil value = removed) to the
// workload's cached metadata and re-resolves the effective mode of every
// in-memory row/entry of that workload through the SAME policy resolution the
// refresh uses — so a UI write takes effect at once but can never yield a mode
// the refresh would not (an ignored workload, or a container pinned or excluded
// by its own annotation, stays Off). Unknown workload (not in the last refresh):
// nothing to reflect; the triggered refresh resolves it. Caller holds e.mu.
func (e *Engine) reflectLocked(ns, kind, name string, anns map[string]*string) {
	wk := ns + "/" + kind + "/" + name
	m, ok := e.meta[wk]
	if !ok || e.pol == nil {
		return
	}
	next := make(map[string]string, len(m.annotations)+len(anns))
	for k, v := range m.annotations {
		next[k] = v
	}
	for k, v := range anns {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = *v
		}
	}
	m.annotations = next
	e.meta[wk] = m
	wres := e.pol.Resolve(ns, m.labels, next)
	for i := range e.list {
		r := &e.list[i]
		if r.Namespace == ns && r.Kind == kind && r.Workload == name {
			res := e.pol.ResolveContainer(wres, next, r.Container, r.ContainerClass)
			r.Mode = string(res.Mode)
			r.Automated = res.Mode.Webhook() || res.Mode.InPlace()
			r.Overridden = res.Overridden
		}
	}
	prefix := wk + "/"
	for k, en := range e.byKey {
		if strings.HasPrefix(k, prefix) {
			en.Mode = e.pol.ResolveContainer(wres, next, strings.TrimPrefix(k, prefix), string(en.Class)).Mode
			e.byKey[k] = en
		}
	}
}

// Errors of the per-workload / per-container write API (the HTTP layer maps
// them to 404 / 400).
var (
	ErrWorkloadNotFound      = errors.New("workload not found")
	ErrContainerNotFound     = errors.New("container not found")
	ErrContainerNotResizable = errors.New("container is not resizable (init or ephemeral)")
	ErrNothingToChange       = errors.New("nothing to change: send mode and/or exclude")
)

// annotate writes workload annotations, mapping an API-server NotFound to
// ErrWorkloadNotFound.
func (e *Engine) annotate(ctx context.Context, kind, ns, name string, anns map[string]*string) error {
	err := e.k8s.AnnotateWorkload(ctx, kind, ns, name, anns)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s %s/%s", ErrWorkloadNotFound, kind, ns, name)
	}
	return err
}

// SetContainer writes the per-container overrides
// tailor.k8s-tailor.dev/container.<name>.mode and .exclude on the workload — the
// same annotations a kubectl annotate would set, resolved by
// policy.ResolveContainer on the next refresh. mode: Off|Recommend|Webhook|
// InPlace|Both, or ""/"inherit" to remove the override; exclude: true writes
// "true", false removes it; nil leaves a field unchanged. For an injected
// sidecar an explicit mode IS the opt-in that lifts its Recommend cap.
//
// The container must have been seen in the last refresh (template or running
// pods) and must be resizable: init and ephemeral containers are never sized,
// so an override on them would be a no-op that looks like it worked.
func (e *Engine) SetContainer(ctx context.Context, ns, kind, name, container string, mode *string, exclude *bool) error {
	if mode == nil && exclude == nil {
		return ErrNothingToChange
	}
	anns := map[string]*string{}
	if mode != nil {
		v, err := policy.ParseModeOverride(*mode)
		if err != nil {
			return err
		}
		anns[policy.AnnContainer(container, "mode")] = v
	}
	if exclude != nil {
		var v *string
		if *exclude {
			t := "true"
			v = &t
		}
		anns[policy.AnnContainer(container, "exclude")] = v
	}
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Pod":
	default:
		return fmt.Errorf("%w %q: per-container overrides are written on Deployment, StatefulSet, DaemonSet or Pod", k8s.ErrUnsupportedKind, kind)
	}
	e.mu.RLock()
	wl, okWL := e.containers[ns+"/"+kind+"/"+name]
	class, okC := wl[container]
	e.mu.RUnlock()
	switch {
	case !okWL:
		return fmt.Errorf("%w: %s %s/%s is not in the last snapshot", ErrWorkloadNotFound, kind, ns, name)
	case container == "" || !okC:
		return fmt.Errorf("%w: %q in %s %s/%s", ErrContainerNotFound, container, kind, ns, name)
	case !class.Resizable():
		return fmt.Errorf("%w: %q is %s", ErrContainerNotResizable, container, class)
	}
	if err := e.annotate(ctx, kind, ns, name, anns); err != nil {
		return err
	}
	// Reflect the write at once, re-resolved through the policy (reflectLocked):
	// an ignored workload stays Off whatever the container mode; an excluded
	// container is Off. Re-inclusion of an excluded container (it has no entry)
	// waits for the refresh triggered below.
	e.mu.Lock()
	e.reflectLocked(ns, kind, name, anns)
	e.mu.Unlock()
	e.TriggerRefresh()
	return nil
}

// RefreshTimeout bounds one recommender refresh: 2× the refresh interval, never
// below 30s. A refresh is O(namespaces) VM queries; without a bound a hung
// backend would pile up refreshes forever.
func (e *Engine) RefreshTimeout() time.Duration {
	d := 2 * e.cfg.RefreshEvery
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// TriggerRefresh schedules an asynchronous refresh with a timeout, coalescing
// bursts: if one is already QUEUED (waiting behind a running refresh) the call
// is a no-op, so N rapid UI clicks cost at most one refresh after the current
// one — never N concurrent full refreshes.
func (e *Engine) TriggerRefresh() {
	e.trigMu.Lock()
	if e.trigPending {
		e.trigMu.Unlock()
		return
	}
	e.trigPending = true
	e.trigMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), e.RefreshTimeout())
		defer cancel()
		if err := e.refreshLocked(ctx, func() {
			e.trigMu.Lock()
			e.trigPending = false // started: later triggers queue a NEW refresh
			e.trigMu.Unlock()
		}); err != nil {
			log.Printf("triggered refresh: %v", err)
		}
	}()
}

// InPlaceSupported reports whether the cluster can resize pods in place.
func (e *Engine) InPlaceSupported(ctx context.Context) bool { return e.k8s.InPlaceSupported(ctx) }

// Pricing exposes the active pricing basis for the UI.
func (e *Engine) PricingSource() string   { return e.price.Source() }
func (e *Engine) PriceCPUCoreHr() float64 { return e.price.CPUCoreHr() }
func (e *Engine) PriceGBHr() float64      { return e.price.GBHr() }

// rangeDefaults fills a zero start/end with the default trend window and clamps
// the step (absolute from/to override it when provided by the caller).
func (e *Engine) rangeDefaults(start, end time.Time, step int) (time.Time, time.Time, int) {
	if end.IsZero() {
		end = e.clock()
	}
	if start.IsZero() || !start.Before(end) {
		w := e.cfg.TrendWindow
		if w <= 0 {
			w = 3 * time.Hour
		}
		start = end.Add(-w)
	}
	if step <= 0 {
		step = e.cfg.TrendStepSec
	}
	if step <= 0 {
		step = 60
	}
	return start, end, step
}

// DetailSeries builds the dense time series the drawer charts need, over a
// SHORT, FINE-GRAINED window (cfg.TrendWindow at cfg.TrendStepSec) so movement is
// visible over minutes — the multi-day recommendation window at a 5-minute step
// collapses to a couple of points for freshly-created workloads. It returns both
// measured usage (from cAdvisor/kubelet) and OUR OWN recommended/current series
// (tailor_* metrics read back from the same vmsingle).
func (e *Engine) DetailSeries(ctx context.Context, ns, kind, wl, c string, start, end time.Time, step int) map[string][]Sample {
	start, end, step = e.rangeDefaults(start, end, step)

	run := func(query string) []Sample {
		rows, err := e.vm.QueryRange(ctx, query, start, end, time.Time{}, step)
		if err != nil || len(rows) == 0 {
			return nil
		}
		out := make([]Sample, 0, len(rows[0].Samples))
		for _, p := range rows[0].Samples {
			out = append(out, Sample{T: p.T, V: p.V})
		}
		return out
	}

	// Measured usage: resolve this workload's pods, then sum across them so the
	// chart shows the workload's real CPU/mem over time (kubelet has only pod labels).
	var cpuTrend, memTrend []Sample
	if own, err := e.k8s.Ownership(ctx, ns); err == nil {
		var pods []string
		for pod, o := range own {
			if o.Kind == kind && o.Name == wl {
				// Pod names land in a regex matcher: quote them ('.' is a meta).
				pods = append(pods, regexp.QuoteMeta(pod))
			}
		}
		if len(pods) > 0 {
			podRe := strings.Join(pods, "|")
			base := vm.WithJob(fmt.Sprintf(`{namespace=%q,container=%q,pod=~%q}`, ns, c, podRe), e.cfg.VMJob)
			cpuTrend = run(fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total%s[2m]))`, base))
			memTrend = run(fmt.Sprintf(`sum(container_memory_working_set_bytes%s)`, base))
		}
	}

	lbl := fmt.Sprintf(`{namespace=%q,workload=%q,container=%q}`, ns, wl, c)
	return map[string][]Sample{
		"cpuTrend": cpuTrend,
		"memTrend": memTrend,
		"cpuRec":   run("tailor_recommended_cpu_cores" + lbl),
		"cpuCur":   run("tailor_current_cpu_cores" + lbl),
		"memRec":   run("tailor_recommended_mem_bytes" + lbl),
		"memCur":   run("tailor_current_mem_bytes" + lbl),
	}
}

// ClusterUsageSeries returns the cluster-aggregate measured CPU (cores) and
// memory (bytes) usage over the short, fine-grained trend window — the dense
// series the Overview charts render. Summed across all in-scope namespaces.
func (e *Engine) ClusterUsageSeries(ctx context.Context, start, end time.Time, step int) ([]UsagePoint, error) {
	nss := e.Namespaces()
	if len(nss) == 0 {
		return nil, nil
	}
	start, end, step = e.rangeDefaults(start, end, step)
	sel := vm.WithJob(fmt.Sprintf(`{namespace=~%q,container!="",container!="POD"}`, strings.Join(nss, "|")), e.cfg.VMJob)
	cpuRows, err := e.vm.QueryRange(ctx, `sum(rate(container_cpu_usage_seconds_total`+sel+`[2m]))`, start, end, time.Time{}, step)
	if err != nil {
		return nil, err
	}
	memRows, err := e.vm.QueryRange(ctx, `sum(container_memory_working_set_bytes`+sel+`)`, start, end, time.Time{}, step)
	if err != nil {
		return nil, err
	}
	byT := map[float64]*UsagePoint{}
	order := []float64{}
	add := func(rows []vm.Series, set func(p *UsagePoint, v float64)) {
		if len(rows) == 0 {
			return
		}
		for _, s := range rows[0].Samples {
			p, ok := byT[s.T]
			if !ok {
				p = &UsagePoint{T: s.T}
				byT[s.T] = p
				order = append(order, s.T)
			}
			set(p, s.V)
		}
	}
	add(cpuRows, func(p *UsagePoint, v float64) { p.CPU = v })
	add(memRows, func(p *UsagePoint, v float64) { p.Mem = v })
	sort.Float64s(order)
	out := make([]UsagePoint, 0, len(order))
	for _, t := range order {
		out = append(out, *byT[t])
	}
	return out, nil
}

// EconomySeries returns the cluster savings ($/week) trend from the same VM.
func (e *Engine) EconomySeries(ctx context.Context, start, end time.Time, stepSec int) []Sample {
	rows, err := e.vm.QueryRange(ctx, "sum(clamp_min(tailor_waste_usd_week, 0))", start, end, time.Time{}, stepSec)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]Sample, 0, len(rows[0].Samples))
	for _, p := range rows[0].Samples {
		out = append(out, Sample{T: p.T, V: p.V})
	}
	return out
}

// Namespaces returns the distinct namespaces with current recommendations.
func (e *Engine) Namespaces() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, r := range e.list {
		if !seen[r.Namespace] {
			seen[r.Namespace] = true
			out = append(out, r.Namespace)
		}
	}
	return out
}

func Key(ns, kind, workload, container string) string {
	return ns + "/" + kind + "/" + workload + "/" + container
}

// splitKey is the inverse of Key (names cannot contain '/').
func splitKey(k string) (ns, kind, workload, container string) {
	p := strings.SplitN(k, "/", 4)
	for len(p) < 4 {
		p = append(p, "")
	}
	return p[0], p[1], p[2], p[3]
}

func (e *Engine) List() []Recommendation {
	e.mu.RLock()
	defer e.mu.RUnlock()
	// Return a copy: SetWorkload mutates e.list[i] fields in place under the write
	// lock, so handing out the shared backing array would let a concurrent scrape /
	// API read tear a Recommendation struct after the RLock is released.
	out := make([]Recommendation, len(e.list))
	copy(out, e.list)
	return out
}

// Seed replaces the in-memory recommendation set (list + per-container
// entries) wholesale. Refresh is the production writer; Seed exists so callers
// in other packages (webhook/reconciler/server tests) can drive the engine
// without a metrics backend.
func (e *Engine) Seed(list []Recommendation, entries map[string]Entry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append([]Recommendation(nil), list...)
	e.byKey = make(map[string]Entry, len(entries))
	for k, v := range entries {
		e.byKey[k] = v
	}
	e.warmEntries = nil
	e.warm.Store(true)
}

func (e *Engine) Get(ns, kind, workload, container string) (Entry, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	en, ok := e.byKey[Key(ns, kind, workload, container)]
	return en, ok
}

// Refresh recomputes recommendations for every in-scope workload container.
// Refreshes are serialized (never overlap) and instrumented.
func (e *Engine) Refresh(ctx context.Context) error {
	return e.refreshLocked(ctx, nil)
}

func (e *Engine) refreshLocked(ctx context.Context, onStart func()) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	if onStart != nil {
		onStart()
	}
	impl := e.refreshImpl
	if impl == nil {
		impl = e.refresh
	}
	t0 := time.Now()
	err := impl(ctx)
	result := "ok"
	if err != nil {
		result = "error"
	} else {
		metrics.LastRefresh.Set(float64(time.Now().Unix()))
		e.warm.Store(true)
	}
	metrics.RefreshDuration.Observe(time.Since(t0).Seconds(), result)
	return err
}

func (e *Engine) refresh(ctx context.Context) error {
	// Apply TailorGroup collapse keys before listing (built-in Knative default
	// always included). This makes grouped units (ksvc) consistent for both the
	// recommender and the reconciler, which share this client.
	groupKeys := make([]k8s.GroupKey, 0)
	for _, gr := range e.pol.Groups() {
		// A group's logical Kind follows its actuation: Knative → "ksvc" (patch the
		// Service); everything else → "group" (ephemeral members sized at admission).
		kind := "group"
		if gr.Actuation == "ksvc" {
			kind = "ksvc"
		}
		groupKeys = append(groupKeys, k8s.GroupKey{
			Label: gr.GroupBy, Kind: kind, Namespaces: gr.Namespaces,
			CohortLabel: gr.CohortBy, CohortSelect: gr.CohortSelect, Source: gr.Name,
		})
	}
	e.k8s.SetGroupKeys(groupKeys)
	// Scope from the global config CR (which kinds to observe, which namespaces to
	// skip, extra sidecar names) — seeded on install, editable via kubectl/UI.
	g := e.pol.Global()
	e.k8s.SetSidecarNames(g.SidecarNames)

	workloads, err := e.k8s.ListWorkloads(ctx)
	if err != nil {
		return err
	}
	observed := map[string]bool{}
	for _, k := range g.ObservedKinds {
		observed[k] = true
	}
	excluded := map[string]bool{}
	for _, ns := range g.ExcludeNamespaces {
		excluded[ns] = true
	}
	matchesSelector := func(lbls map[string]string) bool {
		for k, v := range g.ObserveSelector {
			if lbls[k] != v {
				return false
			}
		}
		return true
	}
	filtered := workloads[:0]
	for _, w := range workloads {
		// ksvc/group are collapsed synthetic units — always observed (they stand in
		// for per-revision Deployments or a pool of ephemeral sandbox pods, which are
		// skipped/uncounted upstream).
		if (observed[w.Kind] || w.Kind == "ksvc" || w.Kind == "group") && !excluded[w.Namespace] && matchesSelector(w.Labels) {
			filtered = append(filtered, w)
		}
	}
	workloads = filtered

	nsSet := map[string]bool{}
	for _, w := range workloads {
		nsSet[w.Namespace] = true
	}

	// One cluster-wide read of pods + replicasets for the whole refresh — instead
	// of listing per namespace (O(1) vs O(namespaces) load on the API server).
	snap, err := e.k8s.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	// Sidecars injected at admission (istio-proxy, linkerd-proxy, …) are not in
	// the template: add what the running pods actually carry, marked
	// InTemplate=false so no rollout ever patches them into the template.
	containers := map[string]map[string]k8s.ContainerClass{}
	meta := map[string]workloadMeta{}
	for i := range workloads {
		w := &workloads[i]
		w.Containers = MergeObservedContainers(*w, snap.Running[w.Namespace])
		cs := map[string]k8s.ContainerClass{}
		for _, p := range snap.Running[w.Namespace] {
			if p.Owner.Kind == w.Kind && p.Owner.Name == w.Name {
				for _, c := range p.Containers {
					cs[c.Name] = c.Class // init/ephemeral too: SetContainer rejects them by class
				}
			}
		}
		for _, c := range w.Containers {
			cs[c.Name] = c.Class
		}
		containers[w.Namespace+"/"+w.Kind+"/"+w.Name] = cs
		meta[w.Namespace+"/"+w.Kind+"/"+w.Name] = workloadMeta{labels: w.Labels, annotations: w.Annotations}
	}

	end := e.clock()
	start := end.Add(-e.cfg.Window)
	halfLife := e.cfg.HalfLife.Seconds()
	inScope := map[string]bool{} // containers still in scope (stabilization history to keep)
	for _, w := range workloads {
		for _, c := range w.Containers {
			inScope[Key(w.Namespace, w.Kind, w.Name, c.Name)] = true
		}
	}

	var list []Recommendation
	byKey := map[string]Entry{}
	if e.cfg.OOMLearn {
		e.seedOOM(workloads) // rebuild floors persisted before a restart/failover
		changed := e.learnOOM(snap.OOM)
		e.persistOOM(ctx, workloads, changed, func(w k8s.Workload) bool {
			m := e.pol.Resolve(w.Namespace, w.Labels, w.Annotations).Mode
			return m.InPlace() || m.Webhook()
		})
	}

	e.rememberUnits(snap)
	ownerOf := e.ownerOf(snap)
	ceilings := e.memCeilings(ctx)
	skipped := 0
	for ns := range nsSet {
		cpuByWL, memByWL, thrByWL, err := e.fetchAndCollapse(ctx, ns, start, end, ownerOf)
		if err != nil {
			// One namespace's query failing (e.g. a vmsingle 422 on a very large /
			// high-cardinality namespace) must NOT zero out every recommendation —
			// skip that namespace and keep sizing the rest of the fleet.
			log.Printf("refresh: skipping ns %s: %v", ns, err)
			skipped++
			continue
		}
		// Weekly seasonality needs more than the default 7 days: fetch the longer
		// history once for the namespaces that have a weekly (or both) policy.
		var seasonCPU, seasonMem map[string]map[string][]Sample
		for _, w := range workloads {
			if w.Namespace == ns && weeklySeason(e.pol.Resolve(w.Namespace, w.Labels, w.Annotations).Season) {
				seasonCPU, seasonMem = e.seasonSeries(ctx, ns, end, ownerOf)
				break
			}
		}
		for _, w := range workloads {
			if w.Namespace != ns {
				continue
			}
			wres := e.pol.Resolve(w.Namespace, w.Labels, w.Annotations)
			wlKey := w.Kind + "/" + w.Name
			for _, c := range w.Containers {
				if !c.Class.Resizable() {
					continue // init/ephemeral containers are never sized
				}
				// Per-container policy: class default (an injected sidecar is only
				// recommended, with its own sidecar profile, until opted in), then
				// spec.containers rules, then per-container annotations.
				res := e.pol.ResolveContainer(wres, w.Annotations, c.Name, string(c.Class))
				if res.Excluded {
					continue
				}
				// Size every container from its OWN usage series — a proxy scales
				// with requests/connections, never with the app's numbers.
				cpu := cpuByWL[wlKey][c.Name]
				mem := memByWL[wlKey][c.Name]
				if len(cpu) == 0 && len(mem) == 0 {
					continue
				}
				key := Key(ns, w.Kind, w.Name, c.Name)
				rec := e.recommendStabilized(key, RecommendInput{
					Workload: w, Container: c, CPU: cpu, Mem: mem, Policy: res,
					SeasonCPU: seasonCPU[wlKey][c.Name], SeasonMem: seasonMem[wlKey][c.Name],
					CPUThrottled: thrByWL[wlKey][c.Name], LiveCPULimit: liveCPULimit(snap.Running[ns], w.Kind, w.Name, c.Name),
					CPULimitGrowth: e.cfg.CPULimitGrowth, CPUCensorMaxFactor: e.cfg.CPUCensorMaxFactor,
					CPUCensorMaxSteps: e.cfg.CPUCensorMaxSteps, CPUCensorAnchor: e.censorAnchorOf(key),
					MemCeiling:  ceilings.For(ns),
					HalfLifeSec: halfLife, OOMFloor: e.oomFloorFor(ns + "/" + wlKey + "/" + c.Name), OOMGrowth: e.cfg.OOMGrowthFactor,
					OOMMemCapFactor: e.cfg.OOMMemCapFactor,
					Now:             end, MinSamples: e.cfg.MinSamples,
					PriceCPUCoreHr: e.price.CPUCoreHr(), PriceGBHr: e.price.GBHr(),
				})
				liveLim := liveCPULimit(snap.Running[ns], w.Kind, w.Name, c.Name)
				if liveLim <= 0 {
					liveLim = c.CPULimit
				}
				e.trackCensor(key, rec, liveLim)
				// Waste + status reflect what's ACTUALLY running (live pods), not the
				// stale template: an in-place resize lands on the pod, so a resized
				// workload reads "right-sized" even while its spec still shows the old
				// request. SpecDrift flags that the change isn't yet persisted to the spec.
				if acpu, amem, ok := liveRequest(snap.Running[ns], w.Kind, w.Name, c.Name); ok {
					rec.Applied = Resources{CPURequest: acpu, CPULimit: rec.Current.CPULimit, MemRequest: amem, MemLimit: rec.Current.MemLimit}
					rec.WasteUSDWeek = round2(((acpu-rec.Recommended.CPURequest)*e.price.CPUCoreHr() +
						((amem-rec.Recommended.MemRequest)/gb)*e.price.GBHr()) * config.HoursPerWeek * float64(w.Replicas))
					rec.SpecDrift = drifted(rec.Current.CPURequest, acpu) || drifted(rec.Current.MemRequest, amem)
				} else {
					rec.Applied = rec.Current
				}
				rec.ContainerClass = string(c.Class)
				rec.NativeSidecar = c.Native
				rec.InTemplate = c.InTemplate
				rec.Mesh = c.Mesh
				rec.ContainerPolicy = res.ContainerPolicy
				rec.ProxyAnnotations = res.ProxyAnnotations
				rec.ActuationPath = actuationPath(w, c, res)
				list = append(list, rec)
				byKey[key] = Entry{
					Rec:                   rec,
					CPUTrend:              aggregateByTime(cpu),
					MemTrend:              aggregateByTime(mem),
					Mode:                  res.Mode,
					Apply:                 res.Apply,
					Direction:             res.Direction,
					OOMOverridesDirection: res.OOMOverridesDirection,
					PolicyName:            res.PolicyName,
					ScaleUpCooldown:       res.ScaleUpCooldown,
					ScaleDownCooldown:     res.ScaleDownCooldown,
					EnforceMemLimit:       res.Mem.EnforceLimit,
					GuaranteedResize:      res.Guaranteed.ResizeBoth(),
					Class:                 c.Class,
					Native:                c.Native,
					NotInTemplate:         !c.InTemplate,
					Mesh:                  c.Mesh,
					ProxyAnnotations:      res.ProxyAnnotations,
				}
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].WasteUSDWeek > list[j].WasteUSDWeek })
	// Every namespace query failed (the metrics backend is down): that is not an
	// empty fleet. Keep what the engine knew (and any warm-cache entries a start-up
	// loaded) instead of replacing it with nothing; the caller logs the error and the
	// next refresh tries again. One failing namespace among others still only skips it.
	if skipped > 0 && skipped == len(nsSet) {
		return fmt.Errorf("refresh: all %d namespace queries failed; keeping the previous recommendations", skipped)
	}
	e.stab.retain(inScope)
	e.censorMu.Lock()
	for k := range e.censorAnchor {
		if !inScope[k] {
			delete(e.censorAnchor, k)
		}
	}
	e.censorMu.Unlock()

	e.mu.Lock()
	e.list = list
	e.byKey = byKey
	e.containers = containers
	e.meta = meta
	e.warmEntries = nil // the real recommendations replace anything loaded from the warm cache
	e.mu.Unlock()
	if skipped > 0 {
		log.Printf("refresh: %d recommendations (%d namespace(s) skipped on query error)", len(list), skipped)
	}
	return nil
}

// fetchAndCollapse queries cpu rate and mem working set for a namespace and
// regroups pod-level series onto workloads via ownerOf (live pods, plus pods
// that no longer exist but are attributable by name: their history is the
// workload's history). Returns workloadKey -> container -> concatenated
// samples (across pods).
//
// The third map is the OPTIONAL CFS throttled-period ratio per workload
// container (cAdvisor container_cpu_cfs_throttled_periods_total /
// container_cpu_cfs_periods_total, scrape job cfg.VMThrottleJob). Without those
// series the query returns nothing and there is no signal. A failing throttle query never fails the
// namespace — it is dropped (missing data is no signal, never zero).
func (e *Engine) fetchAndCollapse(ctx context.Context, ns string, start, end time.Time, ownerOf func(ns, pod string) (k8s.Owner, bool)) (
	map[string]map[string][]Sample, map[string]map[string][]Sample, map[string]map[string][]Sample, error,
) {
	// Pinned to the kubelet scrape job (cfg.VMJob): only the kubelet's series
	// may drive a recommendation.
	cpuExpr, memExpr := e.usageExprs(ns)
	cpuSeries, err := e.vm.QueryRange(ctx, cpuExpr, start, end, time.Time{}, stepSec)
	if err != nil {
		return nil, nil, nil, err
	}
	memSeries, err := e.vm.QueryRange(ctx, memExpr, start, end, time.Time{}, stepSec)
	if err != nil {
		return nil, nil, nil, err
	}
	// Throttling (optional): share of CFS periods throttled in each 5m step.
	// The cAdvisor CFS counters come from their own scrape job (VMThrottleJob,
	// /metrics/cadvisor), pinned like the usage series: another job's series with
	// the same labels must not fake (or hide) throttling.
	thrSel := vm.WithJob(fmt.Sprintf(`{namespace=%q,container!="",container!="POD"}`, ns), e.cfg.VMThrottleJob)
	thrExpr := "sum by (namespace, pod, container) (increase(container_cpu_cfs_throttled_periods_total" + thrSel + "[5m]))" +
		" / sum by (namespace, pod, container) (increase(container_cpu_cfs_periods_total" + thrSel + "[5m]))"
	thrSeries, terr := e.vm.QueryRange(ctx, thrExpr, start, end, time.Time{}, stepSec)
	if terr != nil {
		log.Printf("refresh: ns %s: CPU throttling query failed, no throttling signal this refresh: %v", ns, terr)
		thrSeries = nil
	}

	return foldSeries(ns, cpuSeries, ownerOf), foldSeries(ns, memSeries, ownerOf), foldSeries(ns, thrSeries, ownerOf), nil
}

// usageExprs are the two usage queries of a namespace (CPU rate, memory working
// set), pinned to the kubelet scrape job (cfg.VMJob): only the kubelet's series
// may drive a recommendation.
func (e *Engine) usageExprs(ns string) (cpuExpr, memExpr string) {
	sel := vm.WithJob(fmt.Sprintf(`{namespace=%q,container!="",container!="POD"}`, ns), e.cfg.VMJob)
	// CPU rate: rate() (avg, smooths bursts) by default; rollup_rate(...,"max")
	// captures sub-window micro-bursts (VictoriaMetrics-only) when CPU_RATE_MODE=max.
	cpuExpr = "rate(container_cpu_usage_seconds_total" + sel + "[5m])"
	if e.cfg.CPURateMode == "max" {
		cpuExpr = `rollup_rate(container_cpu_usage_seconds_total` + sel + `[5m], "max")`
	}
	// Memory: the MAX of each 5m step (max_over_time), not the instantaneous value
	// at the step boundary — at step=300s a plain query samples one scrape in
	// ~10-20 and misses short peaks, exactly the ones that OOMKill. Deliberately
	// conservative for memory (a miss is a kill; for CPU a miss is throttling).
	memExpr = "max_over_time(container_memory_working_set_bytes" + sel + "[5m])"
	return cpuExpr, memExpr
}

// foldSeries regroups pod-level series onto workloads: workloadKey -> container ->
// samples concatenated across pods.
func foldSeries(ns string, series []vm.Series, ownerOf func(ns, pod string) (k8s.Owner, bool)) map[string]map[string][]Sample {
	out := map[string]map[string][]Sample{}
	for _, s := range series {
		pod := s.Metric["pod"]
		container := s.Metric["container"]
		if pod == "" || container == "" {
			continue
		}
		owner, ok := ownerOf(ns, pod) // cluster-wide snapshot
		if !ok {
			continue
		}
		wlKey := owner.Kind + "/" + owner.Name
		if out[wlKey] == nil {
			out[wlKey] = map[string][]Sample{}
		}
		for _, p := range s.Samples {
			out[wlKey][container] = append(out[wlKey][container], Sample{T: p.T, V: p.V})
		}
	}
	return out
}

// stabSeedMax bounds the extra Recommend calls made to seed the stabilization
// history of a container the engine has no history for (first refresh after a
// start/failover): one per query step inside the window, at most this many.
const stabSeedMax = 12

// recommendStabilized runs Recommend for key with the scale-down stabilization
// floor (HPA semantics) and records the raw result. When the engine holds no
// history for key — the first refresh after a restart or failover, or a new
// container — the history is seeded first by replaying Recommend (pure) at the
// past query steps inside the stabilization window, on the series truncated to
// each instant: what a process that had been running would have recorded.
// Without the seed the first window published the raw recommendation, so a
// restart could let a transient dip shrink capacity the window was holding.
func (e *Engine) recommendStabilized(key string, in RecommendInput) Recommendation {
	w := in.Policy.ScaleDownStabilization
	if w > 0 && !e.stab.has(key) {
		var steps []time.Time
		for k := 1; k <= stabSeedMax; k++ {
			t := in.Now.Add(-time.Duration(k*stepSec) * time.Second)
			if in.Now.Sub(t) >= w {
				break
			}
			steps = append(steps, t)
		}
		for i := len(steps) - 1; i >= 0; i-- { // oldest first: record prunes by its own now
			t := steps[i]
			past := in
			past.Now, past.StabilizeCPU, past.StabilizeMem = t, 0, 0
			past.CPU, past.Mem, past.CPUThrottled = samplesUpTo(in.CPU, t), samplesUpTo(in.Mem, t), samplesUpTo(in.CPUThrottled, t)
			past.SeasonCPU, past.SeasonMem = samplesUpTo(in.SeasonCPU, t), samplesUpTo(in.SeasonMem, t)
			if len(past.CPU) == 0 && len(past.Mem) == 0 {
				continue
			}
			r := Recommend(past)
			e.stab.record(key, t, w, r.Signals.RawCPURequest, r.Signals.RawMemRequest)
		}
	}
	in.StabilizeCPU, in.StabilizeMem = e.stab.floor(key, in.Now, w)
	rec := Recommend(in)
	e.stab.record(key, in.Now, w, rec.Signals.RawCPURequest, rec.Signals.RawMemRequest)
	return rec
}

// samplesUpTo returns the samples at or before t (order preserved).
func samplesUpTo(s []Sample, t time.Time) []Sample {
	if len(s) == 0 {
		return nil
	}
	lim := float64(t.Unix())
	out := make([]Sample, 0, len(s))
	for _, x := range s {
		if x.T <= lim {
			out = append(out, x)
		}
	}
	return out
}

// changePointIndex finds the start of the CURRENT usage regime in a
// TIME-ORDERED series with one sample per timestamp: the latest sustained level
// shift. It scans candidate splits from the most-recent side; if the mean AFTER a
// split differs from the mean BEFORE by at least `rel` (relative), that split is
// the change point. Returns 0 (use all) when the series is short or steady.
// O(n) via prefix sums (the per-split means used to be recomputed: O(n²)).
func changePointIndex(s []Sample, rel float64) int {
	n := len(s)
	if n < 24 {
		return 0
	}
	minSeg := n / 4
	if minSeg < 8 {
		minSeg = 8
	}
	prefix := make([]float64, n+1)
	for i, x := range s {
		prefix[i+1] = prefix[i] + x.V
	}
	for i := n - minSeg; i >= minSeg; i-- {
		before := prefix[i] / float64(i)
		after := (prefix[n] - prefix[i]) / float64(n-i)
		base := before
		if base < 1e-9 {
			base = 1e-9
		}
		if math.Abs(after-before)/base >= rel {
			return i
		}
	}
	return 0
}

// meanByTime collapses samples that share a timestamp (one per replica) into
// their mean and sorts by time — the per-replica-average series of a workload.
// Workload samples arrive concatenated per pod, NOT time-ordered, so anything
// that reasons about "before/after" or "latest" must go through this first.
func meanByTime(samples []Sample) []Sample {
	if len(samples) == 0 {
		return nil
	}
	type acc struct{ sum, n float64 }
	byT := make(map[float64]*acc, len(samples))
	for _, s := range samples {
		a := byT[s.T]
		if a == nil {
			a = &acc{}
			byT[s.T] = a
		}
		a.sum += s.V
		a.n++
	}
	out := make([]Sample, 0, len(byT))
	for t, a := range byT {
		out = append(out, Sample{T: t, V: a.sum / a.n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// currentRegime keeps only the samples (from every replica) at or after the
// latest level shift of the workload's per-timestamp mean. A boundary between
// two replicas in the concatenated input is therefore never mistaken for a
// change point, and replicas running at different steady levels all stay in.
//
// The scan runs on a running median of that series (±cpFilterHalfWidth samples,
// ~1h): a level shift must persist for more than half the filter window to be
// seen at all, and then for a quarter of the window (changePointIndex minSeg) to
// be cut on. A burst of a few minutes raises the MEAN of the recent segment but
// not its median, which is what made the window collapse onto a burst before.
func currentRegime(s []Sample, rel float64) []Sample {
	m := meanByTime(s)
	i := changePointIndex(medianFilter(m, cpFilterHalfWidth), rel)
	if i == 0 {
		return s
	}
	cut := m[i].T
	out := make([]Sample, 0, len(s))
	for _, x := range s {
		if x.T >= cut {
			out = append(out, x)
		}
	}
	return out
}

// liveRequest returns the max per-container request across the workload's running
// pods (replicas are identical; max is the representative). found=false when no
// live pod is seen yet.
func liveRequest(pods []k8s.RunningPod, kind, name, container string) (cpu, mem float64, found bool) {
	for _, p := range pods {
		if p.Owner.Kind != kind || p.Owner.Name != name {
			continue
		}
		for _, c := range p.Containers {
			if c.Name != container {
				continue
			}
			if c.CPURequest > cpu {
				cpu = c.CPURequest
			}
			if c.MemRequest > mem {
				mem = c.MemRequest
			}
			found = true
		}
	}
	return
}

// liveCPULimit returns the max CPU limit of container across the workload's
// running pods (0 = none seen). An in-place resize moves the pod's limit, not
// the template's, and the usage is capped by the pod's.
func liveCPULimit(pods []k8s.RunningPod, kind, name, container string) float64 {
	var lim float64
	for _, p := range pods {
		if p.Owner.Kind != kind || p.Owner.Name != name {
			continue
		}
		for _, c := range p.Containers {
			if c.Name == container && c.CPULimit > lim {
				lim = c.CPULimit
			}
		}
	}
	return lim
}

// drifted reports a meaningful (>10%) difference between two request values.
func drifted(a, b float64) bool {
	base := a
	if base < 1e-9 {
		base = b
	}
	if base < 1e-9 {
		return false
	}
	return math.Abs(a-b)/base > 0.1
}

func meanV(s []Sample) float64 {
	if len(s) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range s {
		sum += x.V
	}
	return sum / float64(len(s))
}

// RecommendInput is everything Recommend needs: no cluster, no metrics backend,
// no wall clock. CPU / Mem are the workload container's samples concatenated
// across pods (one per pod per scrape).
type RecommendInput struct {
	Workload  k8s.Workload
	Container k8s.Container // current spec requests/limits
	CPU, Mem  []Sample
	Policy    policy.Resolved
	// HalfLifeSec is the engine default decay half-life (seconds); a policy
	// HalfLife overrides it.
	HalfLifeSec float64
	// OOMFloor is the active learned mem-limit floor (0 = none).
	OOMFloor float64
	// OOMGrowth is the OOM growth factor (cfg.OOMGrowthFactor; 0 = 1.0). Used to
	// reserve memory for an OOMKilled container that has NO limit.
	OOMGrowth float64
	// OOMMemCapFactor (cfg.OOMMemCapFactor; 0 = config.DefaultOOMMemCapFactor)
	// bounds that reservation at observedPeak × max(OOMMemCapFactor, OOMGrowth),
	// then the policy memory.max. The learned floor of an unbounded container
	// comes from its request at the kill, so repeated node-pressure kills could
	// otherwise ratchet it ×growth per kill toward a request no node can schedule.
	OOMMemCapFactor float64
	// Now is the evaluation time (end of the window; seasonal forecast target is Now+1h).
	Now        time.Time
	MinSamples int
	// Prices for WasteUSDWeek.
	PriceCPUCoreHr, PriceGBHr float64
	// StabilizeCPU / StabilizeMem: scale-down stabilization floors — the max RAW
	// request recommended within the policy's ScaleDownStabilization window
	// (0 = none). The request never drops below them (HPA-style).
	StabilizeCPU, StabilizeMem float64

	// Censored CPU inputs (see cpuCensoring). LiveCPULimit is the CPU limit the
	// running pods actually have (max across them; 0 = unknown → the Container's):
	// after an in-place raise the template still shows the old one.
	// CPUThrottled is the optional CFS throttled-period ratio series (nil = the
	// series is absent from the metrics backend). CPULimitGrowth / CPUCensorMaxFactor
	// are cfg.CPULimitGrowth / cfg.CPUCensorMaxFactor (0 = the defaults).
	LiveCPULimit float64
	// SeasonCPU / SeasonMem: the usage over the longer SEASON_WINDOW, fetched only
	// for weekly and both policies. They feed the seasonality gate and the seasonal
	// forecast alone (the quantile, the peak and the change-point keep the WINDOW
	// series); nil, or not longer than CPU/Mem, means those are used.
	SeasonCPU, SeasonMem []Sample
	// CPUCensorMaxSteps (0 = config.DefaultCPUCensorMaxSteps): raises of a censored
	// CPU limit before the container is parked as greedy. CPUCensorAnchor is the
	// limit those raises are counted from: the engine remembers the live limit at
	// the moment it first saw the container censored and forgets it once usage
	// falls below 80% of the limit (0 = no run of raises is being counted).
	CPUCensorMaxSteps int
	CPUCensorAnchor   float64
	// MemCeiling (0 = none): the largest memory request a pod can have here, from
	// the cluster lookups (tailor.rbac.clusterLookups): min(largest eligible node
	// allocatable, namespace LimitRange max). The memory request never exceeds it,
	// so an OOM raise cannot ask for a pod no node can schedule.
	MemCeiling         float64
	CPUThrottled       []Sample
	CPULimitGrowth     float64
	CPUCensorMaxFactor float64
}

// Censored-CPU detection (cpuCensoring). A 5-minute rate of a container held at
// its CFS quota reads at (or a hair above) the limit; >= 90% of it on a quarter
// of the last hour's samples, or a quarter of the CFS periods throttled, means
// the measured usage no longer tells the demand.
const (
	censorLookbackSec = 3600.0
	censorPinnedAt    = 0.9
	censorMinShare    = 0.25
	censorMinSamples  = 6
	throttleMinRatio  = 0.25
)

// autoQuantileTo is the CPU quantile autoQuantile raises a bursty container to
// (the `bursty` preset's value).
const autoQuantileTo = 0.95

// burstMinCoverage: distinct scrape timestamps (12 h of 5-minute steps) needed to
// call an index; pooled replicas count once per timestamp.
const burstMinCoverage = 144

// burstDenomFloor keeps an idle container's p90 (a fraction of a millicore) from
// turning a few 20m blips into a huge ratio.
const burstDenomFloor = 0.005

// burstiness is p99 / p90 of the CPU series (all replicas pooled): how far the tail
// sits above the level the request is sized from. Measured on the backtest
// scenarios (14 days, last 7 days, 3 seeds): bursty 6.5-9.0, heterogeneous 1.7-1.8,
// diurnal-weekly 1.16-1.19, the rest <= 1.2. (p99/p50 and the share of time above
// 2x the median do not separate bursty from diurnal or heterogeneous.) ok=false when
// the window covers less than 12 h.
func burstiness(cpu []Sample) (float64, bool) {
	if len(meanByTime(cpu)) < burstMinCoverage {
		return 0, false
	}
	p90, p99 := quantileOf(cpu, 0.90), quantileOf(cpu, 0.99)
	return p99 / math.Max(p90, burstDenomFloor), true
}

// censorReleaseAt: a parked (greedy) container is released once fewer than a
// quarter of the last hour's samples sit at or above 80% of its limit.
const censorReleaseAt = 0.8

// censorStepsOf is how many growth steps the live limit is above its anchor:
// floor(log_growth(live/anchor)). Each raise multiplies by growth and rounds
// UP to 1m, so after k raises the ratio is a hair above growth^k and never
// reaches the next power. 0 when either is unknown or growth <= 1.
func censorStepsOf(live, anchor, growth float64) int {
	if live <= 0 || anchor <= 0 || growth <= 1 || live <= anchor {
		return 0
	}
	return int(math.Floor(math.Log(live/anchor)/math.Log(growth) + 1e-9))
}

// nearLimitShare is the share of the last hour's samples at or above frac of limit.
func nearLimitShare(cpu []Sample, limit, frac float64) float64 {
	latest := math.Inf(-1)
	for _, x := range cpu {
		latest = math.Max(latest, x.T)
	}
	n, near := 0, 0
	for _, x := range cpu {
		if x.T < latest-censorLookbackSec || math.IsNaN(x.V) {
			continue
		}
		n++
		if x.V >= frac*limit {
			near++
		}
	}
	if n < censorMinSamples {
		return 1 // too little to call it a release
	}
	return float64(near) / float64(n)
}

// cpuCensoring returns the share of the last hour's samples at >= 90% of
// limit, the mean throttled-period ratio over the same hour (nil when that
// series is absent or too short: missing data is no signal, never "zero
// throttling"), and whether either crosses its threshold. limit <= 0 (no CPU
// limit) is never censored: nothing caps the usage.
func cpuCensoring(cpu, throttled []Sample, limit float64) (pinned float64, thr *float64, censored bool) {
	if limit <= 0 {
		return 0, nil, false
	}
	recent := func(s []Sample) []Sample {
		latest := math.Inf(-1)
		for _, x := range s {
			latest = math.Max(latest, x.T)
		}
		var out []Sample
		for _, x := range s {
			if x.T >= latest-censorLookbackSec && !math.IsNaN(x.V) {
				out = append(out, x)
			}
		}
		return out
	}
	if r := recent(cpu); len(r) >= censorMinSamples {
		n := 0
		for _, x := range r {
			if x.V >= censorPinnedAt*limit {
				n++
			}
		}
		pinned = float64(n) / float64(len(r))
	}
	if r := recent(throttled); len(r) >= censorMinSamples {
		m := meanV(r)
		thr = &m
	}
	censored = pinned >= censorMinShare || (thr != nil && *thr >= throttleMinRatio)
	return pinned, thr, censored
}

// recommend is the engine wrapper over Recommend (engine clock, config, prices).
func (e *Engine) recommend(w k8s.Workload, c k8s.Container, cpu, mem []Sample, halfLife float64, res policy.Resolved, oomFloor float64) Recommendation {
	return Recommend(RecommendInput{
		Workload: w, Container: c, CPU: cpu, Mem: mem, Policy: res,
		HalfLifeSec: halfLife, OOMFloor: oomFloor, OOMGrowth: e.cfg.OOMGrowthFactor, OOMMemCapFactor: e.cfg.OOMMemCapFactor, Now: e.clock(), MinSamples: e.cfg.MinSamples,
		PriceCPUCoreHr: e.price.CPUCoreHr(), PriceGBHr: e.price.GBHr(),
	})
}

// Recommend computes the recommendation for one workload container. Pure: same
// input → same output.
func Recommend(in RecommendInput) Recommendation {
	w, c, cpu, mem, res, oomFloor := in.Workload, in.Container, in.CPU, in.Mem, in.Policy, in.OOMFloor
	halfLife := in.HalfLifeSec
	cpuM := 1 + res.CPU.Margin
	memM := 1 + res.Mem.Margin

	// Per-policy decay: a shorter half-life reacts faster; longer smooths more.
	if res.HalfLife > 0 {
		halfLife = res.HalfLife.Seconds()
	}
	// Change-point: when usage shifts to a new baseline (a deploy changed the
	// footprint), older samples are a DIFFERENT regime. Trim the quantile/peak to
	// the current regime so a stale-but-recent history doesn't drag the number —
	// the decay alone would forget it too slowly. Seasonal forecast below keeps
	// the FULL series (it needs the long history to see periodicity). The
	// detector runs on a median-filtered series (currentRegime), so a burst of
	// minutes is never mistaken for a new regime.
	cpuCur := currentRegime(cpu, 0.5)
	memCur := currentRegime(mem, 0.5)
	// Burstiness (p99 / p90 of the window): always measured and exposed; with the
	// opt-in autoQuantile it lifts the CPU quantile to 0.95 for bursty containers.
	cpuQuantile := res.CPU.Quantile
	var burstIdx *float64
	burstReason := "burstiness unknown: fewer than 12 h of samples"
	burstRaised := false
	if idx, ok := burstiness(cpu); ok {
		burstIdx = &idx
		thr := nonZero(res.BurstThreshold, config.DefaultBurstThreshold)
		switch {
		case !res.AutoQuantile:
			burstReason = fmt.Sprintf("burstiness %.1f (threshold %.1f); autoQuantile is off, cpu quantile stays %.2f", idx, thr, cpuQuantile)
		case idx < thr:
			burstReason = fmt.Sprintf("burstiness %.1f is below the threshold %.1f; cpu quantile stays %.2f", idx, thr, cpuQuantile)
		case cpuQuantile >= autoQuantileTo:
			burstReason = fmt.Sprintf("burstiness %.1f is at or above the threshold %.1f; cpu quantile %.2f is already at least %.2f", idx, thr, cpuQuantile, autoQuantileTo)
		default:
			burstReason = fmt.Sprintf("burstiness %.1f is at or above the threshold %.1f: cpu quantile raised %.2f -> %.2f", idx, thr, cpuQuantile, autoQuantileTo)
			cpuQuantile, burstRaised = autoQuantileTo, true
		}
	}
	cpuQ := WeightedQuantile(cpuCur, cpuQuantile, halfLife, 0)
	cpuPeak := Peak(cpuCur)
	memQ := WeightedQuantile(memCur, res.Mem.Quantile, halfLife, 0)
	memPeak := Peak(memCur)

	// Seasonal forecast: anticipate the recurring peak ~1h ahead. The season is
	// configurable per policy: "daily" (hour-of-day), "weekly" (hour-of-week — the
	// pattern that matters for site traffic / Knative scale-from-zero), or "both"
	// (take the higher of the two, so we never under-provision ahead of a ramp).
	// The forecast is only trusted when the data is actually periodic
	// (effectiveSeason): with random bursts the same-hour bucket "lights up" or
	// not depending on where past bursts happened to land — chasing noise.
	now := float64(in.Now.Unix())
	hr := time.Hour.Seconds()
	// Weekly and both judge and forecast from the longer SEASON_WINDOW series when
	// the engine supplied one: a week-lag correlation needs more than 7 days.
	seasonCPU, seasonMem := cpu, mem
	if weeklySeason(res.Season) {
		if len(in.SeasonCPU) > len(cpu) {
			seasonCPU = in.SeasonCPU
		}
		if len(in.SeasonMem) > len(mem) {
			seasonMem = in.SeasonMem
		}
	}
	cpuSeason := effectiveSeasonGate(seasonCPU, res.Season, res.SeasonMinCorrelation, res.SeasonMinPairs)
	memSeason := effectiveSeasonGate(seasonMem, res.Season, res.SeasonMinCorrelation, res.SeasonMinPairs)
	cpuFc := seasonForecast(seasonCPU, cpuQuantile, cpuSeason, now+hr)
	memFc := seasonForecast(seasonMem, res.Mem.Quantile, memSeason, now+hr)

	// clamp to [min, max] (max 0 = unbounded); mem also floored at observed peak (anti-OOM);
	// both lifted by the seasonal forecast so we scale up ahead of a known ramp.
	reqCPU := clamp(max(cpuQ*cpuM, cpuFc*cpuM), res.CPU.Min, res.CPU.Max)
	reqMem := clamp(max(max(memQ*memM, memPeak), memFc*memM), res.Mem.Min, res.Mem.Max)

	// Headroom guard (opt-in, default off): floor the request at the (1-threshold)
	// quantile of the FULL window, so at most `threshold` of samples exceed it — i.e.
	// the headroom breach stays <= threshold by construction. Bounded by Max (a tight
	// Max can still leave breach above threshold, which the breach signal surfaces).
	if res.HeadroomGuard {
		t := res.HeadroomThreshold
		if t <= 0 {
			t = 0.02
		}
		reqCPU = clamp(max(reqCPU, quantileOf(cpu, 1-t)), res.CPU.Min, res.CPU.Max)
		reqMem = clamp(max(reqMem, quantileOf(mem, 1-t)), res.Mem.Min, res.Mem.Max)
	}

	// Scale-down stabilization (HPA semantics): the raw value is what the engine
	// would recommend from this window alone; the published request is never
	// below the highest raw recommendation of the last ScaleDownStabilization
	// window, so a transient dip can't shrink capacity that is needed again
	// minutes later. Scale-ups pass through at once (the floor is a max).
	// No series at all for a resource: nothing to size it from — keep the current
	// request (never fall to the policy minimum blind).
	if len(cpu) == 0 && c.CPURequest > 0 {
		reqCPU = c.CPURequest
	}
	if len(mem) == 0 && c.MemRequest > 0 {
		reqMem = c.MemRequest
	}
	rawCPU, rawMem := reqCPU, reqMem
	stabilized := false
	if in.StabilizeCPU > reqCPU {
		reqCPU, stabilized = clamp(in.StabilizeCPU, res.CPU.Min, res.CPU.Max), true
	}
	if in.StabilizeMem > reqMem {
		reqMem, stabilized = clamp(in.StabilizeMem, res.Mem.Min, res.Mem.Max), true
	}

	// A learned OOM floor means this container was OOMKilled recently; the durable
	// protection is applied to the LIMIT after the caps below (a limit raise, not a
	// request inflation), so tight request-based node packing is preserved.
	oomRisk := oomFloor > 0
	memCeilingCapped := false

	// Health cap: unless the policy opts in (allowExceedLimit), never set a request
	// above the currently-configured limit — keeps the workload from ballooning
	// past its declared ceiling.
	// The CPU cap reads the LIVE limit when known: after an in-place limit
	// raise (censored CPU below) the template still carries the old one, and
	// capping at it would pull the request back under what the pod already runs.
	effCPULim := c.CPULimit
	if in.LiveCPULimit > 0 {
		effCPULim = in.LiveCPULimit
	}
	limitCapped := false
	// A Guaranteed-shaped container under guaranteed: resizeBoth moves its limit
	// with its request, so the current limit is no ceiling for it.
	guar := res.Guaranteed.ResizeBoth() && c.GuaranteedShape()
	allowExceed := res.AllowExceedLimit || guar
	if !allowExceed {
		if c.MemLimit > 0 && reqMem > c.MemLimit {
			reqMem = c.MemLimit
			limitCapped = true // wanted more memory than the current limit allows
		}
		if effCPULim > 0 && reqCPU > effCPULim {
			reqCPU = effCPULim
		}
	}
	reqCPU = RoundCPUCores(reqCPU)
	reqMem = RoundMemBytes(reqMem)

	rec := Resources{CPURequest: reqCPU, MemRequest: reqMem}
	// Memory limit: only re-derived for a container that ALREADY has one — a
	// limit factor must never impose an OOM ceiling where the owner chose none —
	// unless the policy explicitly opts in (memory.enforceLimit). A derived limit
	// also clears the observed peak with the same margin as the request:
	// limit == request == peak would OOMKill on the next peak.
	if c.MemLimit > 0 || (res.Mem.EnforceLimit && len(mem) > 0) {
		lim := reqMem * nonZero(res.Mem.LimitFactor, 1.0)
		if pk := memPeak * memM; lim < pk {
			lim = pk
		}
		if lim < reqMem { // limitFactor < 1: never below the request
			lim = reqMem
		}
		rec.MemLimit = RoundMemBytes(lim)
	}
	if res.CPU.LimitFactor > 0 {
		// limitFactor < 1 would put the limit below the request: invalid.
		rec.CPULimit = RoundCPUCores(math.Max(reqCPU*res.CPU.LimitFactor, reqCPU))
	}
	// Don't let the recommended LIMIT exceed the current one either, unless opted in.
	if !allowExceed {
		if c.MemLimit > 0 && rec.MemLimit > c.MemLimit {
			rec.MemLimit = c.MemLimit
		}
		if effCPULim > 0 && rec.CPULimit > effCPULim {
			rec.CPULimit = effCPULim
		}
	}

	// Censored CPU (after the caps above): the
	// usage is pinned at the live CPU limit (or the kernel throttles), so the
	// quantile above estimates the LIMIT, not the demand — the recommendation
	// would sit at ~the limit forever. The demand is provably >= the live limit:
	// lift the request to it and raise the limit one growth step, so the next
	// window measures past it. Bounded by the policy cpu.max, or without one by
	// the template limit × CPUCensorMaxFactor; a limit is never lowered.
	cpuPinned, cpuThrottled, cpuCensored := cpuCensoring(cpu, in.CPUThrottled, effCPULim)
	// Convergence: a container whose demand has no end (a busy loop) eats every
	// raise at once. Count the raises the live limit already is above its anchor;
	// at CPUCensorMaxSteps with usage still pinned the limit is parked, not raised
	// further, and the container is marked greedy until usage falls below 80% of it.
	growth := nonZero(in.CPULimitGrowth, config.DefaultCPULimitGrowth)
	censorSteps := censorStepsOf(effCPULim, in.CPUCensorAnchor, growth)
	maxSteps := in.CPUCensorMaxSteps
	if maxSteps <= 0 {
		maxSteps = config.DefaultCPUCensorMaxSteps
	}
	cpuGreedy := cpuCensored && growth > 1 && censorSteps >= maxSteps
	cpuGreedyRelease := !cpuCensored && censorSteps >= 1 && effCPULim > 0 && nearLimitShare(cpu, effCPULim, censorReleaseAt) < censorMinShare
	if cpuCensored {
		lim := effCPULim * growth
		if cpuGreedy {
			lim = effCPULim // parked: the request still follows the live limit below
		}
		// Ceiling: the policy cpu.max when set (the operator's bound), else the
		// template limit × CPUCensorMaxFactor — a brake for a CPU-greedy container
		// whose demand has no end (a busy loop is always "censored").
		ceiling := res.CPU.Max
		if ceiling <= 0 && c.CPULimit > 0 {
			ceiling = c.CPULimit * nonZero(in.CPUCensorMaxFactor, config.DefaultCPUCensorMaxFactor)
		}
		if ceiling > 0 && lim > ceiling {
			lim = ceiling
		}
		// Round to the microcore first: 0.05×1.5 is 0.07500000000000001 in
		// float64 and must not ceil to 76m.
		lim = math.Max(RoundCPUCores(math.Round(lim*1e6)/1e6), effCPULim)
		reqCPU = clamp(math.Max(reqCPU, effCPULim), res.CPU.Min, res.CPU.Max)
		if reqCPU > lim {
			lim = reqCPU
		}
		rec.CPURequest = RoundCPUCores(reqCPU)
		reqCPU = rec.CPURequest
		if lim > rec.CPULimit {
			rec.CPULimit = lim
		}
	}

	// Learned OOM floor (safety override): re-protect the memory LIMIT durably at
	// the value that killed the container × growth, regardless of AllowExceedLimit
	// or a Down policy — OOM safety is never traded for savings. Applied AFTER the
	// caps so it can raise the limit above the current (killer) value. Expires after
	// OOMFloorTTL (see Engine.learnOOM). The request stays tight; keep the k8s
	// request<=limit invariant.
	if oomRisk {
		if c.MemLimit > 0 || rec.MemLimit > 0 {
			if rec.MemLimit < oomFloor {
				rec.MemLimit = RoundMemBytes(oomFloor)
			}
			// request<=limit: raise the LIMIT, never lower the request (it is already
			// floored at the observed peak — cutting it would starve the container).
			if rec.MemRequest > rec.MemLimit {
				rec.MemLimit = rec.MemRequest
			}
		} else {
			// No memory limit: nothing in the pod capped it — the kill came from node
			// memory pressure. Never INTRODUCE a limit (one derived from the request
			// would sit below real usage and turn a rare kill into an OOM loop);
			// reserve the memory instead: request >= max(learned floor, peak × growth).
			// NEVER derived from the live c.MemRequest: after the first raise that is
			// the Tailor's own output, and feeding it back compounded ×growth on every
			// refresh with no new OOM. The floor already encodes request-at-kill ×
			// growth, learned once per kill (finishedAt dedupe in learnOOM), so this is
			// a fixed point between kills.
			g := nonZero(in.OOMGrowth, 1.0)
			want := math.Max(oomFloor, memPeak*g)
			// Ceiling: a multiple of the observed peak (OOM_MEM_CAP_FACTOR, default 4;
			// repeated node-pressure kills each relearn from an already-raised
			// request), then the policy memory.max — the lower of the two wins. Never
			// below peak × g (config validation also enforces cap >= growth).
			if memPeak > 0 {
				capF := nonZero(in.OOMMemCapFactor, config.DefaultOOMMemCapFactor)
				if cp := memPeak * math.Max(capF, g); want > cp {
					want = cp
				}
			}
			if res.Mem.Max > 0 && want > res.Mem.Max {
				want = res.Mem.Max
			}
			if in.MemCeiling > 0 && want > in.MemCeiling {
				want, memCeilingCapped = in.MemCeiling, true
			}
			if want > rec.MemRequest {
				rec.MemRequest = RoundMemBytes(want)
				reqMem = rec.MemRequest
			}
		}
	}
	// Schedulable ceiling (cluster lookups): last word on the request, after the
	// OOM floor above. A limit may sit higher (a limit does not schedule), but never
	// below the request, and a Guaranteed container keeps request == limit.
	if in.MemCeiling > 0 {
		ceil := math.Floor(in.MemCeiling)
		if rec.MemRequest > ceil {
			rec.MemRequest, reqMem, memCeilingCapped = ceil, ceil, true
			if rec.MemLimit > 0 && rec.MemLimit < rec.MemRequest {
				rec.MemLimit = rec.MemRequest
			}
		}
	}
	// guaranteed: resizeBoth — request and limit are ONE value, so the pod keeps
	// its QoS class when the actuation patches both. Memory takes the larger of the
	// two (the limit already clears the observed peak with the margin and carries
	// the OOM floor), bounded by memory.max unless an OOM floor is active. CPU takes
	// the request; a censored container takes the grown limit, which is the step
	// that lets the next window measure past the old ceiling.
	if guar {
		mv := math.Max(rec.MemRequest, rec.MemLimit)
		if res.Mem.Max > 0 && mv > res.Mem.Max && !oomRisk {
			mv = res.Mem.Max
		}
		cv := rec.CPURequest
		if cpuCensored {
			cv = math.Max(rec.CPURequest, rec.CPULimit)
		}
		rec.CPURequest, rec.CPULimit = RoundCPUCores(cv), RoundCPUCores(cv)
		rec.MemRequest, rec.MemLimit = RoundMemBytes(mv), RoundMemBytes(mv)
		if in.MemCeiling > 0 && rec.MemRequest > math.Floor(in.MemCeiling) {
			c := math.Floor(in.MemCeiling)
			rec.MemRequest, rec.MemLimit, memCeilingCapped = c, c, true
		}
		reqCPU, reqMem = rec.CPURequest, rec.MemRequest
	}
	// Confidence = window COVERAGE (distinct scrape timestamps), not samples
	// pooled across replicas: 40 replicas seen in one scrape is 1 point in time.
	// Per resource: each one is actuated on its OWN coverage only.
	memT := meanByTime(mem)
	need := maxInt(in.MinSamples, 1)
	cpuConfident := len(meanByTime(cpu)) >= need
	memConfident := len(memT) >= need
	confident := cpuConfident || memConfident

	// 24 h memory trend (display + tailor_predicted_mem_bytes): validated —
	// enough points, a real linear fit, horizon capped at the observed span.
	var trend *float64
	if p, ok := TrendForecast(memT, (24 * time.Hour).Seconds()); ok {
		trend = &p
	}
	var cpuFcP, memFcP *float64
	if cpuFc > 0 {
		cpuFcP = &cpuFc
	}
	if memFc > 0 {
		memFcP = &memFc
	}

	cpuDelta := c.CPURequest - reqCPU
	memDeltaGB := (c.MemRequest - reqMem) / gb
	waste := (cpuDelta*in.PriceCPUCoreHr + memDeltaGB*in.PriceGBHr) * config.HoursPerWeek * float64(w.Replicas)

	// Headroom breach: the SLI share_gt_over_time — the fraction of window samples
	// that would exceed the recommended request. ~0 = the rec safely covers usage;
	// high = the rec is too tight (throttle/OOM risk). Computed over the samples we
	// already fetched (no extra VM query — matters at fleet scale). Uses the FULL
	// window series (not the change-point-trimmed cpuCur/memCur): headroom is a
	// whole-window property.
	breachCPU := shareGt(cpu, reqCPU)
	breachMem := shareGt(mem, reqMem)

	return Recommendation{
		Namespace: w.Namespace, Workload: w.Name, Kind: w.Kind, Container: c.Name, Replicas: w.Replicas,
		Current: Resources{
			CPURequest: c.CPURequest, CPULimit: c.CPULimit,
			MemRequest: c.MemRequest, MemLimit: c.MemLimit,
		},
		Recommended:  rec,
		Mode:         string(res.Mode),
		Apply:        string(res.Apply),
		Direction:    string(res.Direction),
		Policy:       res.PolicyName,
		Profile:      res.Profile,
		Automated:    res.Mode.Webhook() || res.Mode.InPlace(),
		Overridden:   res.Overridden,
		Managed:      w.Kind != "Pod",
		Strategy:     strategyFor(w),
		PDBName:      w.PDBName,
		PDBProtected: w.PDBProtected,
		PDBAllowed:   w.PDBAllowed,
		Autoscaler:   w.Autoscaler,
		Knative:      w.Knative,
		CPUHeldByHPA: w.Autoscaler.ManagesCPU,
		MemHeldByHPA: w.Autoscaler.ManagesMem,
		Confident:    confident,
		CPUConfident: cpuConfident,
		MemConfident: memConfident,
		OOMRisk:      oomRisk,
		LimitCapped:  limitCapped,
		Signals: Signals{
			CPUQuantile: cpuQ, CPUPeak: cpuPeak, CPUForecast: cpuFcP,
			MemQuantile: memQ, MemPeak: memPeak, MemForecastBytes: memFcP,
			MemTrendBytes: trend, Samples: maxInt(len(cpu), len(mem)),
			HeadroomBreachCPU: breachCPU, HeadroomBreachMem: breachMem,
			RawCPURequest: rawCPU, RawMemRequest: rawMem, Stabilized: stabilized,
			CPUSeason: cpuSeason, MemSeason: memSeason,
			CPUPinnedShare: cpuPinned, CPUThrottledShare: cpuThrottled, CPUCensored: cpuCensored,
			MemCeiling: in.MemCeiling, MemCeilingCapped: memCeilingCapped,
			Burstiness: burstIdx, CPUQuantileUsed: cpuQuantile, BurstQuantileRaised: burstRaised, BurstReason: burstReason,
			CPUGreedy: cpuGreedy, CPUCensorSteps: censorSteps, CPUGreedyRelease: cpuGreedyRelease,
		},
		WasteUSDWeek: round2(waste),
	}
}

// quantileOf is a plain (unweighted) phi-quantile over the samples' values —
// matches the basis shareGt measures against, so flooring the request at
// quantileOf(s, 1-t) guarantees shareGt(s, request) <= t.
func quantileOf(s []Sample, phi float64) float64 {
	if len(s) == 0 {
		return 0
	}
	vals := make([]float64, len(s))
	for i, x := range s {
		vals[i] = x.V
	}
	sort.Float64s(vals)
	if phi <= 0 {
		return vals[0]
	}
	if phi >= 1 {
		return vals[len(vals)-1]
	}
	idx := int(phi * float64(len(vals)-1))
	return vals[idx]
}

// shareGt is the MetricsQL share_gt_over_time computed locally: the fraction of
// samples strictly greater than gt (0..1). Empty series => 0.
func shareGt(s []Sample, gt float64) float64 {
	if len(s) == 0 {
		return 0
	}
	n := 0
	for _, x := range s {
		if x.V > gt {
			n++
		}
	}
	return float64(n) / float64(len(s))
}

// aggregateByTime sums samples sharing a timestamp (across pods) and sorts by time.
func aggregateByTime(samples []Sample) []Sample {
	if len(samples) == 0 {
		return nil
	}
	byT := map[float64]float64{}
	for _, s := range samples {
		byT[s.T] += s.V
	}
	out := make([]Sample, 0, len(byT))
	for t, v := range byT {
		out = append(out, Sample{T: t, V: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// clamp raises v to at least min, then caps at max (max <= 0 means unbounded).
func clamp(v, min, max float64) float64 {
	if v < min {
		v = min
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

func nonZero(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

func round2(f float64) float64 { return float64(int64(f*100)) / 100 }
