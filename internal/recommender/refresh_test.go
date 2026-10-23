package recommender

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

// A burst of SetWorkload-triggered refreshes must coalesce: while one refresh
// runs, any number of triggers queue at most ONE follow-up, refreshes never
// overlap, and each runs under a deadline.
func TestTriggerRefreshCoalescesAndBounds(t *testing.T) {
	e := &Engine{cfg: config.Config{RefreshEvery: time.Second}}
	release := make(chan struct{})
	var calls, active, overlap atomic.Int32
	var noDeadline atomic.Bool
	e.refreshImpl = func(ctx context.Context) error {
		if active.Add(1) > 1 {
			overlap.Store(1)
		}
		defer active.Add(-1)
		if _, ok := ctx.Deadline(); !ok {
			noDeadline.Store(true)
		}
		calls.Add(1)
		<-release
		return nil
	}
	e.TriggerRefresh()
	waitUntil(t, func() bool { return calls.Load() == 1 }) // first one running
	for i := 0; i < 50; i++ {
		e.TriggerRefresh() // burst while running → exactly one queued
	}
	close(release)
	waitUntil(t, func() bool { return calls.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("refreshes = %d, want 2 (running + one coalesced)", n)
	}
	if overlap.Load() != 0 {
		t.Fatal("refreshes overlapped")
	}
	if noDeadline.Load() {
		t.Fatal("triggered refresh ran without a deadline")
	}
	if e.RefreshTimeout() != 30*time.Second {
		t.Fatalf("timeout floor = %v", e.RefreshTimeout())
	}
}

// The periodic Refresh and a triggered one are serialized too.
func TestRefreshSerialized(t *testing.T) {
	e := &Engine{cfg: config.Config{RefreshEvery: time.Minute}}
	var active, maxActive atomic.Int32
	e.refreshImpl = func(ctx context.Context) error {
		n := active.Add(1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		return nil
	}
	done := make(chan struct{})
	go func() { _ = e.Refresh(context.Background()); close(done) }()
	e.TriggerRefresh()
	_ = e.Refresh(context.Background())
	<-done
	time.Sleep(100 * time.Millisecond)
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrent refreshes = %d, want 1", maxActive.Load())
	}
	if e.RefreshTimeout() != 2*time.Minute {
		t.Fatalf("timeout = %v, want 2m", e.RefreshTimeout())
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
