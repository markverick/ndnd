package sim

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	"github.com/named-data/ndnd/std/types/optional"
)

// SimEngine implements ndn.Engine for discrete-event simulation.
//
// Unlike BasicEngine, it processes all packets synchronously on the caller's
// thread. This is essential because ns-3 is single-threaded: all API calls
// (including Simulator::Schedule, NetDevice::Send) must happen on the main
// simulation thread.
//
// When the forwarder delivers a packet to the application face, SimFace.Receive()
// calls SimEngine.onPacket() directly, which parses, dispatches to the handler,
// and the handler's Reply callback sends Data back — all synchronously, all on
// the ns-3 main thread.
type SimEngine struct {
	face    ndn.Face
	timer   ndn.Timer
	running atomic.Bool

	// Interest handler FIB (prefix → handler)
	fib     *nameTrie[ndn.InterestHandler]
	fibLock sync.Mutex
}

var _ ndn.Engine = (*SimEngine)(nil)

// NewSimEngine creates a new simulation engine attached to the given face and timer.
func NewSimEngine(face ndn.Face, timer ndn.Timer) *SimEngine {
	return &SimEngine{
		face:  face,
		timer: timer,
		fib:   newNameTrie[ndn.InterestHandler](),
	}
}

func (e *SimEngine) String() string {
	return "SimEngine"
}

func (e *SimEngine) EngineTrait() ndn.Engine {
	return e
}

func (e *SimEngine) Spec() ndn.Spec {
	return spec.Spec{}
}

func (e *SimEngine) Timer() ndn.Timer {
	return e.timer
}

func (e *SimEngine) Face() ndn.Face {
	return e.face
}

func (e *SimEngine) IsRunning() bool {
	return e.running.Load()
}

func (e *SimEngine) Start() error {
	if e.face.IsRunning() {
		return fmt.Errorf("face is already running")
	}

	// Register synchronous packet handler — no goroutine, no channel.
	e.face.OnPacket(func(frame []byte) {
		e.onPacket(frame)
	})

	if err := e.face.Open(); err != nil {
		return err
	}
	e.running.Store(true)
	return nil
}

func (e *SimEngine) Stop() error {
	if !e.running.Load() {
		return fmt.Errorf("engine is not running")
	}
	e.running.Store(false)
	return e.face.Close()
}

func (e *SimEngine) AttachHandler(prefix enc.Name, handler ndn.InterestHandler) error {
	e.fibLock.Lock()
	defer e.fibLock.Unlock()
	n := e.fib.matchAlways(prefix)
	if n.val != nil {
		return fmt.Errorf("%w: %s", ndn.ErrMultipleHandlers, prefix)
	}
	n.val = handler
	return nil
}

func (e *SimEngine) DetachHandler(prefix enc.Name) error {
	e.fibLock.Lock()
	defer e.fibLock.Unlock()
	n := e.fib.exactMatch(prefix)
	if n == nil {
		return fmt.Errorf("no handler for prefix %s", prefix)
	}
	n.val = nil
	return nil
}

// Express is not supported in simulation (consumers inject packets directly).
func (e *SimEngine) Express(interest *ndn.EncodedInterest, callback ndn.ExpressCallbackFunc) error {
	return fmt.Errorf("SimEngine: Express not supported; inject packets via NdndSimReceivePacket")
}

// ExecMgmtCmd is not supported in simulation.
func (e *SimEngine) ExecMgmtCmd(module string, cmd string, args any) (any, error) {
	return nil, fmt.Errorf("SimEngine: ExecMgmtCmd not supported in simulation")
}

// SetCmdSec is a no-op in simulation.
func (e *SimEngine) SetCmdSec(signer ndn.Signer, validator func(enc.Name, enc.Wire, ndn.Signature) bool) {
}

// RegisterRoute is a no-op — FIB routes are managed by C++ via NdndSimAddRoute.
func (e *SimEngine) RegisterRoute(prefix enc.Name) error {
	return nil
}

// UnregisterRoute is a no-op.
func (e *SimEngine) UnregisterRoute(prefix enc.Name) error {
	return nil
}

// Post executes the task synchronously.
func (e *SimEngine) Post(task func()) {
	task()
}

// --- Packet processing (synchronous) ---

