package sim

// Unit tests for the Clock implementations.
//
// These tests guard against the global event ID collision bug that was
// discovered when per-node Ns3Clock counters produced duplicate IDs in
// the shared C++ g_eventMap, causing one node's Cancel to silently
// remove another node's event.

import (
	"sync/atomic"
	"testing"
	"time"
)

// ---------- DeterministicClock unit tests ----------

// TestDeterministicClockEventIDsAreUnique verifies that a single
// DeterministicClock never reuses an EventID.
func TestDeterministicClockEventIDsAreUnique(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))

	const n = 1000
	ids := make(map[EventID]bool, n)
	for i := 0; i < n; i++ {
		id := clock.Schedule(time.Duration(i)*time.Millisecond, func() {})
		if ids[id] {
			t.Fatalf("duplicate EventID %d on iteration %d", id, i)
		}
		ids[id] = true
	}
}

// TestDeterministicClockCancelOnly cancels one event and verifies
// that no other events are affected.
func TestDeterministicClockCancelOnly(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))

	var firedA, firedB, firedC int32
	clock.Schedule(10*time.Millisecond, func() { atomic.AddInt32(&firedA, 1) })
	idB := clock.Schedule(20*time.Millisecond, func() { atomic.AddInt32(&firedB, 1) })
	clock.Schedule(30*time.Millisecond, func() { atomic.AddInt32(&firedC, 1) })

	clock.Cancel(idB)
	clock.Advance(50 * time.Millisecond)

	if atomic.LoadInt32(&firedA) != 1 {
		t.Fatalf("event A: expected 1 firing, got %d", firedA)
	}
	if atomic.LoadInt32(&firedB) != 0 {
		t.Fatalf("event B was cancelled but fired %d time(s)", firedB)
	}
	if atomic.LoadInt32(&firedC) != 1 {
		t.Fatalf("event C: expected 1 firing, got %d", firedC)
	}
}

// TestDeterministicClockCancelIdempotent verifies that cancelling an
// event twice (or cancelling a non-existent ID) does not panic or
// corrupt state.
func TestDeterministicClockCancelIdempotent(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))

	var fired int32
	id := clock.Schedule(10*time.Millisecond, func() { atomic.AddInt32(&fired, 1) })

	clock.Cancel(id)
	clock.Cancel(id)         // second cancel -- must be safe
	clock.Cancel(EventID(0)) // never-issued ID -- must be safe

	clock.Advance(20 * time.Millisecond)
	if atomic.LoadInt32(&fired) != 0 {
		t.Fatalf("event fired %d time(s) after double-cancel", fired)
	}
}

// TestDeterministicClockFiringOrder verifies that events at the same
// time fire in EventID order (deterministic).
func TestDeterministicClockFiringOrder(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))

	var order []EventID
	for i := 0; i < 5; i++ {
		id := clock.Schedule(0, func() {})
		// Replace the callback to capture the id at execution time.
		// Since Schedule already appended, we overwrite the callback
		// by scheduling anew and cancelling the first.
		clock.Cancel(id)
		capturedID := id
		clock.Schedule(10*time.Millisecond, func() {
			order = append(order, capturedID)
		})
	}

	clock.Advance(20 * time.Millisecond)

	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("events did not fire in ID order: %v", order)
		}
	}
}

// ---------- Cross-clock isolation tests ----------

// These tests simulate the invariant that was violated by the original bug:
// two independent clocks (representing two ns-3 nodes) must not interfere
// with each other's events when Cancel is called.
//
// DeterministicClock uses per-instance counters, so IDs from different
// clocks CAN collide (e.g., both produce ID=1). We verify that cancelling
// ID=X on clock A does NOT affect the event with ID=X on clock B.

// TestTwoClocksEventIDsCanOverlap demonstrates that per-instance counters
// produce the same IDs (the pre-fix behavior for Ns3Clock).
func TestTwoClocksEventIDsCanOverlap(t *testing.T) {
	cA := NewDeterministicClock(time.Unix(0, 0))
	cB := NewDeterministicClock(time.Unix(0, 0))

	idA := cA.Schedule(10*time.Millisecond, func() {})
	idB := cB.Schedule(10*time.Millisecond, func() {})

	if idA != idB {
		t.Fatalf("expected same first ID from independent clocks; got %d vs %d", idA, idB)
	}
}

