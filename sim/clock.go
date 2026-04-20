// Package sim provides simulation abstractions for running NDNd under
// a discrete-event simulator such as ns-3. It replaces wall-clock time,
// real network I/O, and goroutine-based concurrency with callback-driven
// equivalents controlled by an external simulation engine.
package sim

import (
	"container/heap"
	"sync"
	"time"
)

// EventID uniquely identifies a scheduled simulation event.
type EventID uint64

// Clock is the simulation time abstraction. In simulation mode, all
// time-related operations in NDNd go through this interface instead
// of the Go time package. The implementation is provided by the
// external simulator (e.g., ns-3 Simulator::Now / Simulator::Schedule).
type Clock interface {
	// Now returns the current simulation time.
	Now() time.Time

	// Schedule requests that callback be invoked after delay simulation time.
	// Returns an EventID that can be used to cancel the event.
	Schedule(delay time.Duration, callback func()) EventID

	// Cancel cancels a previously scheduled event. It is safe to cancel
	// an event that has already fired or been cancelled.
	Cancel(id EventID)
}

// --- Wall-clock implementation (production / testing) --------------------

// WallClock is a Clock backed by the real Go time package.
// Used for testing the simulation interfaces outside of ns-3.
type WallClock struct {
	mu     sync.Mutex
	nextID EventID
	timers map[EventID]*time.Timer
}

// NewWallClock creates a WallClock.
func NewWallClock() *WallClock {
	return &WallClock{
		timers: make(map[EventID]*time.Timer),
	}
}

func (c *WallClock) Now() time.Time {
	return time.Now()
}

func (c *WallClock) Schedule(delay time.Duration, callback func()) EventID {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	t := time.AfterFunc(delay, func() {
		c.mu.Lock()
		delete(c.timers, id)
		c.mu.Unlock()
		callback()
	})
	c.timers[id] = t
	c.mu.Unlock()
	return id
}

func (c *WallClock) Cancel(id EventID) {
	c.mu.Lock()
	if t, ok := c.timers[id]; ok {
		t.Stop()
		delete(c.timers, id)
	}
	c.mu.Unlock()
}

// --- Deterministic manual clock (tests) ---------------------------------

type scheduledEvent struct {
	id EventID
	at time.Time
	cb func()
}

// eventHeap implements heap.Interface for scheduledEvent (min-heap by time, then ID).
type eventHeap []scheduledEvent

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].id < h[j].id
	}
	return h[i].at.Before(h[j].at)
}
func (h eventHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)         { *h = append(*h, x.(scheduledEvent)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// DeterministicClock is a single-threaded manual clock for simulation tests.
// Call Advance() to move time forward and execute due callbacks.
type DeterministicClock struct {
	mu     sync.Mutex
	now    time.Time
	nextID EventID
	events eventHeap
}

func NewDeterministicClock(start time.Time) *DeterministicClock {
	return &DeterministicClock{now: start}
}

func (c *DeterministicClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *DeterministicClock) Schedule(delay time.Duration, callback func()) EventID {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID

	if delay < 0 {
		delay = 0
	}

	heap.Push(&c.events, scheduledEvent{
		id: id,
		at: c.now.Add(delay),
		cb: callback,
	})
	return id
}

func (c *DeterministicClock) Cancel(id EventID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, ev := range c.events {
		if ev.id == id {
			heap.Remove(&c.events, i)
			return
		}
	}
}

func (c *DeterministicClock) Advance(delta time.Duration) {
	c.mu.Lock()
	target := c.now.Add(delta)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		if c.events.Len() == 0 {
			c.now = target
			c.mu.Unlock()
			return
		}

		next := c.events[0]
		if next.at.After(target) {
			c.now = target
			c.mu.Unlock()
			return
		}

		heap.Pop(&c.events)
		c.now = next.at
		c.mu.Unlock()

		if next.cb != nil {
			next.cb()
		}
	}
}