func (e *SimEngine) onPacket(frame []byte) {
	reader := enc.NewBufferView(frame)

	var pitToken []byte
	var incomingFaceId optional.Optional[uint64]
	var raw enc.Wire

	pkt, ctx, err := spec.ReadPacket(reader)
	if err != nil {
		return
	}

	if pkt.LpPacket != nil {
		lp := pkt.LpPacket
		if lp.FragIndex.IsSet() || lp.FragCount.IsSet() {
			return // fragmentation not supported
		}

		raw = lp.Fragment
		pitToken = lp.PitToken
		incomingFaceId = lp.IncomingFaceId

		// Parse inner packet
		if len(raw) == 1 {
			pkt, ctx, err = spec.ReadPacket(enc.NewBufferView(raw[0]))
		} else {
			pkt, ctx, err = spec.ReadPacket(enc.NewWireView(raw))
		}
		if err != nil || (pkt.Data == nil) == (pkt.Interest == nil) {
			return
		}
	} else {
		raw = reader.Range(0, reader.Length())
	}

	if pkt.Interest != nil {
		e.onInterest(ndn.InterestHandlerArgs{
			Interest:       pkt.Interest,
			RawInterest:    raw,
			SigCovered:     ctx.Interest_context.SigCovered(),
			PitToken:       pitToken,
			IncomingFaceId: incomingFaceId,
		})
	}
	// Data packets from forwarder are not dispatched to the engine
	// (there's no PIT on the app side in simulation).
}

func (e *SimEngine) onInterest(args ndn.InterestHandlerArgs) {
	name := args.Interest.Name()
	args.Deadline = e.timer.Now().Add(
		args.Interest.Lifetime().GetOr(4 * time.Second))

	handler := func() ndn.InterestHandler {
		e.fibLock.Lock()
		defer e.fibLock.Unlock()
		n := e.fib.prefixMatch(name)
		for n != nil && n.val == nil {
			n = n.par
		}
		if n != nil {
			return n.val
		}
		return nil
	}()

	if handler == nil {
		return
	}

	args.Reply = e.makeReplyFunc(args.PitToken)
	handler(args)
}

func (e *SimEngine) makeReplyFunc(pitToken []byte) ndn.WireReplyFunc {
	return func(dataWire enc.Wire) error {
		if dataWire == nil || !e.running.Load() || !e.face.IsRunning() {
			return ndn.ErrFaceDown
		}

		var outWire enc.Wire = dataWire
		if pitToken != nil {
			lpPkt := &spec.Packet{
				LpPacket: &spec.LpPacket{
					PitToken: pitToken,
					Fragment: dataWire,
				},
			}
			encoder := spec.PacketEncoder{}
			encoder.Init(lpPkt)
			wire := encoder.Encode(lpPkt)
			if wire != nil {
				outWire = wire
			}
		}

		return e.face.Send(outWire)
	}
}

// --- Minimal name trie for Interest dispatch ---

type nameTrie[V any] struct {
	val V
	par *nameTrie[V]
	dep int
	chd map[string]*nameTrie[V]
}

func newNameTrie[V any]() *nameTrie[V] {
	return &nameTrie[V]{chd: map[string]*nameTrie[V]{}}
}

func (n *nameTrie[V]) exactMatch(name enc.Name) *nameTrie[V] {
	if len(name) <= n.dep {
		return n
	}
	c := name[n.dep].TlvStr()
	if ch, ok := n.chd[c]; ok {
		return ch.exactMatch(name)
	}
	return nil
}

func (n *nameTrie[V]) prefixMatch(name enc.Name) *nameTrie[V] {
	if len(name) <= n.dep {
		return n
	}
	c := name[n.dep].TlvStr()
	if ch, ok := n.chd[c]; ok {
		return ch.prefixMatch(name)
	}
	return n
}

func (n *nameTrie[V]) matchAlways(name enc.Name) *nameTrie[V] {
	if len(name) <= n.dep {
		return n
	}
	c := name[n.dep].TlvStr()
	ch, ok := n.chd[c]
	if !ok {
		ch = &nameTrie[V]{
			par: n,
			dep: n.dep + 1,
			chd: map[string]*nameTrie[V]{},
		}
		n.chd[c] = ch
	}
	return ch.matchAlways(name)
}
