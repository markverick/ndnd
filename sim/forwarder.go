package sim

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/dispatch"
	"github.com/named-data/ndnd/fw/fw"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
)

// globalFaceID is a process-wide atomic counter ensuring face IDs are unique
// across all simulated nodes. This is critical because faces are registered in
// the global dispatch.FaceDispatch table shared by all forwarder threads.
var globalFaceID atomic.Uint64

const (
	// Maintenance interval for PIT/CS expiry and dead nonce list cleanup.
	simMaintenanceInterval = 100 * time.Millisecond
)

// SimForwarder wraps a real fw.Thread to provide per-node NDN forwarding
// in simulation mode. It delegates all packet processing to the real
// forwarder pipeline rather than reimplementing it.
type SimForwarder struct {
	thread *fw.Thread

	clock Clock

	// Per-node RIB (routes go through here so readvertise fires)
	rib *table.RibTable

	// Per-node face table (face ID → DispatchFace)
	faces  map[uint64]*DispatchFace
	faceMu sync.Mutex

	// Scheduled maintenance event
	maintEvent EventID

	// Counters
	nPktsIn  atomic.Uint64
	nPktsOut atomic.Uint64
}

// NewSimForwarder creates a new simulation forwarder backed by a real fw.Thread.
// Each simulated node should have its own SimForwarder.
func NewSimForwarder(clock Clock) *SimForwarder {
	rib := &table.RibTable{}
	rib.InitRoot()

	fwd := &SimForwarder{
		clock: clock,
		faces: make(map[uint64]*DispatchFace),
		rib:   rib,
	}

	// Create a real forwarding thread (ID 0 — single-threaded in sim)
	fwd.thread = fw.NewThread(0)

	// Give this node its own FIB (not the global shared one)
	fwd.thread.SetFib(table.NewFibStrategyTree())

	return fwd
}

// Start schedules periodic maintenance.
func (fwd *SimForwarder) Start() {
	fwd.scheduleMaintenance()
}

// Stop cancels scheduled maintenance.
func (fwd *SimForwarder) Stop() {
	if fwd.maintEvent != 0 {
		fwd.clock.Cancel(fwd.maintEvent)
		fwd.maintEvent = 0
	}
}

func (fwd *SimForwarder) scheduleMaintenance() {
	fwd.maintEvent = fwd.clock.Schedule(simMaintenanceInterval, func() {
		fwd.thread.RunMaintenance()
		fwd.scheduleMaintenance()
	})
}

// --- Face management ---

// AddFace creates a new DispatchFace, registers it in the global dispatch table,
// and returns its face ID. The sendFunc callback is invoked when the forwarder
// wants to transmit a packet out this face.
func (fwd *SimForwarder) AddFace(scope defn.Scope, linkType defn.LinkType, sendFunc FwSendFunc) uint64 {
	fwd.faceMu.Lock()
	defer fwd.faceMu.Unlock()

	id := globalFaceID.Add(1)

	face := NewDispatchFace(id, scope, linkType, sendFunc)
	fwd.faces[id] = face
	dispatch.AddFace(id, face)

	return id
}

// RemoveFace removes a face from both the local table and global dispatch.
func (fwd *SimForwarder) RemoveFace(id uint64) {
	fwd.faceMu.Lock()
	defer fwd.faceMu.Unlock()

	delete(fwd.faces, id)
	dispatch.RemoveFace(id)
}

// GetFace returns a face by ID.
func (fwd *SimForwarder) GetFace(id uint64) *DispatchFace {
	fwd.faceMu.Lock()
	defer fwd.faceMu.Unlock()
	return fwd.faces[id]
}

// --- RIB/FIB management ---

// withNodeFib temporarily swaps the global FibStrategyTable to this node's
// FIB for the duration of f(). The simulation is single-threaded so this
// is safe — it ensures RIB's updateNexthopsEnc writes to the correct FIB.
func (fwd *SimForwarder) withNodeFib(f func()) {
	old := table.FibStrategyTable
	table.FibStrategyTable = fwd.thread.Fib()
	defer func() { table.FibStrategyTable = old }()
	f()
}

// AddRoute adds a route through this node's RIB so that readvertise fires.
func (fwd *SimForwarder) AddRoute(name enc.Name, faceID uint64, cost uint64) {
	fwd.AddRouteWithOrigin(name, faceID, cost, 0)
}

// AddRouteWithOrigin adds a route with a specific origin value.
func (fwd *SimForwarder) AddRouteWithOrigin(name enc.Name, faceID uint64, cost uint64, origin uint64) {
	fwd.withNodeFib(func() {
		fwd.rib.AddEncRoute(name, &table.Route{
			FaceID: faceID,
			Cost:   cost,
			Origin: origin,
		})
	})
}

// SetStrategy sets the forwarding strategy for a prefix.
func (fwd *SimForwarder) SetStrategy(prefix enc.Name, strategy enc.Name) {
	fwd.thread.Fib().SetStrategyEnc(prefix, strategy)
}

// RemoveRoute removes a route through this node's RIB so that readvertise fires.
func (fwd *SimForwarder) RemoveRoute(name enc.Name, faceID uint64) {
	fwd.RemoveRouteWithOrigin(name, faceID, 0)
}

// RemoveRouteWithOrigin removes a route with a specific origin value.
func (fwd *SimForwarder) RemoveRouteWithOrigin(name enc.Name, faceID uint64, origin uint64) {
	fwd.withNodeFib(func() {
		fwd.rib.RemoveRouteEnc(name, faceID, origin)
	})
}

// --- Packet processing ---

// ReceivePacket is the main entry point for packets arriving from ns-3.
// It parses the frame and dispatches it synchronously through the real
// forwarding pipeline.
func (fwd *SimForwarder) ReceivePacket(faceID uint64, frame []byte) {
	face := fwd.GetFace(faceID)
	if face == nil || face.State() != defn.Up {
		return
	}

	// Parse the frame as an NDNLPv2 / bare TLV packet
	wire := enc.Wire{frame}
	parsed, err := defn.ParseFwPacket(enc.NewWireView(wire), false)
	if err != nil {
		return
	}

	pkt := &defn.Pkt{
		IncomingFaceID: faceID,
	}

	if parsed.LpPacket != nil {
		// LP-wrapped: extract PIT token, NextHopFaceId, and inner fragment
		lp := parsed.LpPacket
		pkt.PitToken = lp.PitToken
		pkt.CongestionMark = lp.CongestionMark
		pkt.NextHopFaceID = lp.NextHopFaceId

		fragment := lp.Fragment
		if len(fragment) == 0 {
			return
		}
		inner, err := defn.ParseFwPacket(enc.NewWireView(fragment), false)
		if err != nil {
			return
		}
		pkt.Raw = fragment
		pkt.L3 = inner
	} else {
		// Bare Interest or Data
		pkt.Raw = wire
		pkt.L3 = parsed
	}

	if pkt.L3 == nil || (pkt.L3.Interest == nil && pkt.L3.Data == nil) {
		return
	}

	// Fill in the name for the forwarding pipeline
	if pkt.L3.Interest != nil {
		pkt.Name = pkt.L3.Interest.NameV
	} else if pkt.L3.Data != nil {
		pkt.Name = pkt.L3.Data.NameV
	}

	fwd.nPktsIn.Add(1)

	// Synchronously process through the real forwarding pipeline
	fwd.thread.ProcessPacket(pkt)
}

// Thread returns the underlying fw.Thread (for testing/debug access).
func (fwd *SimForwarder) Thread() *fw.Thread {
	return fwd.thread
}
