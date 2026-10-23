package backtest

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

// KRR: CPU = max over pods of each pod's P95 (linear interpolation); memory =
// max over pods of the peak × 1.15, request = limit; ceil to 1m / 1MiB.
func TestKRRSimpleStrategyValues(t *testing.T) {
	n := 200
	tr := fnTrace(2, n,
		func(p, i int) float64 { return float64(i%100+1) / 100 * float64(p+1) }, // pod1 = 2× pod0
		func(p, i int) float64 { return float64(100+p*50) * mib },
		Resources{CPURequest: 5, CPULimit: 5, MemRequest: gib, MemLimit: gib})
	s, _ := NewKRR(DefaultKRR())(tr)
	k := s.(*krr)
	cpu, mem, ok := k.recommend(n - 1)
	if !ok {
		t.Fatal("no recommendation with 400 points")
	}
	var vals []float64
	for i := 0; i < n; i++ {
		vals = append(vals, tr.Pods[1].CPU[i])
	}
	want := math.Ceil(recommender.Quantile(vals, 0.95)*1000) / 1000
	if cpu != want {
		t.Fatalf("cpu = %v, want %v (max over pods of per-pod P95)", cpu, want)
	}
	if wantMem := math.Ceil(150*mib*1.15/mib) * mib; mem != wantMem {
		t.Fatalf("mem = %vMi, want %vMi", mem/mib, wantMem/mib)
	}
	tk := Tick{I: n - 1, T: tr.TimeAt(n - 1), CanAct: true}
	got, _ := k.Observe(tk, tr.Initial)
	if got.CPULimit != 0 || got.MemLimit != got.MemRequest {
		t.Fatalf("KRR must unset the CPU limit and set mem limit = request: %+v", got)
	}
}

func TestKRRNeedsPointsAndMinimums(t *testing.T) {
	tr := constTrace(1, 99, 0.0001, mib, Resources{CPURequest: 1, MemRequest: gib})
	k := &krr{cfg: DefaultKRR(), tr: tr}
	if _, _, ok := k.recommend(98); ok {
		t.Fatal("99 points < points_required=100 must yield no recommendation")
	}
	tr2 := constTrace(1, 120, 0.0001, mib, Resources{CPURequest: 1, MemRequest: gib})
	k2 := &krr{cfg: DefaultKRR(), tr: tr2}
	cpu, mem, ok := k2.recommend(119)
	if !ok || cpu != 0.010 || mem != 100*mib {
		t.Fatalf("minimums: cpu=%v mem=%v ok=%v, want 10m / 100Mi", cpu, mem/mib, ok)
	}
}

