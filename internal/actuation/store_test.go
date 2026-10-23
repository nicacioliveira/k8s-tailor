package actuation

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type fakeSink struct {
	mu      sync.Mutex
	pushes  int
	records int
}

func (f *fakeSink) Push(_ context.Context, rs []map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes++
	f.records += len(rs)
	return nil
}

func ev(i int) Event {
	return Event{Time: int64(i), Mode: "inplace", Namespace: "ns", Kind: "Deployment", Workload: fmt.Sprintf("w%d", i), Container: "app"}
}

// P2: one goroutine + one INSERT per event. A rollout burst across a fleet
// meant thousands of goroutines and single-row INSERTs (ClickHouse hates them).
// Events must be batched by one background flusher.
func TestRecordBatchesSinkWrites(t *testing.T) {
	s := NewStore(100)
	sink := &fakeSink{}
	s.SetSink(sink)
	for i := 0; i < 1000; i++ {
		s.Record(ev(i))
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.records != 1000 {
		t.Fatalf("records pushed = %d, want 1000", sink.records)
	}
	// The flusher may race the explicit Flush (partial batches), but never one
	// INSERT per event.
	if sink.pushes > 20 {
		t.Fatalf("pushes = %d for 1000 events, want batched (<= 20)", sink.pushes)
	}
}

// P2: the per-key maps grew without bound (one entry per workload container ever
// actuated, including deleted ones).
func TestPerKeyMapsBounded(t *testing.T) {
	s := NewStore(10)
	s.maxKeys = 50
	for i := 0; i < 500; i++ {
		s.Record(ev(i))
	}
	if n := len(s.Counts()); n > 50 {
		t.Fatalf("per-key entries = %d, want <= 50", n)
	}
	// The most recent key survives eviction.
	if _, ok := s.Counts()[key("ns", "Deployment", "w499", "app")]; !ok {
		t.Fatal("most recent key evicted")
	}
}

// The pending queue is bounded too (a dead sink must not grow memory forever).
func TestPendingQueueBounded(t *testing.T) {
	s := NewStore(10)
	s.maxPending = 100
	s.mu.Lock()
	s.sink = &fakeSink{} // attached without a flusher: nothing drains
	s.mu.Unlock()
	for i := 0; i < 1000; i++ {
		s.Record(ev(i))
	}
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n > 100 {
		t.Fatalf("pending = %d, want <= 100", n)
	}
}
