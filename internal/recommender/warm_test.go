package recommender

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

func warmTestEntry(mode policy.Mode, confident bool) Entry {
	return Entry{
		Rec: Recommendation{
			Namespace: "apps", Kind: "Deployment", Workload: "web", Container: "app",
			Confident: confident, CPUConfident: confident, MemConfident: confident,
			Recommended:  Resources{CPURequest: 0.25, CPULimit: 0.5, MemRequest: 256 * mib, MemLimit: 512 * mib},
			Signals:      Signals{CPUCensored: true},
			CPUHeldByHPA: true,
		},
		Mode: mode, Apply: policy.ApplyInPlace, Direction: policy.DirDown, OOMOverridesDirection: true,
		EnforceMemLimit: true, GuaranteedResize: true, Class: k8s.ClassApp,
		CPUTrend: []Sample{{T: 1, V: 1}}, // big series are never persisted
	}
}

func warmEngine(ttl time.Duration, now *time.Time) *Engine {
	e := NewEngine(config.Config{WarmCache: true, WarmTTL: ttl, SelfNamespace: "k8s-tailor"}, nil, nil, nil, nil)
	e.SetClock(func() time.Time { return *now })
	return e
}

// The warm cache carries what the webhook needs, only for containers the webhook
// can act on with a confident recommendation; a restarted engine serves it at once
// but the entries never reach the reconciler (Get) until a real refresh replaces them.
func TestWarmRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := warmEngine(15*time.Minute, &now)
	good := warmTestEntry(policy.ModeBoth, true)
	a.Seed(nil, map[string]Entry{
		Key("apps", "Deployment", "web", "app"):     good,
		Key("apps", "Deployment", "rec", "app"):     warmTestEntry(policy.ModeRecommend, true), // nothing to admit
		Key("apps", "Deployment", "young", "app"):   warmTestEntry(policy.ModeWebhook, false),  // not confident
		Key("apps", "Deployment", "inplace", "app"): warmTestEntry(policy.ModeInPlace, true),   // reconciler only
	})
	blob, n, err := a.warmPayload()
	if err != nil || n != 1 {
		t.Fatalf("payload: n=%d err=%v, want exactly the one webhook-capable confident entry", n, err)
	}

	b := warmEngine(15*time.Minute, &now)
	if b.Ready() || b.Warm() {
		t.Fatal("a fresh engine must not be ready")
	}
	if got, err := b.loadWarmPayload(blob); err != nil || got != 1 {
		t.Fatalf("load: %d entries, err %v", got, err)
	}
	if !b.Ready() || b.Warm() {
		t.Fatalf("after load: Ready=%v Warm=%v, want true / false (only a real refresh makes it Warm)", b.Ready(), b.Warm())
	}
	got, ok := b.Admission("apps", "Deployment", "web", "app")
	if !ok {
		t.Fatal("the webhook cannot see the loaded entry")
	}
	if got.Rec.Recommended != good.Rec.Recommended || got.Mode != policy.ModeBoth || got.Direction != policy.DirDown ||
		!got.OOMOverridesDirection || !got.EnforceMemLimit || !got.GuaranteedResize || !got.Rec.CPUHeldByHPA ||
		!got.Rec.Confident || !got.Rec.Signals.CPUCensored || got.Class != k8s.ClassApp {
		t.Fatalf("round trip lost a field the plan reads: %+v", got)
	}
	if len(got.CPUTrend) != 0 {
		t.Fatal("a trend series was persisted")
	}
	if _, ok := b.Get("apps", "Deployment", "web", "app"); ok {
		t.Fatal("a loaded entry is visible to the reconciler: it would act in place on stale data")
	}
	if len(b.List()) != 0 {
		t.Fatal("loaded entries leaked into the recommendation list (UI/API)")
	}

	// The first real refresh replaces the loaded entries wholesale.
	b.Seed(nil, map[string]Entry{})
	if _, ok := b.Admission("apps", "Deployment", "web", "app"); ok {
		t.Fatal("loaded entry survived a real refresh")
	}
}

func TestWarmRejectsStaleOrDamagedPayload(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := warmEngine(15*time.Minute, &now)
	a.Seed(nil, map[string]Entry{Key("apps", "Deployment", "web", "app"): warmTestEntry(policy.ModeWebhook, true)})
	blob, _, _ := a.warmPayload()

	t.Run("older than the TTL", func(t *testing.T) {
		later := now.Add(16 * time.Minute)
		b := warmEngine(15*time.Minute, &later)
		if n, err := b.loadWarmPayload(blob); err == nil || n != 0 || b.Ready() {
			t.Fatalf("stale payload accepted: n=%d err=%v ready=%v", n, err, b.Ready())
		}
	})
	t.Run("TTL 0 disables it", func(t *testing.T) {
		b := warmEngine(0, &now)
		if n, _ := b.loadWarmPayload(blob); n != 0 || b.Ready() {
			t.Fatal("WARM_TTL=0 still loaded entries")
		}
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		var f map[string]any
		_ = json.Unmarshal(blob, &f)
		f["entries"].(map[string]any)[Key("apps", "Deployment", "web", "app")].(map[string]any)["cpuReq"] = 99.0
		bad, _ := json.Marshal(f)
		b := warmEngine(15*time.Minute, &now)
		if n, err := b.loadWarmPayload(bad); err == nil || n != 0 {
			t.Fatalf("tampered payload accepted: n=%d err=%v", n, err)
		}
	})
	t.Run("unknown version", func(t *testing.T) {
		var f map[string]any
		_ = json.Unmarshal(blob, &f)
		f["version"] = 99
		bad, _ := json.Marshal(f)
		b := warmEngine(15*time.Minute, &now)
		if n, err := b.loadWarmPayload(bad); err == nil || n != 0 {
			t.Fatalf("future version accepted: n=%d err=%v", n, err)
		}
	})
	t.Run("expires in memory too", func(t *testing.T) {
		clock := now
		b := warmEngine(15*time.Minute, &clock)
		if _, err := b.loadWarmPayload(blob); err != nil {
			t.Fatal(err)
		}
		clock = now.Add(15*time.Minute + time.Second)
		if _, ok := b.Admission("apps", "Deployment", "web", "app"); ok || b.Ready() {
			t.Fatal("a loaded entry outlived its TTL while no refresh succeeded")
		}
	})
}

