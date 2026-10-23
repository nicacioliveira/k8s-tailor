package backtest

// Faithful port of the Kubernetes Vertical Pod Autoscaler recommender + updater
// decision logic, from kubernetes/autoscaler tag vertical-pod-autoscaler-1.4.1:
//
//   pkg/recommender/util/{histogram,histogram_options,decaying_histogram}.go
//   pkg/recommender/model/{aggregations_config,aggregate_container_state,container}.go
//   pkg/recommender/logic/{recommender,estimator}.go
//   pkg/updater/priority/{priority_processor,update_priority_calculator}.go
//
// Only the math and the update decision are ported; the API plumbing
// (checkpoints, CRDs, eviction rate limiting, admission webhook) is modelled as
// "an accepted update sets every replica's requests to the target, and limits
// proportionally to the template's limit/request ratio" (default
// controlledValues=RequestsAndLimits). Mode modelled: updateMode Recreate/Auto
// (evict + re-admit). In-place (InPlaceOrRecreate, alpha in 1.4) is NOT modelled.

import (
	"math"
	"time"
)

// VPAConfig holds the upstream defaults (flag names in comments).
type VPAConfig struct {
	TargetCPUPercentile float64       // --target-cpu-percentile 0.9
	LowerCPUPercentile  float64       // --recommendation-lower-bound-cpu-percentile 0.5
	UpperCPUPercentile  float64       // --recommendation-upper-bound-cpu-percentile 0.95
	TargetMemPercentile float64       // --target-memory-percentile 0.9
	LowerMemPercentile  float64       // --recommendation-lower-bound-memory-percentile 0.5
	UpperMemPercentile  float64       // --recommendation-upper-bound-memory-percentile 0.95
	SafetyMargin        float64       // --recommendation-margin-fraction 0.15
	PodMinCPUMilli      float64       // --pod-recommendation-min-cpu-millicores 25
	PodMinMemMB         float64       // --pod-recommendation-min-memory-mb 250 (×1024×1024)
	CPUHalfLife         time.Duration // --cpu-histogram-decay-half-life 24h
	MemHalfLife         time.Duration // --memory-histogram-decay-half-life 24h
	MemAggInterval      time.Duration // --memory-aggregation-interval 24h
	ConfidenceInterval  time.Duration // --confidence-interval-{cpu,memory} 24h
	OOMBumpUpRatio      float64       // --oom-bump-up-ratio 1.2
	OOMMinBumpUp        float64       // --oom-min-bump-up-bytes 100MiB
	UpdateThreshold     float64       // updater --pod-update-threshold 0.1
	PodLifetimeThresh   time.Duration // updater --in-recommendation-bounds-eviction-lifetime-threshold 12h
	EvictAfterOOM       time.Duration // updater --evict-after-oom-threshold 10m
	MinReplicas         int           // updater --min-replicas 2
	// MetricsResolutionSec is the cadence metrics-server feeds the recommender
	// (60s). A coarser trace step is up-weighted to match (see Observe).
	MetricsResolutionSec int64
}

func DefaultVPA() VPAConfig {
	return VPAConfig{
		TargetCPUPercentile: 0.9, LowerCPUPercentile: 0.5, UpperCPUPercentile: 0.95,
		TargetMemPercentile: 0.9, LowerMemPercentile: 0.5, UpperMemPercentile: 0.95,
		SafetyMargin: 0.15, PodMinCPUMilli: 25, PodMinMemMB: 250,
		CPUHalfLife: 24 * time.Hour, MemHalfLife: 24 * time.Hour, MemAggInterval: 24 * time.Hour,
		ConfidenceInterval: 24 * time.Hour, OOMBumpUpRatio: 1.2, OOMMinBumpUp: 100 * mib,
		UpdateThreshold: 0.1, PodLifetimeThresh: 12 * time.Hour, EvictAfterOOM: 10 * time.Minute,
		MinReplicas: 2, MetricsResolutionSec: 60,
	}
}

// ---------------- histogram (util/histogram*.go) ----------------

const (
	vpaMinSampleWeight  = 0.1
	vpaEpsilon          = 0.001 * vpaMinSampleWeight
	vpaMaxDecayExponent = 100
)

type expOptions struct {
	numBuckets int
	first      float64
	ratio      float64
	epsilon    float64
}

func newExpOptions(maxValue, firstBucketSize, ratio, epsilon float64) expOptions {
	n := int(math.Ceil(logBase(ratio, maxValue*(ratio-1)/firstBucketSize+1))) + 1
	return expOptions{n, firstBucketSize, ratio, epsilon}
}

func logBase(base, x float64) float64 { return math.Log(x) / math.Log(base) }

