package recommender

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

func newOOMEngine(growth float64, ttl time.Duration) *Engine {
	return &Engine{
		cfg:      config.Config{OOMGrowthFactor: growth, OOMFloorTTL: ttl},
		oomFloor: map[string]oomFloorEntry{},
	}
}

const oomKey = "apps-x/Deployment/web/app"

// TestLearnOOMGrowth: an OOMKill at limit L is re-protected at L×growth.
func TestLearnOOMGrowth(t *testing.T) {
	e := newOOMEngine(1.5, 30*24*time.Hour)
	e.learnOOM(map[string]k8s.OOMObservation{
		oomKey: {MemBytes: 1000, At: time.Now()},
	})
	if got := e.oomFloorFor(oomKey); got != 1500 {
		t.Fatalf("floor = %v, want 1500 (1000×1.5)", got)
	}
	if got := e.oomFloorFor("apps-x/Deployment/other/app"); got != 0 {
		t.Fatalf("unseen workload floor = %v, want 0", got)
	}
}

// TestLearnOOMNeverLowersActiveFloor: a later, SMALLER OOM must not shrink an
// active floor — an OOM is proof memory was insufficient; we don't un-learn it
// while it's fresh. A later, LARGER OOM raises it.
func TestLearnOOMNeverLowersActiveFloor(t *testing.T) {
	e := newOOMEngine(1.5, 30*24*time.Hour)
	now := time.Now()
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 2000, At: now}}) // floor 3000
	// Each OOM is a distinct kill (later FinishedAt); the same kill is learned once
	// (TestLearnOOMSameEventNotCompounded).
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: now.Add(time.Second)}}) // 1500 < 3000
	if got := e.oomFloorFor(oomKey); got != 3000 {
		t.Fatalf("floor lowered to %v, want held at 3000", got)
	}
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 4000, At: now.Add(2 * time.Second)}}) // 6000 > 3000
	if got := e.oomFloorFor(oomKey); got != 6000 {
		t.Fatalf("floor = %v, want raised to 6000", got)
	}
}

// TestLearnOOMExpiry: a floor is forgotten once no OOM has fed it for the TTL, so
// a fixed workload resumes normal sizing.
func TestLearnOOMExpiry(t *testing.T) {
	e := newOOMEngine(1.5, 30*24*time.Hour)
	old := time.Now().Add(-31 * 24 * time.Hour) // older than the 30d TTL
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: old}})
	if got := e.oomFloorFor(oomKey); got != 0 {
		t.Fatalf("expired floor = %v, want 0", got)
	}
	if _, ok := e.oomFloor[oomKey]; ok {
		t.Fatalf("expired floor should have been pruned from the map")
	}
}

// TestLearnOOMIgnoresZero: an observation with no learnable value is dropped.
func TestLearnOOMIgnoresZero(t *testing.T) {
	e := newOOMEngine(1.5, 30*24*time.Hour)
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 0, At: time.Now()}})
	if got := e.oomFloorFor(oomKey); got != 0 {
		t.Fatalf("zero-value observation produced floor %v, want 0", got)
	}
}

// Found by the memory-spikes backtest: after the floor raises the limit in
// place, the pod's lastState still says OOMKilled — and the snapshot reports the
// NEW (raised) limit as "the value that was hit". Folding that again compounded
// the floor ×growth on every refresh (to absurd limits). The same OOM event
// (same FinishedAt) must be learned once.
func TestLearnOOMSameEventNotCompounded(t *testing.T) {
	e := newOOMEngine(1.5, 30*24*time.Hour)
	at := time.Now().Add(-time.Minute)
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1000, At: at}}) // floor 1500
	for i := 0; i < 5; i++ {                                                    // limit now 1500, lastState unchanged
		e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: e.oomFloorFor(oomKey), At: at}})
	}
	if got := e.oomFloorFor(oomKey); got != 1500 {
		t.Fatalf("floor compounded to %v, want 1500", got)
	}
	// A NEW OOM (later) at the raised limit does raise it.
	e.learnOOM(map[string]k8s.OOMObservation{oomKey: {MemBytes: 1500, At: at.Add(time.Second)}})
	if got := e.oomFloorFor(oomKey); got != 2250 {
		t.Fatalf("new OOM floor = %v, want 2250", got)
	}
}
