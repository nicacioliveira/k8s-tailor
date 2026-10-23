package reconciler

import (
	"math"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// Plan is the in-place decision for ONE running container, before any side
// effect (API patch, Event, cooldown bookkeeping). reconcileNS builds it with
// PlanInPlace and then applies the pod-level guards (pending resize, QoS,
// failure backoff, PDB); Decide is the same decision for the common case
// (Burstable pod, no pending resize, no PDB block) as a pure function, so an
// offline replay (internal/backtest) runs this exact code in simulated time.
type Plan struct {
	TargetCPU, TargetMem float64 // requests to patch (cores, bytes)
	CPUDrift, MemDrift   bool    // request moved past the drift tolerance
	MemLimitRaise        bool    // learned OOM floor needs a higher mem limit
	// MemLimitAdd: policy memory.enforceLimit — the container runs WITHOUT a
	// memory limit and the recommendation carries one: add it (opt-in only).
	MemLimitAdd      bool
	CPULimitRaise    bool // censored CPU (pinned at / throttled by its limit) needs a higher CPU limit
	CPUHeld, MemHeld bool // held at the live value (HPA scales on it)
	// Guaranteed: the container's request equals its limit and the policy opted in
	// (guaranteed: resizeBoth). Request and limit then move together (Limits), so
	// the pod keeps its QoS class. GuaranteedMemShrinkHeld: a memory shrink was
	// dropped because a Guaranteed pod's memory never shrinks.
	Guaranteed, GuaranteedMemShrinkHeld bool
}

// Drift tolerances: a resize is only worth it past 15% AND an absolute step.
const (
	cpuDriftRel = 0.15
	cpuDriftAbs = 0.02 // cores
	memDriftRel = 0.15
	memDriftAbs = 16 * 1024 * 1024 // bytes
)

// PlanInPlace gates the recommendation against the live container c: HPA hold,
// direction (shrink-only / grow-only), OOM override, drift tolerance and the
// OOM limit raise. It does NOT check confidence or cooldown.
func PlanInPlace(entry recommender.Entry, c k8s.Container) Plan {
	rec := entry.Rec.Recommended
	p := Plan{CPUHeld: entry.Rec.CPUHeldByHPA, MemHeld: entry.Rec.MemHeldByHPA}
	// Direction gate: Down (shrink-only) never raises a resource above what the
	// pod already has; Up (grow-only) never lowers it. Gated against the live pod
	// request before drift is measured, so a gated-away change reads as no drift.
	gCPU := entry.Direction.Gate(c.CPURequest, rec.CPURequest)
	gMem := entry.Direction.Gate(c.MemRequest, rec.MemRequest)
	// Safety override (opt-in): a container being OOMKilled may grow memory even
	// under a shrink-only policy — recovering health beats holding the ceiling.
	if entry.Rec.OOMRisk && entry.OOMOverridesDirection && rec.MemRequest > gMem {
		gMem = rec.MemRequest
	}
	p.TargetCPU, p.TargetMem = gCPU, gMem
	// Per-resource confidence: a resource without enough data of its own is held
	// like an HPA-held one (memory still moves on an OOM — health first).
	cpuOK, memOK := entry.Rec.ResourceConfidence()
	if !cpuOK {
		p.CPUHeld = true
	}
	if !memOK && !entry.Rec.OOMRisk {
		p.MemHeld = true
	}
	if p.CPUHeld {
		p.TargetCPU = c.CPURequest
	}
	if p.MemHeld {
		p.TargetMem = c.MemRequest
	}
	// Censored CPU (usage pinned at the CPU limit, or CFS throttling): the
	// measured usage is a lower bound, so the recommended growth is a
	// high-confidence one — the absolute/relative drift gate (sized for noise on
	// an UNcapped measurement) must not swallow it: 50m→64m is 14m, under the
	// 20m step, and would never be applied. Growth only, never under a
	// shrink-only policy (the CPU limit is not an OOM line: no safety override).
	cpuCensored := entry.Rec.Signals.CPUCensored && !p.CPUHeld && entry.Direction != policy.DirDown
	p.CPUDrift = !p.CPUHeld && (drift(c.CPURequest, gCPU, cpuDriftRel, cpuDriftAbs) ||
		(cpuCensored && gCPU >= c.CPURequest+0.001))
	// The CFS quota (the limit) is what throttles, not the request: raise it even
	// when the request is already where it should be. Only an EXISTING limit.
	p.CPULimitRaise = cpuCensored && c.CPULimit > 0 && rec.CPULimit > c.CPULimit*1.01
	if p.CPULimitRaise && !p.CPUDrift {
		p.TargetCPU = c.CPURequest // pure limit raise: request unchanged
	}
	p.MemDrift = !p.MemHeld && drift(c.MemRequest, gMem, memDriftRel, memDriftAbs)
	// A learned OOM floor may need to RAISE the mem limit even when the request
	// itself hasn't drifted: a restarted pod returns at its spec limit — the very
	// value that OOMKilled it. Raising a ceiling never shrinks anything, so force
	// the resize to lift the limit back above the floor.
	// Only an EXISTING limit is raised: a container without one is never given a
	// limit (the recommender reserves memory via the request instead).
	p.MemLimitRaise = entry.Rec.OOMRisk && !p.MemHeld && c.MemLimit > 0 && rec.MemLimit > c.MemLimit*1.01
	// Pure limit raise (OOM floor, request unchanged): keep the request exactly
	// as-is — only the ceiling moves.
	if p.MemLimitRaise && !p.MemDrift {
		p.TargetMem = c.MemRequest
	}
	// Opt-in limit (memory.enforceLimit): a container without a memory limit gets
	// the recommended one even when its request has not drifted — otherwise the
	// limit would never land. Without the opt-in a limit is never introduced.
	p.MemLimitAdd = entry.EnforceMemLimit && !p.MemHeld && c.MemLimit <= 0 && rec.MemLimit > 0
	if p.MemLimitAdd && !p.MemDrift {
		p.TargetMem = c.MemRequest
	}
	if entry.GuaranteedResize && c.GuaranteedShape() {
		p.guaranteedAdjust(rec, c)
	}
	return p
}

// guaranteedAdjust turns the plan of a Guaranteed-shaped container (policy
// guaranteed: resizeBoth) into one that keeps request == limit: a limit raise
// (OOM floor, censored CPU) raises the request to the limit instead of leaving
// them apart, and a memory shrink is dropped. The kubelet applies a lowered
// memory limit only best effort, and a limit equal to a request sitting near
// the peak turns a small error into an OOM, so memory only grows. A CPU shrink
// is safe (the CFS quota is simply lowered) and goes through. Limits then
// patches both to the same value.
func (p *Plan) guaranteedAdjust(rec recommender.Resources, c k8s.Container) {
	p.Guaranteed = true
	if p.CPULimitRaise && rec.CPULimit > p.TargetCPU {
		p.TargetCPU = rec.CPULimit
		p.CPUDrift = true
	}
	if p.MemLimitRaise && rec.MemLimit > p.TargetMem {
		p.TargetMem = rec.MemLimit
		p.MemDrift = true
	}
	if p.MemDrift && p.TargetMem < c.MemRequest {
		p.holdMem(c)
		p.GuaranteedMemShrinkHeld = true
	}
}

// Wants reports whether anything is worth patching.
func (p Plan) Wants() bool {
	return p.CPUDrift || p.CPULimitRaise || p.MemDrift || p.MemLimitRaise || p.MemLimitAdd
}

// Up reports whether the patch grows anything (short scale-up cooldown applies).
func (p Plan) Up(c k8s.Container) bool {
	return p.TargetCPU > c.CPURequest || p.TargetMem > c.MemRequest || p.MemLimitRaise || p.CPULimitRaise
}

// holdMem drops the memory part of the plan (PDB guardrail: CPU only).
func (p *Plan) holdMem(c k8s.Container) {
	p.TargetMem = c.MemRequest
	p.MemDrift = false
	p.MemLimitRaise = false
	p.MemLimitAdd = false
}

// holdCPU drops the CPU part of the plan.
func (p *Plan) holdCPU(c k8s.Container) {
	p.TargetCPU = c.CPURequest
	p.CPUDrift = false
	p.CPULimitRaise = false
}

// Disruptive reports whether applying the plan restarts the container: the
// resource that moves has resizePolicy RestartContainer (memory OR cpu).
func (p Plan) Disruptive(c k8s.Container) bool {
	return ((p.CPUDrift || p.CPULimitRaise) && c.CPURestartOnResize) || ((p.MemDrift || p.MemLimitRaise || p.MemLimitAdd) && c.MemRestartOnResize)
}

// holdDisruptive drops every part of the plan that would restart the container.
func (p *Plan) holdDisruptive(c k8s.Container) {
	if c.CPURestartOnResize {
		p.holdCPU(c)
	}
	if c.MemRestartOnResize {
		p.holdMem(c)
	}
}

// Limits returns the limits to patch alongside the requests (0 = leave as is).
// A limit is patched only with a request that DRIFTED — a request raised above
// the old limit is rejected otherwise — and a limit is a safety ceiling: NEVER
// shrunk in place (limitFloor). A memory limit is never INTRODUCED on a
// container running without one (not even by an OOM-learned floor) unless the
// policy opts in (memory.enforceLimit → MemLimitAdd).
//
// An EXISTING limit is always lifted to at least the new request, even when the
// recommendation carries no limit (limitFactor 0): a request above its limit is
// invalid and would be rejected on every tick (partial-limit containers).
func (p Plan) Limits(rec recommender.Resources, c k8s.Container) (cpuLim, memLim float64) {
	if p.Guaranteed {
		// request == limit, both patched in the same call: the QoS class is immutable
		// and a Guaranteed pod resized to unequal values is rejected. Unlike every
		// other path this may LOWER a limit (CPU only; memory never shrinks, see
		// guaranteedAdjust).
		if p.CPUDrift || p.CPULimitRaise {
			cpuLim = p.TargetCPU
		}
		if p.MemDrift || p.MemLimitRaise {
			memLim = p.TargetMem
		}
		return cpuLim, memLim
	}
	if (p.CPUDrift || p.CPULimitRaise) && rec.CPULimit > 0 {
		cpuLim = limitFloor(rec.CPULimit, c.CPULimit)
	}
	if p.CPUDrift && c.CPULimit > 0 && p.TargetCPU > math.Max(cpuLim, c.CPULimit) {
		cpuLim = p.TargetCPU
	}
	if (p.MemDrift || p.MemLimitRaise) && rec.MemLimit > 0 && c.MemLimit > 0 {
		memLim = limitFloor(rec.MemLimit, c.MemLimit)
	}
	if p.MemLimitAdd {
		memLim = rec.MemLimit
	}
	if p.MemDrift && c.MemLimit > 0 && p.TargetMem > math.Max(memLim, c.MemLimit) {
		memLim = p.TargetMem
	}
	// A patched limit is never below the patched request (limitFactor < 1 would
	// otherwise produce one, and the API rejects requests > limits).
	if cpuLim > 0 && cpuLim < p.TargetCPU {
		cpuLim = p.TargetCPU
	}
	if memLim > 0 && memLim < p.TargetMem {
		memLim = p.TargetMem
	}
	return cpuLim, memLim
}

// Cooldown is the anti-thrash interval after an actuation: the (short)
// up-cooldown when growing so we react fast to load, the (long) down-cooldown
// when shrinking so we never flap capacity away; never below floor.
func Cooldown(entry recommender.Entry, up bool, floor time.Duration) time.Duration {
	cd := entry.ScaleDownCooldown
	if up {
		cd = entry.ScaleUpCooldown
	}
	if cd < floor {
		cd = floor
	}
	return cd
}

// Cooled reports whether an actuation at now is allowed after the last one at
// last (zero = never acted).
func Cooled(entry recommender.Entry, up bool, floor time.Duration, now, last time.Time) bool {
	return last.IsZero() || now.Sub(last) >= Cooldown(entry, up, floor)
}

// Decide is the complete in-place decision for a container of a Burstable pod
// with no pending resize and no PDB block: confidence gate, PlanInPlace,
// drift, cooldown. last is the previous actuation of this workload/container
// (zero = never). ok=false means "leave the pod alone".
func Decide(entry recommender.Entry, c k8s.Container, now, last time.Time) (Plan, bool) {
	if !entry.Rec.Confident && !entry.Rec.OOMRisk {
		return Plan{}, false
	}
	p := PlanInPlace(entry, c)
	if !p.Wants() {
		return p, false
	}
	if !Cooled(entry, p.Up(c), 0, now, last) {
		return p, false
	}
	return p, true
}

// QuantizeCPU / QuantizeMem are the values the API actually receives
// (cpuStr: millicores, memStr: MiB, both rounded half-up).
func QuantizeCPU(cores float64) float64 { return float64(int64(cores*1000+0.5)) / 1000 }
func QuantizeMem(bytes float64) float64 {
	const mib = 1024 * 1024
	return float64(int64(bytes/mib+0.5)) * mib
}

// drift reports whether cur is far enough from target to be worth a resize.
func drift(cur, target, relTol, absTol float64) bool {
	d := math.Abs(cur - target)
	if d < absTol {
		return false
	}
	base := math.Max(math.Max(cur, target), 1e-9)
	return d/base >= relTol
}

// MinRolloutInterval floors how often a workload may be rolled, so we never
// churn disruptive rollouts even if the recommendation keeps drifting.
const MinRolloutInterval = 10 * time.Minute

// RolloutPlan is the template-level decision for ONE container (Apply=Rollout,
// InPlacePreferred falling back, Knative, proxy annotations): the requests and
// limits to write into the controller template. Like Plan it has no side effect;
// the backtest replays it.
type RolloutPlan struct {
	TargetCPU, TargetMem float64 // template requests to write
	CPULimit, MemLimit   float64 // template limits to write (0 = none patched)
	CPUHeld, MemHeld     bool    // held at the template value (HPA, or no data of its own)
	Up                   bool    // grows something: the short scale-up cooldown applies
}

// PlanRollout gates the recommendation against the TEMPLATE values
// (entry.Rec.Current): HPA hold, per-resource confidence, direction gate, OOM
// override, drift tolerance, censored-CPU and OOM limit raises, enforceLimit. ok=false
// means nothing is worth patching. It does NOT check confidence as a whole or any
// cooldown.
func PlanRollout(entry recommender.Entry) (RolloutPlan, bool) {
	rec := entry.Rec.Recommended
	// Baseline: the template request — or, for an injected proxy, the live one
	// (the recommender fills Current from the running pods: there is no template).
	tmplCPU := entry.Rec.Current.CPURequest
	tmplMem := entry.Rec.Current.MemRequest
	// HPA coexistence: hold whichever resource the HPA scales on (CPU and/or mem)
	// at its template value; only the non-scaled resource is rolled. (An HPA on
	// resource utilization divides by the SUM of the pod's container requests, so
	// this holds for sidecars too.)
	cpuHeld, memHeld := entry.Rec.CPUHeldByHPA, entry.Rec.MemHeldByHPA
	// Per-resource confidence (as PlanInPlace): no data of its own → held.
	if cpuOK, memOK := entry.Rec.ResourceConfidence(); !cpuOK || (!memOK && !entry.Rec.OOMRisk) {
		cpuHeld = cpuHeld || !cpuOK
		memHeld = memHeld || (!memOK && !entry.Rec.OOMRisk)
	}
	// Direction gate (shrink-only/grow-only) against the template request.
	gCPU := entry.Direction.Gate(tmplCPU, rec.CPURequest)
	gMem := entry.Direction.Gate(tmplMem, rec.MemRequest)
	if entry.Rec.OOMRisk && entry.OOMOverridesDirection && rec.MemRequest > gMem {
		gMem = rec.MemRequest // OOM recovery may grow mem even under shrink-only
	}
	targetCPU, targetMem := gCPU, gMem
	if cpuHeld {
		targetCPU = tmplCPU
	}
	if memHeld {
		targetMem = tmplMem
	}
	// Censored CPU (pinned at / throttled by its limit): same rule as
	// PlanInPlace — a growth past the drift gate and a CPU limit raise, never
	// under a shrink-only policy.
	cpuCensored := entry.Rec.Signals.CPUCensored && !cpuHeld && entry.Direction != policy.DirDown
	cpuDrift := !cpuHeld && (drift(tmplCPU, gCPU, cpuDriftRel, cpuDriftAbs) || (cpuCensored && gCPU >= tmplCPU+0.001))
	memDrift := !memHeld && drift(tmplMem, gMem, memDriftRel, memDriftAbs)
	cpuLimitRaise := cpuCensored && entry.Rec.Current.CPULimit > 0 && rec.CPULimit > entry.Rec.Current.CPULimit*1.01
	// Learned OOM floor: raise the mem limit even when the request template hasn't
	// drifted — the template limit is the value that OOMKilled the container.
	memLimitRaise := entry.Rec.OOMRisk && !memHeld && entry.Rec.Current.MemLimit > 0 && rec.MemLimit > entry.Rec.Current.MemLimit*1.01
	// Opt-in (memory.enforceLimit): a template without a memory limit gets the
	// recommended one, as in place (Plan.MemLimitAdd).
	memLimitAdd := entry.EnforceMemLimit && !memHeld && entry.Rec.Current.MemLimit <= 0 && rec.MemLimit > 0
	if !cpuDrift && !memDrift && !memLimitRaise && !cpuLimitRaise && !memLimitAdd {
		return RolloutPlan{}, false
	}
	if (memLimitRaise || memLimitAdd) && !memDrift {
		targetMem = tmplMem // pure limit raise/add: template request unchanged
	}
	if cpuLimitRaise && !cpuDrift {
		targetCPU = tmplCPU
	}
	p := RolloutPlan{TargetCPU: targetCPU, TargetMem: targetMem, CPUHeld: cpuHeld, MemHeld: memHeld}
	p.Up = targetCPU > tmplCPU || targetMem > tmplMem || memLimitRaise || cpuLimitRaise
	// Same rule as in-place: never auto-shrink a limit — floor at the current
	// (template) limit. Only raise a limit when a growing request needs it; an
	// existing limit is always lifted to at least the new request (a request
	// above its limit is invalid), even without a limitFactor.
	if !cpuHeld {
		if rec.CPULimit > 0 {
			p.CPULimit = limitFloor(rec.CPULimit, entry.Rec.Current.CPULimit)
		} else if entry.Rec.Current.CPULimit > 0 && targetCPU > entry.Rec.Current.CPULimit {
			p.CPULimit = targetCPU
		}
		if p.CPULimit > 0 && p.CPULimit < targetCPU {
			p.CPULimit = targetCPU
		}
	}
	// As in-place: never introduce a mem limit the template didn't have (not even
	// for an OOM-learned floor) — unless the policy opts in (memory.enforceLimit).
	if !memHeld {
		if rec.MemLimit > 0 && (entry.Rec.Current.MemLimit > 0 || memLimitAdd) {
			p.MemLimit = limitFloor(rec.MemLimit, entry.Rec.Current.MemLimit)
		} else if entry.Rec.Current.MemLimit > 0 && targetMem > entry.Rec.Current.MemLimit {
			p.MemLimit = targetMem
		}
		if p.MemLimit > 0 && p.MemLimit < targetMem {
			p.MemLimit = targetMem
		}
	}
	return p, true
}

// RollingState is what the disruptive-rolling gate looks at for one Ready pod that
// would be restarted by a resize with resizePolicy RestartContainer.
type RollingState struct {
	// OthersReady: every other replica is Ready, none Pending or missing.
	OthersReady bool
	// ControllerShort: the controller's own ready count is below its desired count.
	ControllerShort bool
	// ReadyRestarted: Ready pods of the workload already restarted this cycle.
	ReadyRestarted int
	// PDBProtected / PDBAllowed: the live budget (PDBAllowed is disruptionsAllowed).
	PDBProtected bool
	PDBAllowed   int
}

// RollingVerdict is the outcome of RollingGate.
type RollingVerdict int

const (
	RollingGo         RollingVerdict = iota // restart this pod now
	RollingWait                             // another replica is not Ready (or missing) yet
	RollingOverBudget                       // this cycle's restart budget is used up
)

// RollingBudget is how many Ready pods of a workload may be restarted in one
// cycle: the PDB's disruptionsAllowed when one covers it, else one.
func RollingBudget(protected bool, allowed int) int {
	if protected {
		return allowed
	}
	return 1
}

// RollingGate decides whether a READY pod may be restarted now. A NotReady target
// is exempt (it serves nothing: restarting it costs no availability) and never
// reaches this gate.
func RollingGate(st RollingState) RollingVerdict {
	if st.ControllerShort || !st.OthersReady {
		return RollingWait
	}
	if st.ReadyRestarted >= RollingBudget(st.PDBProtected, st.PDBAllowed) {
		return RollingOverBudget
	}
	return RollingGo
}