// Through the ConfigMap: the leader's Save and a restarted replica's Load.
func TestWarmConfigMapSaveLoad(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cs := fake.NewSimpleClientset()
	kc := k8s.NewFromClients(cs, nil, nil)
	mk := func() *Engine {
		e := warmEngine(15*time.Minute, &now)
		e.k8s = kc
		return e
	}
	a := mk()
	a.Seed(nil, map[string]Entry{Key("apps", "Deployment", "web", "app"): warmTestEntry(policy.ModeWebhook, true)})
	ctx := context.Background()
	if err := a.SaveWarm(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveWarm(ctx); err != nil { // unchanged: no second write inside TTL/3
		t.Fatal(err)
	}
	writes := 0
	for _, act := range cs.Actions() {
		if v := act.GetVerb(); v == "create" || v == "update" {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("%d writes for two identical saves, want 1", writes)
	}
	b := mk()
	n, err := b.LoadWarm(ctx)
	if err != nil || n != 1 || !b.Ready() {
		t.Fatalf("load: n=%d err=%v ready=%v", n, err, b.Ready())
	}
	// Absent ConfigMap or the opt-out: nothing loaded, no error.
	c := warmEngine(15*time.Minute, &now)
	c.k8s = k8s.NewFromClients(fake.NewSimpleClientset(), nil, nil)
	if n, err := c.LoadWarm(ctx); err != nil || n != 0 || c.Ready() {
		t.Fatalf("empty cluster: n=%d err=%v ready=%v", n, err, c.Ready())
	}
	d := mk()
	d.cfg.WarmCache = false
	if n, _ := d.LoadWarm(ctx); n != 0 || d.Ready() {
		t.Fatal("WARM_CACHE=false still loaded")
	}
	// Saving never happens before a real refresh (nothing trustworthy to save).
	f := mk()
	f.k8s = k8s.NewFromClients(fake.NewSimpleClientset(), nil, nil)
	if err := f.SaveWarm(ctx); err != nil {
		t.Fatal(err)
	}
	for _, act := range f.k8s.Clientset().(*fake.Clientset).Actions() {
		if act.GetVerb() == "create" {
			t.Fatal("an engine without a refresh wrote the cache")
		}
	}
}

// A refresh in which EVERY namespace query failed (the metrics backend is down) used
// to "succeed" with zero recommendations and replace the whole state: the engine
// forgot everything it knew, a webhook served nothing, and the warm cache would have
// been rewritten empty. It is an error now and changes nothing.
func TestRefreshKeepsStateWhenEveryNamespaceFails(t *testing.T) {
	e, calls, _ := seasonEngine(t, "daily", config.Config{})
	ctx := context.Background()
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before, ok := e.Get("apps", "Deployment", "web", "app")
	if !ok || len(e.List()) != 1 {
		t.Fatalf("precondition: one recommendation after a good refresh (ok=%v n=%d)", ok, len(e.List()))
	}
	_ = calls

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	defer down.Close()
	e.vm = vm.New(down.URL)
	if err := e.Refresh(ctx); err == nil {
		t.Fatal("a refresh where every namespace query failed reported success")
	}
	after, ok := e.Get("apps", "Deployment", "web", "app")
	if !ok || after.Rec.Recommended != before.Rec.Recommended || len(e.List()) != 1 || !e.Warm() {
		t.Fatalf("the failed refresh changed the state: ok=%v warm=%v n=%d", ok, e.Warm(), len(e.List()))
	}
}

// After a restart the loaded entries survive a first refresh that fails.
func TestWarmEntriesSurviveAFailedFirstRefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	src := warmEngine(15*time.Minute, &now)
	src.Seed(nil, map[string]Entry{Key("apps", "Deployment", "web", "app"): warmTestEntry(policy.ModeWebhook, true)})
	blob, _, _ := src.warmPayload()

	e, _, _ := seasonEngine(t, "daily", config.Config{WarmCache: true, WarmTTL: 15 * time.Minute, SelfNamespace: "k8s-tailor"})
	e.SetClock(func() time.Time { return now })
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	defer down.Close()
	e.vm = vm.New(down.URL)
	if _, err := e.loadWarmPayload(blob); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("refresh against a dead backend succeeded")
	}
	if _, ok := e.Admission("apps", "Deployment", "web", "app"); !ok || !e.Ready() || e.Warm() {
		t.Fatalf("loaded entries lost to a failed refresh: admission=%v ready=%v warm=%v", ok, e.Ready(), e.Warm())
	}
}
