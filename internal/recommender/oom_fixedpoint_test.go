package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// P0-new-1 (2nd review): for a container WITHOUT a limit that was OOMKilled, the
// request used to be recomputed as max(floor, max(peak, LIVE request) × g). The live
// request is already the value Tailor itself inflated in the previous refresh, so it grew
// ×1.5 on every refresh without a new OOM (256Mi → 17Gi in 6 refreshes; Pending pods).
// Property: N consecutive refreshes WITHOUT a new OOM (same learned floor)
// converge to a fixed point; the published value feeds the next refresh.
func TestOOMNoLimitRequestIsFixedPoint(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(1536*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	e := testEngine(1)
	e.cfg.OOMGrowthFactor = 1.5
	res := testResolved()
	floor := 256 * mib * 1.5 // learned ONCE (dedupe by finishedAt)
	req := 256.0 * mib
	var first float64
	for i := 0; i < 8; i++ {
		rec := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: req}, cpu, mem, 0, res, floor)
		got := rec.Recommended.MemRequest
		if rec.Recommended.MemLimit != 0 {
			t.Fatalf("refresh %d: introduced limit %vMi on a container without a limit", i, rec.Recommended.MemLimit/mib)
		}
		if got < 1536*mib {
			t.Fatalf("refresh %d: request %vMi below the 1536Mi peak", i, got/mib)
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("refresh %d: request %vMi != %vMi from the 1st refresh — compounding without a new OOM", i, got/mib, first/mib)
		}
		req = got
	}
	// Peak × growth factor (1536 × 1.5 = 2304Mi), never derived from the request.
	if first != RoundMemBytes(1536*mib*1.5) {
		t.Fatalf("request = %vMi, want 2304Mi (peak × g)", first/mib)
	}
}

// The path with an existing limit must not compound either: the floor raises the LIMIT
// once and the next refresh (snapshot already showing the raised limit) keeps it.
func TestOOMWithLimitIsFixedPoint(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(400*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	for _, allow := range []bool{false, true} {
		e := testEngine(1)
		e.cfg.OOMGrowthFactor = 1.5
		res := testResolved()
		res.AllowExceedLimit = allow
		floor := 512 * mib * 1.5
		c := k8s.Container{Name: "app", MemRequest: 256 * mib, MemLimit: 512 * mib}
		var first Resources
		for i := 0; i < 8; i++ {
			rec := e.recommend(testWL, c, cpu, mem, 0, res, floor)
			r := rec.Recommended
			if r.MemLimit < floor || r.MemLimit < r.MemRequest {
				t.Fatalf("allow=%v refresh %d: limit %vMi (req %vMi, floor %vMi)", allow, i, r.MemLimit/mib, r.MemRequest/mib, floor/mib)
			}
			if i == 0 {
				first = r
			} else if r.MemLimit != first.MemLimit || r.MemRequest != first.MemRequest {
				t.Fatalf("allow=%v refresh %d: %+v != %+v — compounding without a new OOM", allow, i, r, first)
			}
			c.MemRequest, c.MemLimit = r.MemRequest, r.MemLimit
		}
	}
}

// Default cap: the OOM increase on a container without a limit never exceeds
// peak × OOMRequestCapFactor, even with a much larger learned floor (several
// real OOMs from node pressure, each learned from the already inflated request).
func TestOOMNoLimitRequestCappedByPeakMultiple(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(512*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	e := testEngine(1)
	e.cfg.OOMGrowthFactor = 1.5
	res := testResolved()
	rec := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 8192 * mib}, cpu, mem, 0, res, 16384*mib)
	capB := RoundMemBytes(512 * mib * config.DefaultOOMMemCapFactor)
	if got := rec.Recommended.MemRequest; got > capB {
		t.Fatalf("request = %vMi, above the cap peak×%v = %vMi", got/mib, config.DefaultOOMMemCapFactor, capB/mib)
	}
	if got := rec.Recommended.MemRequest; got < 512*mib*1.5 {
		t.Fatalf("request = %vMi, below peak × g", got/mib)
	}
}

// OOM_MEM_CAP_FACTOR is configurable: with a learned floor far above the peak
// (several node-pressure kills, each relearned from an already-raised request),
// the request settles at exactly peak × cap for each configured cap, stays there
// across refreshes with no new OOM (fixed point: the published request feeds the
// next refresh), and a cap below the growth factor never undercuts peak × g.
func TestOOMNoLimitConfigurableCapFixedPoint(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(512*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	for _, tc := range []struct {
		capf, want float64 // want in MiB
	}{
		{0, 2048},   // unset (backtest, older callers) = default 4
		{2, 1024},   // tighter than the default
		{4, 2048},   // default
		{8, 4096},   // looser
		{1.2, 768},  // below g=1.5: peak × g still wins (cap = max(cap, g))
		{100, 8192}, // cap above the floor: the learned floor binds
	} {
		e := testEngine(1)
		e.cfg.OOMGrowthFactor = 1.5
		e.cfg.OOMMemCapFactor = tc.capf
		res := testResolved()
		floor := 8192.0 * mib
		req := 8192.0 * mib
		var first float64
		for i := 0; i < 6; i++ {
			got := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: req}, cpu, mem, 0, res, floor).Recommended.MemRequest
			if i == 0 {
				first = got
			} else if got != first {
				t.Fatalf("cap=%v refresh %d: %vMi != %vMi (not a fixed point)", tc.capf, i, got/mib, first/mib)
			}
			req = got
		}
		if first != RoundMemBytes(tc.want*mib) {
			t.Errorf("cap=%v: request = %vMi, want %vMi", tc.capf, first/mib, tc.want)
		}
	}
}

// The policy memory.max is applied AFTER the peak-multiple cap: whichever is
// lower wins, and a policy max above the cap does not loosen it.
func TestOOMNoLimitPolicyMaxAndCap(t *testing.T) {
	now := float64(time.Now().Unix())
	mem := series(now-100*300, 300, 100, constant(512*mib))
	cpu := series(now-100*300, 300, 100, constant(0.1))
	for _, tc := range []struct {
		max, want float64 // MiB; max 0 = no policy max
	}{
		{0, 2048},
		{1024, 1024}, // policy max below peak × cap wins
		{6144, 2048}, // policy max above the cap: the cap still binds
	} {
		e := testEngine(1)
		e.cfg.OOMGrowthFactor = 1.5
		e.cfg.OOMMemCapFactor = 4
		res := testResolved()
		res.Mem.Max = tc.max * mib
		got := e.recommend(testWL, k8s.Container{Name: "app", MemRequest: 256 * mib}, cpu, mem, 0, res, 16384*mib).Recommended.MemRequest
		if got != RoundMemBytes(tc.want*mib) {
			t.Errorf("max=%vMi: request = %vMi, want %vMi", tc.max, got/mib, tc.want)
		}
	}
}