func (o expOptions) findBucket(v float64) int {
	if v < o.first {
		return 0
	}
	b := int(logBase(o.ratio, v*(o.ratio-1)/o.first+1))
	if b >= o.numBuckets {
		return o.numBuckets - 1
	}
	return b
}

func (o expOptions) bucketStart(b int) float64 {
	if b == 0 {
		return 0
	}
	return o.first * (math.Pow(o.ratio, float64(b)) - 1) / (o.ratio - 1)
}

type histogram struct {
	opt        expOptions
	w          []float64
	total      float64
	minB, maxB int
}

func newHistogram(o expOptions) *histogram {
	return &histogram{opt: o, w: make([]float64, o.numBuckets), minB: o.numBuckets - 1, maxB: 0}
}

func (h *histogram) add(v, weight float64) {
	b := h.opt.findBucket(v)
	h.w[b] += weight
	h.total += weight
	if b < h.minB && h.w[b] >= h.opt.epsilon {
		h.minB = b
	}
	if b > h.maxB && h.w[b] >= h.opt.epsilon {
		h.maxB = b
	}
}

func safeSubtract(v, sub, eps float64) float64 {
	v -= sub
	if v < eps {
		return 0
	}
	return v
}

func (h *histogram) subtract(v, weight float64) {
	b := h.opt.findBucket(v)
	h.total = safeSubtract(h.total, weight, h.opt.epsilon)
	h.w[b] = safeSubtract(h.w[b], weight, h.opt.epsilon)
	h.updateMinMax()
}

func (h *histogram) updateMinMax() {
	last := h.opt.numBuckets - 1
	for h.w[h.minB] < h.opt.epsilon && h.minB < last {
		h.minB++
	}
	for h.w[h.maxB] < h.opt.epsilon && h.maxB > 0 {
		h.maxB--
	}
}

func (h *histogram) scale(f float64) {
	for b := h.minB; b <= h.maxB; b++ {
		h.w[b] *= f
	}
	h.total *= f
	h.updateMinMax()
}

func (h *histogram) empty() bool { return h.w[h.minB] < h.opt.epsilon }

// percentile returns the END of the bucket where the cumulative weight first
// reaches p·total (the start of the last bucket if it is the last one).
func (h *histogram) percentile(p float64) float64 {
	if h.empty() {
		return 0
	}
	partial := 0.0
	threshold := p * h.total
	b := h.minB
	for ; b < h.maxB; b++ {
		partial += h.w[b]
		if partial >= threshold {
			break
		}
	}
	if b < h.opt.numBuckets-1 {
		return h.opt.bucketStart(b + 1)
	}
	return h.opt.bucketStart(b)
}

type decayingHistogram struct {
	histogram
	halfLife time.Duration
	ref      time.Time
}

func newDecaying(o expOptions, halfLife time.Duration) *decayingHistogram {
	return &decayingHistogram{histogram: *newHistogram(o), halfLife: halfLife}
}

func (h *decayingHistogram) shiftRef(newRef time.Time) {
	newRef = newRef.Round(h.halfLife)
	exp := int(math.Floor(float64(h.ref.Sub(newRef))/float64(h.halfLife) + 0.5))
	h.histogram.scale(math.Ldexp(1, exp))
	h.ref = newRef
}

func (h *decayingHistogram) decayFactor(ts time.Time) float64 {
	maxAllowed := h.ref.Add(time.Duration(int64(h.halfLife) * vpaMaxDecayExponent))
	if ts.After(maxAllowed) {
		h.shiftRef(ts)
	}
	return math.Exp2(float64(ts.Sub(h.ref)) / float64(h.halfLife))
}

func (h *decayingHistogram) addSample(v, w float64, ts time.Time) {
	h.histogram.add(v, w*h.decayFactor(ts))
}

func (h *decayingHistogram) subtractSample(v, w float64, ts time.Time) {
	h.histogram.subtract(v, w*h.decayFactor(ts))
}

// ---------------- model (aggregate + per-container state) ----------------

type vpaAggregate struct {
	cpu, mem     *decayingHistogram
	first, last  time.Time
	totalSamples int
}

type vpaContainer struct {
	lastCPU       time.Time
	memoryPeak    float64
	oomPeak       float64
	windowEnd     time.Time
	lastMemSample time.Time
	started       time.Time // container (re)start: VPA eviction or OOM restart
}

// ResourceAmount semantics: CPU is int64 millicores, memory int64 bytes, with
// truncation on every float→amount conversion (model.resourceAmountFromFloat).
func cpuAmount(cores float64) float64 {
	if math.IsInf(cores, 1) {
		return math.Inf(1)
	}
	return math.Trunc(cores*1000) / 1000
}
func memAmount(b float64) float64 {
	if math.IsInf(b, 1) {
		return math.Inf(1)
	}
	return math.Trunc(b)
}

