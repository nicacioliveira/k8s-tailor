package leader

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// Two replicas contend for one Lease: exactly one runs the work at a time, and
// when the leader shuts down (releasing the Lease) the standby takes over.
func TestSingleActiveLeaderAndFailover(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var active, maxActive, runs atomic.Int32
	work := func(ctx context.Context) {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		runs.Add(1)
		<-ctx.Done()
		active.Add(-1)
	}
	opts := func(id string) Options {
		return Options{Namespace: "k8s-tailor", Name: "k8s-tailor-actuator", Identity: id,
			LeaseDuration: 1500 * time.Millisecond, RenewDeadline: time.Second, RetryPeriod: 100 * time.Millisecond}
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = Run(ctxA, cs, opts("a"), work) }()
	// Let "a" win deterministically before "b" starts contending.
	waitFor(t, func() bool { return runs.Load() == 1 })
	go func() { defer wg.Done(); _ = Run(ctxB, cs, opts("b"), work) }()

	time.Sleep(500 * time.Millisecond)
	if runs.Load() != 1 || active.Load() != 1 {
		t.Fatalf("while a leads: runs=%d active=%d, want 1/1", runs.Load(), active.Load())
	}
	cancelA() // leader shuts down → releases → b takes over
	waitFor(t, func() bool { return runs.Load() == 2 && active.Load() == 1 })
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrently active = %d, want 1", maxActive.Load())
	}
	cancelB()
	wg.Wait()
	if active.Load() != 0 {
		t.Fatalf("work still active after shutdown: %d", active.Load())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// Shutdown of the leader (helm upgrade / SIGTERM): the standby must take over
// in ~RetryPeriod (the Lease is RELEASED, not left to expire), and only AFTER
// the old leader's work has returned — a reconciler still finishing a resize
// pass must never overlap the new leader's first pass.
func TestShutdownReleasesLeaseAfterWorkStops(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var active, maxActive, runs atomic.Int32
	var aStopped atomic.Int64 // unix nanos when a's work returned
	var bStarted atomic.Int64
	mkWork := func(id string, drain time.Duration) func(ctx context.Context) {
		return func(ctx context.Context) {
			n := active.Add(1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			if id == "b" {
				bStarted.Store(time.Now().UnixNano())
			}
			runs.Add(1)
			<-ctx.Done()
			time.Sleep(drain) // finishing an in-flight pass
			if id == "a" {
				aStopped.Store(time.Now().UnixNano())
			}
			active.Add(-1)
		}
	}
	// LeaseDuration long on purpose: a takeover by EXPIRY would take >= 10s.
	opts := func(id string) Options {
		return Options{Namespace: "k8s-tailor", Name: "k8s-tailor-actuator", Identity: id,
			LeaseDuration: 10 * time.Second, RenewDeadline: 5 * time.Second, RetryPeriod: 100 * time.Millisecond}
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneA := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer close(doneA); _ = Run(ctxA, cs, opts("a"), mkWork("a", 400*time.Millisecond)) }()
	waitFor(t, func() bool { return runs.Load() == 1 })
	go func() { defer wg.Done(); _ = Run(ctxB, cs, opts("b"), mkWork("b", 0)) }()
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	cancelA()
	select {
	case <-doneA: // Run returns only after the lease was released
	case <-time.After(5 * time.Second):
		t.Fatal("Run(a) did not return after cancel")
	}
	waitFor(t, func() bool { return runs.Load() == 2 })
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("failover took %s; the lease was not released on shutdown", took)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("old and new leader overlapped (max active=%d)", maxActive.Load())
	}
	if bStarted.Load() < aStopped.Load() {
		t.Fatal("standby started before the old leader's work returned")
	}
	cancelB()
	wg.Wait()
}
