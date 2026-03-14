// Package sim provides simulation abstractions for running NDNd under
// a discrete-event simulator such as ns-3. It replaces wall-clock time,
// real network I/O, and goroutine-based concurrency with callback-driven
// equivalents controlled by an external simulation engine.
package sim

import (
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