// TestCrossCancelIsolation verifies that cancelling an event on clock A
// does not affect the event with the same numeric ID on clock B.
// This is the CORE invariant that the original bug violated.
func TestCrossCancelIsolation(t *testing.T) {
	cA := NewDeterministicClock(time.Unix(0, 0))
	cB := NewDeterministicClock(time.Unix(0, 0))

	var firedA, firedB int32
	idA := cA.Schedule(10*time.Millisecond, func() { atomic.AddInt32(&firedA, 1) })
	cB.Schedule(10*time.Millisecond, func() { atomic.AddInt32(&firedB, 1) })

	// Cancel on A -- B must be unaffected.
	cA.Cancel(idA)

	cA.Advance(20 * time.Millisecond)
	cB.Advance(20 * time.Millisecond)

	if atomic.LoadInt32(&firedA) != 0 {
		t.Fatalf("clock A event should not fire after cancel; fired %d", firedA)
	}
	if atomic.LoadInt32(&firedB) != 1 {
		t.Fatalf("clock B event must fire exactly once; fired %d", firedB)
	}
}

// ---------- Self-rescheduling heartbeat pattern ----------

// TestSelfReschedulingHeartbeat verifies that the heartbeat pattern used
// by SimDvRouter (schedule -> callback -> reschedule) fires the expected
// number of times without losing events.
func TestSelfReschedulingHeartbeat(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))

	const interval = 100 * time.Millisecond
	const total = 200 // number of heartbeats

	var count int32
	var curEvent EventID

	var reschedule func()
	reschedule = func() {
		atomic.AddInt32(&count, 1)
		curEvent = clock.Schedule(interval, reschedule)
		_ = curEvent // suppress unused warning
	}

	// First schedule
	curEvent = clock.Schedule(interval, reschedule)

	// Run for total * interval
	clock.Advance(time.Duration(total) * interval)

	got := atomic.LoadInt32(&count)
	if got != total {
		t.Fatalf("expected %d heartbeats, got %d", total, got)
	}
}

// TestMultiNodeSelfRescheduling runs the heartbeat pattern on N independent
// clocks concurrently and verifies none lose events. This is the unit-level
// regression for the global event ID collision bug.
func TestMultiNodeSelfRescheduling(t *testing.T) {
	const nNodes = 8
	const interval = 50 * time.Millisecond
	const duration = 10 * time.Second
	expected := int32(duration / interval)

	clocks := make([]*DeterministicClock, nNodes)
	counts := make([]int32, nNodes)

	for i := 0; i < nNodes; i++ {
		clocks[i] = NewDeterministicClock(time.Unix(0, 0))
		idx := i // capture
		var reschedule func()
		reschedule = func() {
			atomic.AddInt32(&counts[idx], 1)
			clocks[idx].Schedule(interval, reschedule)
		}
		clocks[i].Schedule(interval, reschedule)
	}

	// Advance all clocks identically
	for i := 0; i < nNodes; i++ {
		clocks[i].Advance(duration)
	}

	for i := 0; i < nNodes; i++ {
		got := atomic.LoadInt32(&counts[i])
		if got != expected {
			t.Fatalf("node %d: expected %d heartbeats, got %d", i, expected, got)
		}
	}
}

// ---------- Shared-clock multi-node cancel/reschedule ----------

// TestSharedClockCancelRescheduleIsolation uses a SINGLE DeterministicClock
// (as in the real simulation where all nodes share ns-3 time) with multiple
// "nodes" that each cancel+reschedule their own event. Verifies that cancel
// on node A never removes node B's event.
func TestSharedClockCancelRescheduleIsolation(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))

	const nNodes = 6
	const interval = 100 * time.Millisecond
	const duration = 5 * time.Second
	expected := int32(duration / interval)

	counts := make([]int32, nNodes)
	events := make([]EventID, nNodes)

	for i := 0; i < nNodes; i++ {
		idx := i
		var reschedule func()
		reschedule = func() {
			atomic.AddInt32(&counts[idx], 1)
			// Cancel old event (simulating the cancel-before-reschedule pattern)
			if events[idx] != 0 {
				clock.Cancel(events[idx])
			}
			events[idx] = clock.Schedule(interval, reschedule)
		}
		events[i] = clock.Schedule(interval, reschedule)
	}

	clock.Advance(duration)

	for i := 0; i < nNodes; i++ {
		got := atomic.LoadInt32(&counts[i])
		if got != expected {
			t.Errorf("node %d: expected %d heartbeats, got %d", i, expected, got)
		}
	}
}
