// Package metrics is a tiny, dependency-free Prometheus text-format registry for
// the engine's OPERATIONAL metrics (resizes applied/failed/skipped, refresh
// latency, backend query errors, leadership). The per-recommendation gauges stay
// in internal/server (they are computed from the engine state at scrape time).
//
// Deliberately minimal: counters, gauges and fixed-bucket histograms with
// label sets, rendered in the 0.0.4 text exposition format. Pulling
// client_golang would add ~10 modules for what is ~200 lines here.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

type kind string

const (
	kindCounter   kind = "counter"
	kindGauge     kind = "gauge"
	kindHistogram kind = "histogram"
)

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64 // histogram only

	mu     sync.Mutex
	values map[string]float64 // label-values key -> value (counter/gauge)
	hist   map[string]*histo  // label-values key -> histogram
}

type histo struct {
	counts []uint64 // per bucket (non-cumulative)
	sum    float64
	count  uint64
}

// Registry holds metric families. Safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// NewRegistry returns an empty registry (tests use their own).
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

// Default is the process-wide registry the engine components write to and the
// /metrics handler renders.
var Default = NewRegistry()

func (r *Registry) family(name, help string, k kind, labels []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		return f
	}
	f := &family{name: name, help: help, kind: k, labels: labels, buckets: buckets,
		values: map[string]float64{}, hist: map[string]*histo{}}
	r.families[name] = f
	return f
}

// Counter is a monotonically increasing labelled counter family.
type Counter struct{ f *family }

// Gauge is a labelled gauge family.
type Gauge struct{ f *family }

// Histogram is a labelled fixed-bucket histogram family.
type Histogram struct{ f *family }

func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.family(name, help, kindCounter, labels, nil)}
}

func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.family(name, help, kindGauge, labels, nil)}
}

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Histogram{r.family(name, help, kindHistogram, labels, b)}
}

func key(f *family, lv []string) string {
	if len(lv) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", f.name, len(f.labels), len(lv)))
	}
	return strings.Join(lv, "\xff")
}

// Inc adds 1.
func (c *Counter) Inc(lv ...string) { c.Add(1, lv...) }

// Add adds v (must be >= 0).
func (c *Counter) Add(v float64, lv ...string) {
	if v < 0 {
		return
	}
	k := key(c.f, lv)
	c.f.mu.Lock()
	c.f.values[k] += v
	c.f.mu.Unlock()
}

// Value returns the current value (tests / introspection).
func (c *Counter) Value(lv ...string) float64 {
	k := key(c.f, lv)
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	return c.f.values[k]
}

// Set sets the gauge.
func (g *Gauge) Set(v float64, lv ...string) {
	k := key(g.f, lv)
	g.f.mu.Lock()
	g.f.values[k] = v
	g.f.mu.Unlock()
}

// Value returns the gauge value.
func (g *Gauge) Value(lv ...string) float64 {
	k := key(g.f, lv)
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	return g.f.values[k]
}

// Reset drops every label set — for gauges recomputed wholesale each cycle
// (e.g. pods by resize state), so a state that disappeared reads as absent,
// not stuck at its last value.
func (g *Gauge) Reset() {
	g.f.mu.Lock()
	g.f.values = map[string]float64{}
	g.f.mu.Unlock()
}

// Observe records one sample.
func (h *Histogram) Observe(v float64, lv ...string) {
	k := key(h.f, lv)
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	st := h.f.hist[k]
	if st == nil {
		st = &histo{counts: make([]uint64, len(h.f.buckets))}
		h.f.hist[k] = st
	}
	for i, ub := range h.f.buckets {
		if v <= ub {
			st.counts[i]++
			break
		}
	}
	st.sum += v
	st.count++
}

// Count returns the number of observations (tests).
func (h *Histogram) Count(lv ...string) uint64 {
	k := key(h.f, lv)
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if st := h.f.hist[k]; st != nil {
		return st.count
	}
	return 0
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func labelStr(names, vals []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escape(vals[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escape(extra[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func fmtFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return fmt.Sprintf("%g", v)
}

func splitKey(k string, n int) []string {
	if n == 0 {
		return nil
	}
	return strings.SplitN(k, "\xff", n)
}

// Write renders every family in the Prometheus text format, sorted by name and
// label values so output is stable.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	r.mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		r.mu.Lock()
		f := r.families[n]
		r.mu.Unlock()
		f.write(w)
	}
}

func (f *family) write(w io.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
	if f.kind == kindHistogram {
		keys := make([]string, 0, len(f.hist))
		for k := range f.hist {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			st := f.hist[k]
			lv := splitKey(k, len(f.labels))
			var cum uint64
			for i, ub := range f.buckets {
				cum += st.counts[i]
				fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelStr(f.labels, lv, "le", fmtFloat(ub)), cum)
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelStr(f.labels, lv, "le", "+Inf"), st.count)
			fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labelStr(f.labels, lv), fmtFloat(st.sum))
			fmt.Fprintf(w, "%s_count%s %d\n", f.name, labelStr(f.labels, lv), st.count)
		}
		return
	}
	keys := make([]string, 0, len(f.values))
	for k := range f.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", f.name, labelStr(f.labels, splitKey(k, len(f.labels))), fmtFloat(f.values[k]))
	}
}

