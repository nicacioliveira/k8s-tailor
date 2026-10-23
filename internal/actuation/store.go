// Package actuation records what the mutating webhook actually did, so the UI
// and metrics can show the decisions (not just the recommendations).
package actuation

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Sink durably persists decision records (implemented by the ClickHouse
// client). Kept as an interface so this package doesn't depend on the CH client.
type Sink interface {
	Push(ctx context.Context, records []map[string]any) error
}

// Event is a single applied mutation.
type Event struct {
	Time      int64   `json:"time"` // unix seconds
	Mode      string  `json:"mode"` // "webhook" (on create) | "inplace" (live resize)
	Namespace string  `json:"namespace"`
	Kind      string  `json:"kind"`
	Workload  string  `json:"workload"`
	Container string  `json:"container"`
	FromCPU   float64 `json:"fromCpu"` // cores
	ToCPU     float64 `json:"toCpu"`
	FromMem   float64 `json:"fromMem"` // bytes
	ToMem     float64 `json:"toMem"`
}

func key(ns, kind, wl, c string) string { return ns + "/" + kind + "/" + wl + "/" + c }

// Sink batching: ONE background flusher pushes pending records every
// sinkFlushEvery, or as soon as sinkBatch are queued — never a goroutine and a
// single-row INSERT per event. The queue is bounded (oldest dropped).
const (
	sinkBatch      = 500
	sinkFlushEvery = 2 * time.Second
	defMaxPending  = 10000
	defMaxKeys     = 20000 // per-key counters (workload containers ever actuated)
)

// Store is a thread-safe ring buffer of recent events plus per-key counters.
// When a Sink is set, events are also batched to the durable decision log.
type Store struct {
	mu         sync.RWMutex
	max        int
	events     []Event
	counts     map[string]int
	last       map[string]int64 // key -> last mutation unix seconds
	maxKeys    int
	sink       Sink
	pending    []map[string]any
	maxPending int
	dropped    int // records dropped because the queue was full
	kick       chan struct{}
	flushMu    sync.Mutex // serializes pushes (flusher vs Flush)
}

func NewStore(max int) *Store {
	return &Store{max: max, counts: map[string]int{}, last: map[string]int64{},
		maxKeys: defMaxKeys, maxPending: defMaxPending, kick: make(chan struct{}, 1)}
}

// SetSink attaches a durable decision store (ClickHouse) and starts the single
// background flusher. Safe to leave nil, and safe to call from a background
// goroutine.
func (s *Store) SetSink(sink Sink) {
	s.mu.Lock()
	start := s.sink == nil && sink != nil
	s.sink = sink
	s.mu.Unlock()
	if start {
		go s.flushLoop()
	}
}

func (s *Store) flushLoop() {
	t := time.NewTicker(sinkFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.kick:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.Flush(ctx)
		cancel()
	}
}

// Flush pushes every pending record to the sink, in batches of sinkBatch.
// A failed batch is dropped (the decision log is best-effort; the in-memory
// ring and the Kubernetes Events remain the source of truth).
func (s *Store) Flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	var firstErr error
	for {
		s.mu.Lock()
		sink := s.sink
		n := len(s.pending)
		if n > sinkBatch {
			n = sinkBatch
		}
		batch := s.pending[:n:n]
		s.pending = s.pending[n:]
		s.mu.Unlock()
		if sink == nil || n == 0 {
			return firstErr
		}
		if err := sink.Push(ctx, batch); err != nil && firstErr == nil {
			firstErr = err
		}
	}
}

func (s *Store) Record(e Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	if len(s.events) > s.max {
		s.events = s.events[len(s.events)-s.max:]
	}
	k := key(e.Namespace, e.Kind, e.Workload, e.Container)
	s.counts[k]++
	s.last[k] = e.Time
	if s.maxKeys > 0 && len(s.counts) > s.maxKeys {
		s.evictLocked()
	}
	kick := false
	if s.sink != nil {
		s.pending = append(s.pending, e.record())
		if s.maxPending > 0 && len(s.pending) > s.maxPending {
			drop := len(s.pending) - s.maxPending
			s.dropped += drop
			s.pending = append([]map[string]any(nil), s.pending[drop:]...)
		}
		kick = len(s.pending) >= sinkBatch
	}
	s.mu.Unlock()
	if kick {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// evictLocked drops the least-recently-actuated ~10% of per-key counters.
func (s *Store) evictLocked() {
	type kv struct {
		k string
		t int64
	}
	all := make([]kv, 0, len(s.last))
	for k, t := range s.last {
		all = append(all, kv{k, t})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t < all[j].t })
	n := len(s.counts) - s.maxKeys + s.maxKeys/10
	for i := 0; i < n && i < len(all); i++ {
		delete(s.counts, all[i].k)
		delete(s.last, all[i].k)
	}
}

// record projects the event into a ClickHouse decisions row (keys match the
// table columns; ts formatted for DateTime64(3) without best_effort parsing).
func (e Event) record() map[string]any {
	dir := "reduced"
	if e.ToCPU > e.FromCPU || e.ToMem > e.FromMem {
		dir = "raised"
	}
	return map[string]any{
		"ts": time.Unix(e.Time, 0).UTC().Format("2006-01-02 15:04:05.000"),
		"msg": e.Mode + " " + e.Namespace + "/" + e.Workload + "[" + e.Container + "] " +
			mStr(e.FromCPU*1000) + "m->" + mStr(e.ToCPU*1000) + "m cpu, " +
			mStr(e.FromMem/(1024*1024)) + "Mi->" + mStr(e.ToMem/(1024*1024)) + "Mi mem",
		"mode": e.Mode, "namespace": e.Namespace, "kind": e.Kind,
		"workload": e.Workload, "container": e.Container, "direction": dir,
		"from_cpu": e.FromCPU, "to_cpu": e.ToCPU, "from_mem": e.FromMem, "to_mem": e.ToMem,
	}
}

func mStr(v float64) string {
	return strconv.FormatInt(int64(v+0.5), 10)
}

// Recent returns up to n most-recent events, newest first.
func (s *Store) Recent(n int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, 0, n)
	for i := len(s.events) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.events[i])
	}
	return out
}

// EventsFor returns all events for a specific workload container, oldest first.
func (s *Store) EventsFor(ns, kind, wl, c string) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	for _, e := range s.events {
		if e.Namespace == ns && e.Kind == kind && e.Workload == wl && e.Container == c {
			out = append(out, e)
		}
	}
	return out
}

type Stat struct {
	Count int   `json:"count"`
	Last  int64 `json:"last"`
}

// Counts returns per-key mutation counts and last-applied time.
func (s *Store) Counts() map[string]Stat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Stat, len(s.counts))
	for k, c := range s.counts {
		out[k] = Stat{Count: c, Last: s.last[k]}
	}
	return out
}

// Snapshot returns everything needed by the /metrics endpoint.
func (s *Store) Snapshot() (total int, perKey map[string]int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	perKey = make(map[string]int, len(s.counts))
	for k, c := range s.counts {
		perKey[k] = c
		total += c
	}
	return
}

func Now() int64 { return time.Now().Unix() }
