package sim

import (
	"crypto/rand"
	"sync/atomic"
	"time"

	"github.com/named-data/ndnd/std/ndn"
)

// SimTimer implements ndn.Timer using a simulation Clock.
// It satisfies the std/ layer's timer contract without wall-clock access.
type SimTimer struct {
	clock Clock
}

var _ ndn.Timer = (*SimTimer)(nil)

// NewSimTimer creates a SimTimer backed by the given simulation clock.
func NewSimTimer(clock Clock) *SimTimer {
	return &SimTimer{clock: clock}
}

func (t *SimTimer) Now() time.Time {
	return t.clock.Now()
}

func (t *SimTimer) Sleep(d time.Duration) {
	// In a discrete-event simulator, blocking sleep is not meaningful.
	// This is a no-op; callers that need to wait should use Schedule.
	// The simulation will advance time externally.
}

func (t *SimTimer) Schedule(d time.Duration, f func()) func() error {
	id := t.clock.Schedule(d, f)
	// Use an atomic flag so concurrent callers (e.g., SVS goroutines) cannot
	// race on the cancel state.  Double-cancel is silently ignored (idempotent)
	// to match the contract of time.AfterFunc's Stop method.
	var cancelled atomic.Bool
	return func() error {
		if !cancelled.CompareAndSwap(false, true) {
			return nil // already cancelled — idempotent, not an error
		}
		t.clock.Cancel(id)
		return nil
	}
}

func (t *SimTimer) Nonce() []byte {
	buf := make([]byte, 8)
	// crypto/rand.Read is guaranteed to fill the buffer on Linux (reads from
	// /dev/urandom).  We always return all 8 bytes; returning buf[:n] would
	// silently produce a shorter nonce on any unexpected error.
	rand.Read(buf)
	return buf
}
