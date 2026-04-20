package sim

import (
	"fmt"
	"sync"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
)

// SendFunc is called when the simulation face wants to transmit a packet.
// The external simulator should deliver the bytes to the appropriate link.
type SendFunc func(frame []byte)

// SimFace implements ndn.Face for simulation environments.
// Instead of real network I/O, it uses callbacks:
//   - Outgoing packets invoke the registered SendFunc.
//   - Incoming packets are injected by calling Receive().
//
// This face is used by the BasicEngine / SimEngine on the std/ application
// layer. It is NOT the same as the forwarder-level face used inside fw/.
type SimFace struct {
	mu       sync.Mutex
	running  bool
	local    bool
	onPkt    func(frame []byte)
	onError  func(err error)
	onUp     []func()
	onDown   []func()
	sendFunc SendFunc
}

var _ ndn.Face = (*SimFace)(nil)

// NewSimFace creates a simulation face. sendFunc is called for every
// outgoing packet. If sendFunc is nil, sends are silently dropped.
func NewSimFace(sendFunc SendFunc, local bool) *SimFace {
	return &SimFace{
		sendFunc: sendFunc,
		local:    local,
	}
}

func (f *SimFace) String() string {
	return "sim-face"
}

func (f *SimFace) IsRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *SimFace) IsLocal() bool {
	return f.local
}

func (f *SimFace) OnPacket(onPkt func(frame []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onPkt = onPkt
}

func (f *SimFace) OnError(onError func(err error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onError = onError
}

func (f *SimFace) Open() error {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return fmt.Errorf("face is already running")
	}
	f.running = true
	// Snapshot callbacks before releasing the lock so that registrations
	// concurrent with Open are not invoked with a partially-updated state.
	// Holding the lock across user callbacks would deadlock if a callback
	// re-entered the face (e.g. Send, IsRunning).
	cbs := make([]func(), len(f.onUp))
	copy(cbs, f.onUp)
	f.mu.Unlock()
	for _, cb := range cbs {
		cb()
	}
	return nil
}

func (f *SimFace) Close() error {
	f.mu.Lock()
	if !f.running {
		f.mu.Unlock()
		return nil
	}
	f.running = false
	// Same snapshot-then-release pattern as Open — prevents deadlock if a
	// callback calls back into the face.
	cbs := make([]func(), len(f.onDown))
	copy(cbs, f.onDown)
	f.mu.Unlock()
	for _, cb := range cbs {
		cb()
	}
	return nil
}

func (f *SimFace) Send(pkt enc.Wire) error {
	f.mu.Lock()
	if !f.running {
		f.mu.Unlock()
		return fmt.Errorf("face is not running")
	}
	sf := f.sendFunc
	f.mu.Unlock()

	if sf == nil {
		return nil // drop
	}
	sf(pkt.Join())
	return nil
}

// Receive injects an incoming packet from the simulator into this face.
// This is the entry point for ns-3 -> NDNd packet delivery.
func (f *SimFace) Receive(frame []byte) {
	f.mu.Lock()
	handler := f.onPkt
	f.mu.Unlock()
	if handler != nil {
		handler(frame)
	}
}

func (f *SimFace) OnUp(onUp func()) (cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onUp = append(f.onUp, onUp)
	idx := len(f.onUp) - 1
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if idx < len(f.onUp) {
			f.onUp[idx] = func() {} // neuter
		}
	}
}

func (f *SimFace) OnDown(onDown func()) (cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onDown = append(f.onDown, onDown)
	idx := len(f.onDown) - 1
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if idx < len(f.onDown) {
			f.onDown[idx] = func() {} // neuter
		}
	}
}