type vpa struct {
	cfg       VPAConfig
	tr        *Trace
	agg       vpaAggregate
	pods      []vpaContainer
	weightK   float64 // samples-per-tick multiplier (trace step / metrics resolution)
	tmplRatio struct{ cpu, mem float64 }
	lastEvict time.Time
}

// NewVPA returns a factory for the VPA recommender+updater model.
func NewVPA(cfg VPAConfig) Factory {
	return func(tr *Trace) (Strategy, error) {
		v := &vpa{cfg: cfg, tr: tr, pods: make([]vpaContainer, len(tr.Pods))}
		v.agg.cpu = newDecaying(newExpOptions(1000, 0.01, 1.05, vpaEpsilon), cfg.CPUHalfLife)
		v.agg.mem = newDecaying(newExpOptions(1e12, 1e7, 1.05, vpaEpsilon), cfg.MemHalfLife)
		v.weightK = 1
		if cfg.MetricsResolutionSec > 0 && tr.StepSec > cfg.MetricsResolutionSec {
			v.weightK = math.Round(float64(tr.StepSec) / float64(cfg.MetricsResolutionSec))
		}
		if tr.Initial.CPURequest > 0 {
			v.tmplRatio.cpu = tr.Initial.CPULimit / tr.Initial.CPURequest
		}
		if tr.Initial.MemRequest > 0 {
			v.tmplRatio.mem = tr.Initial.MemLimit / tr.Initial.MemRequest
		}
		start := time.Unix(tr.Start, 0).UTC()
		for i := range v.pods {
			v.pods[i].started = start
		}
		v.lastEvict = start
		return v, nil
	}
}

func (v *vpa) Name() string { return "vpa" }
func (v *vpa) Close()       {}

// addCPU: aggregate_container_state.addCPUSample (weight = minSampleWeight in
// 1.4.x — no longer request-weighted). A 5-min trace point stands for the k=5
// one-minute samples metrics-server would have produced: weight×k, count+=k.
func (v *vpa) addCPU(p int, cores float64, ts time.Time) {
	c := &v.pods[p]
	if !ts.After(c.lastCPU) {
		return
	}
	v.agg.cpu.addSample(cores, vpaMinSampleWeight*v.weightK, ts)
	if ts.After(v.agg.last) {
		v.agg.last = ts
	}
	if v.agg.first.IsZero() || ts.Before(v.agg.first) {
		v.agg.first = ts
	}
	v.agg.totalSamples += int(v.weightK)
	c.lastCPU = ts
}

// addMem: container.addMemorySample — one peak per container per 24h window.
func (v *vpa) addMem(p int, usage float64, ts time.Time, isOOM bool) {
	c := &v.pods[p]
	if usage < 0 || (!isOOM && ts.Before(c.lastMemSample)) {
		return
	}
	c.lastMemSample = ts
	if c.windowEnd.IsZero() {
		c.windowEnd = ts
	}
	addNew := false
	if ts.Before(c.windowEnd) {
		old := math.Max(c.memoryPeak, c.oomPeak)
		if old != 0 && usage > old {
			v.agg.mem.subtractSample(old, 1.0, c.windowEnd)
			addNew = true
		}
	} else {
		iv := v.cfg.MemAggInterval
		shift := ts.Sub(c.windowEnd).Truncate(iv) + iv
		c.windowEnd = c.windowEnd.Add(shift)
		c.memoryPeak, c.oomPeak = 0, 0
		addNew = true
	}
	if addNew {
		v.agg.mem.addSample(usage, 1.0, c.windowEnd)
		if isOOM {
			c.oomPeak = usage
		} else {
			c.memoryPeak = usage
		}
	}
}

// recordOOM: container.RecordOOM(timestamp, requestedMemory).
func (v *vpa) recordOOM(p int, ts time.Time, requested float64) {
	c := &v.pods[p]
	if ts.Before(c.windowEnd.Add(-v.cfg.MemAggInterval)) {
		return
	}
	used := math.Max(requested, c.memoryPeak)
	needed := math.Max(used+memAmount(v.cfg.OOMMinBumpUp), memAmount(used*v.cfg.OOMBumpUpRatio))
	v.addMem(p, needed, ts, true)
}

type vpaRec struct{ target, lower, upper Resources }

func (v *vpa) confidence() float64 {
	ci := v.cfg.ConfidenceInterval
	life := float64(v.agg.last.Sub(v.agg.first)) / float64(ci)
	samples := float64(v.agg.totalSamples) / ci.Minutes()
	return math.Min(life, samples)
}

