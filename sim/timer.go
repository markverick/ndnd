package sim

import (
	"crypto/rand"
	"fmt"
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
	cancelled := false
	return func() error {
		if cancelled {
			return fmt.Errorf("event has already been canceled")
		}
		cancelled = true
		t.clock.Cancel(id)
		return nil
	}
}

func (t *SimTimer) Nonce() []byte {
	buf := make([]byte, 8)
	n, _ := rand.Read(buf)
	return buf[:n]
}