func TestKRRHistoryWindowAndCadence(t *testing.T) {
	cfg := DefaultKRR()
	cfg.HistorySec = 3600 // 12 points
	cfg.PointsRequired = 1
	n := 48
	tr := fnTrace(1, n, func(_, i int) float64 {
		if i < 24 {
			return 2
		}
		return 0.2
	}, func(_, _ int) float64 { return 100 * mib }, Resources{CPURequest: 1, MemRequest: gib})
	k := &krr{cfg: cfg, tr: tr}
	if cpu, _, _ := k.recommend(n - 1); cpu != 0.2 {
		t.Fatalf("window must exclude points older than history: cpu=%v", cpu)
	}
	r, err := Run(tr, []Factory{NewStatic, NewKRR(cfg)}, Options{WarmupSec: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if m := metricsOf(t, r, "krr"); m.Changes != 1 {
		t.Fatalf("KRR re-applied more often than every 24h: %d changes", m.Changes)
	}
}

// ---- engine adapter ----

func TestEngineVMNeverServesTheFuture(t *testing.T) {
	tr := constTrace(2, 50, 0.3, 100*mib, Resources{CPURequest: 1, MemRequest: gib})
	s, err := newEngineStrategy(testEngineCfg(), tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.upto = 9
	q := url.Values{}
	q.Set("query", `rate(container_cpu_usage_seconds_total{namespace="backtest"}[5m])`)
	q.Set("start", "0")
	q.Set("end", strconv.FormatInt(tr.TimeAt(49), 10))
	res, err := http.Get(s.srv.URL + "/api/v1/query_range?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var out struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Result) != 2 {
		t.Fatalf("want one series per pod, got %d", len(out.Data.Result))
	}
	for _, r := range out.Data.Result {
		if len(r.Values) != 10 {
			t.Fatalf("served %d points, want 10 (ticks 0..9 only)", len(r.Values))
		}
		last := int64(r.Values[9][0].(float64))
		if last != tr.TimeAt(9) {
			t.Fatalf("timestamps must be trace time (no shift): %d", last)
		}
		if r.Metric["pod"] == "" || r.Metric["container"] != "app" {
			t.Fatalf("labels: %v", r.Metric)
		}
	}
}

// The real engine on flat usage converges to ~usage × (1+margin) with no
// over-request and no OOM, and the memory LIMIT is never lowered in place.
func TestEngineConvergesOnFlatUsage(t *testing.T) {
	tr := constTrace(3, 3*24*12, 0.3, 400*mib, Resources{CPURequest: 1, MemRequest: gib, MemLimit: gib})
	r, err := Run(tr, []Factory{NewStatic, NewEngine(testEngineCfg())}, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	m := metricsOf(t, r, "k8s-tailor")
	if m.CPUOverRequestHours != 0 || m.OOMEvents != 0 {
		t.Fatalf("flat usage must be fully covered: %+v", m)
	}
	if !near(m.Final.CPURequest, 0.345, 0.002) {
		t.Fatalf("cpu converged to %v, want ≈ 0.3×1.15", m.Final.CPURequest)
	}
	if !near(m.Final.MemRequest, 460*mib, 2*mib) || m.Final.MemLimit != gib {
		t.Fatalf("mem converged to %vMi lim %vMi, want ≈460Mi and limit kept at 1Gi", m.Final.MemRequest/mib, m.Final.MemLimit/mib)
	}
	if m.Changes != 1 {
		t.Fatalf("flat usage should settle with one resize, got %d", m.Changes)
	}
}

// An OOM in the simulation reaches the engine as a pod OOMKilled last state; the
// engine's learned floor must raise the memory LIMIT above the killer value.
func TestEngineLearnsFromSimulatedOOM(t *testing.T) {
	n := 2 * 24 * 12
	tr := fnTrace(2, n, func(_, _ int) float64 { return 0.2 },
		func(_, i int) float64 {
			if i > n*3/4 {
				return 600 * mib // spike above the 512Mi limit
			}
			return 300 * mib
		}, Resources{CPURequest: 0.5, MemRequest: 400 * mib, MemLimit: 512 * mib})
	cfg := testEngineCfg()
	cfg.RefreshEverySec = 1800
	r, err := Run(tr, []Factory{NewStatic, NewEngine(cfg)}, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	m := metricsOf(t, r, "k8s-tailor")
	if m.OOMEvents == 0 {
		t.Fatal("scenario should produce an OOM")
	}
	if m.Final.MemLimit < 512*mib*cfg.Base.OOMGrowthFactor-mib {
		t.Fatalf("OOM floor not applied: final limit %vMi, want >= %vMi", m.Final.MemLimit/mib, 512*cfg.Base.OOMGrowthFactor)
	}
	if st := metricsOf(t, r, "static"); m.MemOverCeilingHours >= st.MemOverCeilingHours {
		t.Fatalf("engine should spend less time above the ceiling than static (%v vs %v)", m.MemOverCeilingHours, st.MemOverCeilingHours)
	}
}

// reconcileDecision delegates to reconciler.Decide; this pins the adapter
// (units, quantization, limit floor) against the reconciler's rules.
func TestReconcileDecisionAdapter(t *testing.T) {
	entry := func(cpu, mem, memLim float64) recommender.Entry {
		return recommender.Entry{
			Rec: recommender.Recommendation{Confident: true,
				Recommended: recommender.Resources{CPURequest: cpu, MemRequest: mem, MemLimit: memLim}},
			Direction: policy.DirBoth, ScaleUpCooldown: 30 * time.Second, ScaleDownCooldown: time.Hour,
		}
	}
	cur := Resources{CPURequest: 1, MemRequest: 512 * mib, MemLimit: gib}
	now := int64(100000)
	// Under 15% drift on both → no resize.
	if _, act := reconcileDecision(entry(0.9, 480*mib, gib), cur, now, 0, false); act {
		t.Fatal("10% drift must not resize")
	}
	// Scale-down blocked by the 1h cooldown, allowed after it.
	if _, act := reconcileDecision(entry(0.5, 512*mib, gib), cur, now, now-1800, true); act {
		t.Fatal("scale-down inside the 1h cooldown")
	}
	next, act := reconcileDecision(entry(0.5, 512*mib, 600*mib), cur, now, now-3600, true)
	if !act || next.CPURequest != 0.5 {
		t.Fatalf("scale-down after cooldown: %+v %v", next, act)
	}
	if next.MemLimit != gib {
		t.Fatal("limit must never be lowered in place (limitFloor)")
	}
	// Scale-up only needs the 30s cooldown.
	if next, act := reconcileDecision(entry(2, 512*mib, gib), cur, now, now-60, true); !act || next.CPURequest != 2 {
		t.Fatal("scale-up must pass the short cooldown")
	}
	// Not confident → nothing.
	e := entry(0.1, 100*mib, gib)
	e.Rec.Confident = false
	if _, act := reconcileDecision(e, cur, now, 0, false); act {
		t.Fatal("unconfident recommendation actuated")
	}
	// OOM risk raises the limit even when requests didn't drift, keeping the request.
	e = entry(1, 512*mib, 1536*mib)
	e.Rec.OOMRisk = true
	next, act = reconcileDecision(e, cur, now, 0, false)
	if !act || next.MemLimit != 1536*mib || next.MemRequest != cur.MemRequest {
		t.Fatalf("OOM limit raise: %+v %v", next, act)
	}
}

func TestEngineRejectsExcludedNamespace(t *testing.T) {
	tr := constTrace(1, 10, 0.1, mib, Resources{CPURequest: 1, MemRequest: gib})
	tr.Namespace = "kube-system"
	if _, err := NewEngine(testEngineCfg())(tr); err == nil {
		t.Fatal("excluded namespace must be rejected (the engine would silently skip it)")
	}
}

// The memory-spikes scenario produces real OOMKills under the static baseline;
// the engine must learn the floor (raise the limit) and OOM no more than static.
func TestEngineOnMemorySpikes(t *testing.T) {
	tr, err := Generate("memory-spikes", 3, 14, 300)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testEngineCfg()
	cfg.RefreshEverySec = 3600
	r, err := Run(tr, []Factory{NewStatic, NewEngine(cfg)}, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	st, en := metricsOf(t, r, "static"), metricsOf(t, r, "k8s-tailor")
	t.Logf("OOMs static=%d engine=%d; final engine limit=%vMi request=%vMi", st.OOMEvents, en.OOMEvents, en.Final.MemLimit/mib, en.Final.MemRequest/mib)
	if st.OOMEvents == 0 {
		t.Fatal("static should OOM on this scenario")
	}
	if en.OOMEvents > st.OOMEvents {
		t.Fatalf("engine OOMs %d > static %d", en.OOMEvents, st.OOMEvents)
	}
	if en.Final.MemLimit <= tr.Initial.MemLimit {
		t.Fatalf("OOM floor never raised the limit: final %vMi", en.Final.MemLimit/mib)
	}
	// Regression: the same OOM (persisting in lastState) used to be refolded at
	// the raised limit every refresh, compounding the floor to absurd values.
	if en.Final.MemLimit > 4*tr.Initial.MemLimit {
		t.Fatalf("limit ran away to %vMi (floor compounding)", en.Final.MemLimit/mib)
	}
}

// P0-new-1 (2nd review): memory-spikes WITHOUT a memory limit. The OOM becomes a
// REQUEST increase (never a limit); before, it was recomputed from the live request
// (already inflated by Tailor itself) and grew ×OOM_GROWTH_FACTOR on every refresh
// without a new OOM. Post-OOM convergence: after the last simulated OOM the request
// rises at most once (the reaction increase) and stays bounded by a multiple of the peak.
func TestEngineNoLimitOOMConverges(t *testing.T) {
	tr, err := Generate("memory-spikes", 3, 14, 300)
	if err != nil {
		t.Fatal(err)
	}
	tr.Initial = Resources{CPURequest: 0.5, MemRequest: 512 * mib} // no limit
	// Template follows what was applied (apply: Rollout / GitOps): the "current request"
	// the engine reads is its own output, exactly where the compounding showed up.
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "inplace", true: "template-follows"}[follow], func(t *testing.T) {
			checkNoLimitOOMConverges(t, tr, follow)
		})
	}
}

func checkNoLimitOOMConverges(t *testing.T, tr *Trace, follow bool) {
	cfg := testEngineCfg()
	cfg.RefreshEverySec = 3600
	cfg.TemplateFollowsApplied = follow
	r, err := Run(tr, []Factory{NewStatic, NewEngine(cfg)}, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	st, en := metricsOf(t, r, "static"), metricsOf(t, r, "k8s-tailor")
	peak := 0.0
	for _, p := range tr.Pods {
		for _, v := range p.Mem {
			peak = math.Max(peak, v)
		}
	}
	t.Logf("OOMs static=%d engine=%d; final request=%vMi limit=%vMi; peak=%vMi; changes=%d",
		st.OOMEvents, en.OOMEvents, en.Final.MemRequest/mib, en.Final.MemLimit/mib, peak/mib, en.Changes)
	if en.Final.MemLimit != 0 {
		t.Fatalf("introduced limit %vMi on a container without a limit", en.Final.MemLimit/mib)
	}
	if en.OOMEvents > st.OOMEvents {
		t.Fatalf("engine OOMs %d > static %d", en.OOMEvents, st.OOMEvents)
	}
	if en.Final.MemRequest > 4*peak {
		t.Fatalf("request shot up to %vMi (> 4× peak %vMi): compounding", en.Final.MemRequest/mib, peak/mib)
	}
	// Rebuild the applied request per tick from the ChangeLog to find the
	// last OOM (usage > request, no limit) and count later increases.
	applied, ci, lastOOM := tr.Initial, 0, int64(-1)
	for i := 0; i < tr.Len(); i++ {
		ti := tr.TimeAt(i)
		for ci < len(en.ChangeLog) && en.ChangeLog[ci].T <= ti {
			applied = en.ChangeLog[ci].To
			ci++
		}
		for _, p := range tr.Pods {
			if !math.IsNaN(p.Mem[i]) && p.Mem[i] > applied.MemCeiling() {
				lastOOM = ti
			}
		}
	}
	ups, prev := 0, 0.0
	for _, c := range en.ChangeLog {
		if c.T > lastOOM && prev > 0 && c.To.MemRequest > prev {
			ups++
		}
		prev = c.To.MemRequest
	}
	if ups > 1 {
		t.Fatalf("request rose %d times after the last OOM (t=%d) without a new OOM — did not converge", ups, lastOOM)
	}
}