// estimate: logic.CreatePodResourceRecommender + GetRecommendedPodResources for
// a single-container pod: percentile → margin → confidence multiplier → min.
func (v *vpa) estimate() (vpaRec, bool) {
	if v.agg.cpu.empty() && v.agg.mem.empty() {
		return vpaRec{}, false
	}
	m := v.cfg.SafetyMargin
	conf := v.confidence()
	minCPU := cpuAmount(v.cfg.PodMinCPUMilli * 0.001)
	minMem := memAmount(v.cfg.PodMinMemMB * 1024 * 1024)
	withMarginCPU := func(x float64) float64 { return x + cpuAmount(x*m) }
	withMarginMem := func(x float64) float64 { return x + memAmount(x*m) }
	upperMul := math.Pow(1+1.0/conf, 1.0)
	lowerMul := math.Pow(1+0.001/conf, -2.0)

	cpuP := func(p float64) float64 { return cpuAmount(v.agg.cpu.percentile(p)) }
	memP := func(p float64) float64 { return memAmount(v.agg.mem.percentile(p)) }

	var r vpaRec
	r.target.CPURequest = math.Max(withMarginCPU(cpuP(v.cfg.TargetCPUPercentile)), minCPU)
	r.lower.CPURequest = math.Max(cpuAmount(withMarginCPU(cpuP(v.cfg.LowerCPUPercentile))*lowerMul), minCPU)
	r.upper.CPURequest = math.Max(cpuAmount(withMarginCPU(cpuP(v.cfg.UpperCPUPercentile))*upperMul), minCPU)
	r.target.MemRequest = math.Max(withMarginMem(memP(v.cfg.TargetMemPercentile)), minMem)
	r.lower.MemRequest = math.Max(memAmount(withMarginMem(memP(v.cfg.LowerMemPercentile))*lowerMul), minMem)
	r.upper.MemRequest = math.Max(memAmount(withMarginMem(memP(v.cfg.UpperMemPercentile))*upperMul), minMem)
	return r, true
}

func (v *vpa) Observe(tk Tick, applied Resources) (Resources, error) {
	ts := time.Unix(tk.T, 0).UTC()
	live := 0
	quickOOM := false
	for p := range v.tr.Pods {
		if c := tk.CPU[p]; !math.IsNaN(c) {
			v.addCPU(p, c, ts)
		}
		if m := tk.Mem[p]; !math.IsNaN(m) {
			v.addMem(p, m, ts, false)
		}
		if !math.IsNaN(tk.CPU[p]) || !math.IsNaN(tk.Mem[p]) {
			live++
		}
		if tk.OOM[p] {
			// The OOM observer reports the container's memory REQUEST.
			v.recordOOM(p, ts, applied.MemRequest)
			// Quick OOM: the container died < evict-after-oom-threshold after it
			// (re)started. Then it restarts, resetting its start time.
			if ts.Sub(v.pods[p].started) < v.cfg.EvictAfterOOM {
				quickOOM = true
			}
			v.pods[p].started = ts
		}
	}
	if !tk.CanAct {
		return applied, nil
	}
	rec, ok := v.estimate()
	if !ok {
		return applied, nil
	}
	if live < v.cfg.MinReplicas {
		return applied, nil // updater refuses to evict below --min-replicas
	}

	// priority_processor.GetUpdatePriority (millivalue arithmetic).
	milli := func(x float64) float64 { return math.Round(x * 1000) }
	outside := false
	diff := 0.0
	type pair struct{ req, target, lo, hi float64 }
	for _, pr := range []pair{
		{applied.CPURequest, rec.target.CPURequest, rec.lower.CPURequest, rec.upper.CPURequest},
		{applied.MemRequest, rec.target.MemRequest, rec.lower.MemRequest, rec.upper.MemRequest},
	} {
		if pr.req < pr.lo || pr.req > pr.hi {
			outside = true
		}
		tot := math.Max(milli(pr.req), 1)
		diff += math.Abs(tot-milli(pr.target)) / tot
	}
	// update_priority_calculator.AddPod: within bounds, only pods older than the
	// lifetime threshold with a big-enough diff; quick OOM bypasses both.
	if !outside && !quickOOM {
		if ts.Sub(v.lastEvict) < v.cfg.PodLifetimeThresh {
			return applied, nil
		}
		if diff < v.cfg.UpdateThreshold {
			return applied, nil
		}
	}
	if quickOOM && diff == 0 {
		return applied, nil
	}
	next := Resources{CPURequest: rec.target.CPURequest, MemRequest: rec.target.MemRequest}
	// Admission: limits keep the template's limit/request proportion.
	if v.tmplRatio.cpu > 0 {
		next.CPULimit = cpuAmount(next.CPURequest * v.tmplRatio.cpu)
	}
	if v.tmplRatio.mem > 0 {
		next.MemLimit = memAmount(next.MemRequest * v.tmplRatio.mem)
	}
	if next == applied {
		return applied, nil
	}
	v.lastEvict = ts
	for p := range v.pods {
		v.pods[p].started = ts
	}
	return next, nil
}