// ---- The engine's operational metrics (registered on Default). ----

var (
	// Resizes counts actuation attempts by method (inplace|rollout|ksvc) and
	// result (applied|failed).
	Resizes = Default.Counter("tailor_resizes_total",
		"actuation attempts by method (inplace|rollout|ksvc) and result (applied|failed)", "mode", "result")
	// Skips counts reconcile evaluations that wanted to act but were held back,
	// by reason. Incremented per reconcile tick per pod/container, so read it as
	// a rate, not as a number of distinct workloads.
	Skips = Default.Counter("tailor_resize_skipped_total",
		"reconcile evaluations held back from actuating, by reason (per tick per pod/container)", "reason")
	// WebhookSkips counts pod containers the webhook admitted UNMODIFIED although
	// their effective mode includes Webhook, by reason: warming_up (the engine
	// has not completed its first refresh since it started, so it has no
	// recommendation at all), not_confident (the recommendation exists but has
	// too little history), guaranteed_qos (the template is Guaranteed and the
	// container's policy is not guaranteed: resizeBoth, so the webhook leaves it
	// alone to keep it Guaranteed) or guaranteed_mem_shrink (resizeBoth: the memory
	// shrink of a Guaranteed container is dropped, CPU and growth still apply).
	// Per admission call per container.
	WebhookSkips = Default.Counter("tailor_webhook_skipped_total",
		"pod containers admitted unmodified by the webhook although it applies to them, by reason (warming_up|not_confident|guaranteed_qos|guaranteed_mem_shrink)", "reason")
	// DisruptiveWaitExpired counts disruptive-rolling decisions taken after
	// DISRUPTIVE_MAX_WAIT elapsed on a permanently NotReady sibling, by outcome:
	// proceeded (the live PDB and replica reads allowed the restart) or held (they
	// did not: nothing was restarted).
	DisruptiveWaitExpired = Default.Counter("tailor_disruptive_wait_expired_total",
		"disruptive rolling decisions after the NotReady-sibling wait expired (DISRUPTIVE_MAX_WAIT), by outcome (proceeded|held)", "outcome")
	// WarmCacheWrites counts the leader's writes of the warm-cache ConfigMap by
	// result (ok|error|too_large); WarmCacheEntries is how many entries this replica
	// loaded from it at start (0 = none, expired or damaged).
	WarmCacheWrites = Default.Counter("tailor_warm_cache_writes_total",
		"warm-cache ConfigMap writes by the leader, by result (ok|error|too_large)", "result")
	WarmCacheEntries = Default.Gauge("tailor_warm_cache_loaded_entries",
		"recommendations loaded from the warm-cache ConfigMap at start (served to the webhook until the first refresh)")
	// PodsResizeState counts running pods whose previous in-place resize has not
	// landed, by state (Pending/Deferred, Pending/Infeasible, InProgress, ...).
	PodsResizeState = Default.Gauge("tailor_pods_resize_state",
		"running pods with an outstanding in-place resize, by state", "state")
	// RefreshDuration is the recommender refresh latency.
	RefreshDuration = Default.Histogram("tailor_refresh_duration_seconds",
		"recommender refresh latency", []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}, "result")
	// LastRefresh is the unix time of the last SUCCESSFUL refresh.
	LastRefresh = Default.Gauge("tailor_last_refresh_success_timestamp_seconds",
		"unix time of the last successful recommender refresh")
	// QueryErrors counts failed backend calls (vm = VictoriaMetrics/Prometheus,
	// clickhouse = decision store).
	QueryErrors = Default.Counter("tailor_query_errors_total",
		"failed backend queries by backend (vm|clickhouse)", "backend")
	// Leader is 1 while this replica holds the actuation Lease (or leader
	// election is disabled), 0 otherwise.
	Leader = Default.Gauge("tailor_leader",
		"1 when this replica runs the actuating reconciler (holds the Lease or election disabled)")
)
