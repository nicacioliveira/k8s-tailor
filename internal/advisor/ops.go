package advisor

import (
	"container/list"
	"sync"
	"time"
)

// ---- LRU + TTL cache -----------------------------------------------------

type cacheEntry struct {
	key     string
	val     any
	expires time.Time
}

type lruCache struct {
	mu  sync.Mutex
	cap int
	ttl time.Duration
	now func() time.Time
	ll  *list.List
	m   map[string]*list.Element
}

func newLRU(capacity int, ttl time.Duration, now func() time.Time) *lruCache {
	return &lruCache{cap: capacity, ttl: ttl, now: now, ll: list.New(), m: map[string]*list.Element{}}
}

func (c *lruCache) get(key string) (any, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if !c.now().Before(e.expires) {
		c.ll.Remove(el)
		delete(c.m, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

func (c *lruCache) put(key string, val any) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(c.ttl)
	if el, ok := c.m[key]; ok {
		e := el.Value.(*cacheEntry)
		e.val, e.expires = val, exp
		c.ll.MoveToFront(el)
		return
	}
	c.m[key] = c.ll.PushFront(&cacheEntry{key: key, val: val, expires: exp})
	for c.ll.Len() > c.cap {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*cacheEntry).key)
	}
}

func (c *lruCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// ---- sliding window (rate limit and token budget) --------------------------

type winEvent struct {
	t time.Time
	n int
}

type slidingWindow struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	ev     []winEvent
}

func newWindow(d time.Duration, now func() time.Time) *slidingWindow {
	return &slidingWindow{window: d, now: now}
}

func (w *slidingWindow) pruneLocked() {
	cut := w.now().Add(-w.window)
	i := 0
	for i < len(w.ev) && !w.ev[i].t.After(cut) {
		i++
	}
	w.ev = w.ev[i:]
}

func (w *slidingWindow) sum() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	s := 0
	for _, e := range w.ev {
		s += e.n
	}
	return s
}

func (w *slidingWindow) add(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	w.ev = append(w.ev, winEvent{t: w.now(), n: n})
}

// tryAdd adds n only if the running sum stays <= limit.
func (w *slidingWindow) tryAdd(n, limit int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	s := 0
	for _, e := range w.ev {
		s += e.n
	}
	if s+n > limit {
		return false
	}
	w.ev = append(w.ev, winEvent{t: w.now(), n: n})
	return true
}

// ---- circuit breaker ---------------------------------------------------------

type breakerState int

const (
	stClosed breakerState = iota
	stOpen
	stHalfOpen
)

func (s breakerState) String() string {
	return [...]string{"closed", "open", "half-open"}[s]
}

type breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	state     breakerState
	fails     int
	openedAt  time.Time
	probing   bool
}

func newBreaker(threshold int, cooldown time.Duration, now func() time.Time) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: now}
}

// allow reports whether a call may proceed. In half-open state exactly one
// probe is admitted.
func (b *breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return ErrCircuitOpen
		}
		b.state, b.probing = stHalfOpen, true
		return nil
	case stHalfOpen:
		if b.probing {
			return ErrCircuitOpen
		}
		b.probing = true
	}
	return nil
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.fails, b.probing = stClosed, 0, false
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	b.fails++
	if b.state == stHalfOpen || b.fails >= b.threshold {
		b.state, b.openedAt = stOpen, b.now()
	}
}

// release frees a half-open probe slot when the call ended for a reason that
// says nothing about provider health (caller cancellation, local limits).
func (b *breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == stHalfOpen {
		b.probing = false
	}
}

func (b *breaker) current() breakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == stOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		return stHalfOpen
	}
	return b.state
}
