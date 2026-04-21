package sim

import (
	"sync"
	"sync/atomic"
	"time"

	_ndndsim "github.com/named-data/ndndsim"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/dispatch"
	"github.com/named-data/ndnd/fw/fw"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
	spec_mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
)

// globalFaceID is a process-wide atomic counter ensuring face IDs are unique
// across all simulated nodes. This is critical because faces are registered in
// the global dispatch.FaceDispatch table shared by all forwarder threads.
var globalFaceID atomic.Uint64

// fibSwapDepth tracks nesting depth of withNodeFib calls. Nested calls from
// the same node (e.g., management commands triggered during packet forwarding)
// are safe in the sequential simulator — the FIB/PET is already set to the
// right node's instance — so we just increment the depth instead of panicking.
var fibSwapDepth int

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

	// Per-node hooks (goroutine-local state: FIB, PET, clock, scheduler).
	hooks *_ndndsim.NodeHooks

	// Per-node RIB (routes go through here so readvertise fires)
	rib *table.RibTable

	// Per-node FIB and PET: stored here for withNodeFib (which must swap the
	// global table.FibStrategyTable) and are also registered in the node hooks
	// so that goroutine-local simFib()/simPet() return the right instances.
	fib table.FibStrategy
	pet *table.PrefixEgressTable

	// Per-node multicast strategy table.
	multicastFib table.FibStrategy

	// Per-node face table (face ID -> DispatchFace)
	faces  map[uint64]*DispatchFace
	faceMu sync.Mutex

	// Scheduled maintenance event
	maintEvent EventID
}

// NewSimForwarder creates a new simulation forwarder backed by a real fw.Thread.
// Each simulated node should have its own SimForwarder.  The provided hooks
// receive the per-node Fib and Pet so that goroutine-local simFib()/simPet()
// return the correct instances after BindNode is called.
func NewSimForwarder(clock Clock, hooks *_ndndsim.NodeHooks) *SimForwarder {
	rib := &table.RibTable{}
	rib.InitRoot()

	fib := table.NewFibStrategyTree()
	pet := table.NewPrefixEgressTable()

	// Register in hooks so goroutine-local lookups return the per-node instances.
	hooks.Fib = fib
	hooks.Pet = pet

	fwd := &SimForwarder{
		clock:        clock,
		hooks:        hooks,
		fib:          fib,
		pet:          pet,
		faces:        make(map[uint64]*DispatchFace),
		rib:          rib,
		multicastFib: table.NewMulticastStrategyTree(),
	}

	// Create a real forwarding thread (ID 0 -- single-threaded in sim).
	// Per-node FIB/PET isolation is handled via goroutine-local hooks;
	// no SetFib/SetPet calls are needed.
	fwd.thread = fw.NewThread(0)

	// In the current dv2-sim fork, thread.go still reads t.fib / t.pet struct
	// fields via t.Fib() / t.Pet().  Keep those set so the fork's forwarding
	// pipeline uses the per-node instances.  Once ndnd is cleaned up and the
	// transformer produces the final binary these calls can be removed.
	fwd.thread.SetFib(fib)
	fwd.thread.SetPet(pet)

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

	fwd.withNodeFib(func() {
		fwd.rib.CleanUpFace(id)
	})
	fwd.pet.CleanUpFace(id)
	// Remove nexthops for this face from the per-node FIB.
	fwd.fib.CleanUpFace(id)

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

// withNodeFib temporarily swaps the global FibStrategyTable, MulticastStrategyTable,
// and Pet to this node's instances for the duration of f(). This ensures that
// both the forwarding pipeline AND the NFD management handlers (which write to
// the global tables directly) operate on the correct per-node state.
//
// Nested calls for the same node are allowed: management commands fired from
// within packet processing (e.g., DV calling ExecMgmtCmd synchronously) will
// re-enter withNodeFib for the same fwd, which is safe because the globals are
// already set to this node's instances.
func (fwd *SimForwarder) withNodeFib(f func()) {
	if fibSwapDepth == 0 {
		table.FibStrategyTable = fwd.fib
		table.MulticastStrategyTable = fwd.multicastFib
		table.Pet = fwd.pet
	}
	fibSwapDepth++
	defer func() {
		fibSwapDepth--
	}()
	f()
}

// SetMulticastStrategy sets the multicast forwarding strategy for a prefix on this
// node's per-node multicast strategy table.
func (fwd *SimForwarder) SetMulticastStrategy(prefix enc.Name, strategy enc.Name) {
	fwd.multicastFib.SetStrategyEnc(prefix, strategy)
}

// AddRoute adds a route through this node's RIB so that readvertise fires.
func (fwd *SimForwarder) AddRoute(name enc.Name, faceID uint64, cost uint64) {
	fwd.AddDirectRoute(name, faceID, cost)
}

// AddDirectRoute installs a direct prefix route for simulation users.
// dv2 forwarding resolves ordinary application traffic through PET, so we
// mirror legacy ndndSIM AddRoute semantics by populating both PET and FIB.
func (fwd *SimForwarder) AddDirectRoute(name enc.Name, faceID uint64, cost uint64) {
	fwd.pet.AddNextHopEnc(name, faceID, cost)
	fwd.fib.InsertNextHopEnc(name, faceID, cost)
	fwd.AddRouteWithOrigin(name, faceID, cost, 0)
}

// AddRouteWithOrigin adds a route with a specific origin value.
// Uses ChildInherit by default, matching production ndnd behavior.
func (fwd *SimForwarder) AddRouteWithOrigin(name enc.Name, faceID uint64, cost uint64, origin uint64) {
	fwd.AddRouteWithFlags(name, faceID, cost, origin, uint64(spec_mgmt.RouteFlagChildInherit))
}

// AddRouteWithFlags adds a route with explicit flags.
func (fwd *SimForwarder) AddRouteWithFlags(name enc.Name, faceID uint64, cost uint64, origin uint64, flags uint64) {
	fwd.withNodeFib(func() {
		fwd.rib.AddEncRoute(name, &table.Route{
			FaceID: faceID,
			Cost:   cost,
			Origin: origin,
			Flags:  flags,
		})
	})
}

// SetStrategy sets the forwarding strategy for a prefix.
func (fwd *SimForwarder) SetStrategy(prefix enc.Name, strategy enc.Name) {
	fwd.fib.SetStrategyEnc(prefix, strategy)
}

// RemoveRoute removes a route through this node's RIB so that readvertise fires.
func (fwd *SimForwarder) RemoveRoute(name enc.Name, faceID uint64) {
	fwd.RemoveDirectRoute(name, faceID)
}

// RemoveDirectRoute removes a direct prefix route installed for simulation.
func (fwd *SimForwarder) RemoveDirectRoute(name enc.Name, faceID uint64) {
	fwd.pet.RemoveNextHopEnc(name, faceID)
	fwd.fib.RemoveNextHopEnc(name, faceID)
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
	// Bind per-node hooks so that goroutine-local simFib/simPet return
	// this node's instances.  This is a no-op if the caller (e.g.,
	// ReceiveOnInterface or a CGo boundary) has already bound the hooks,
	// but is necessary for paths that call ReceivePacket directly (e.g.,
	// the appFace send closure in Node.Start).
	if fwd.hooks != nil {
		_ndndsim.BindNode(fwd.hooks)
		defer _ndndsim.UnbindNode()
	}
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

	// Synchronously process through the real forwarding pipeline.
	// withNodeFib sets the global FibStrategyTable/MulticastStrategyTable/Pet
	// to this node's per-node instances. This is required so that both the
	// forwarding pipeline (thread.Fib()/Pet()) AND NFD management handlers
	// (which call table.FibStrategyTable.InsertNextHopEnc / table.Pet.AddNextHop
	// directly) operate on the correct per-node state.
	fwd.withNodeFib(func() {
		fwd.thread.ProcessPacket(pkt)
	})
}

// Thread returns the underlying fw.Thread (for testing/debug access).
func (fwd *SimForwarder) Thread() *fw.Thread {
	return fwd.thread
}
