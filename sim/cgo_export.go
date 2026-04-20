package sim

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	dv_table "github.com/named-data/ndnd/dv/table"
	"github.com/named-data/ndnd/fw/core"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/types/optional"
)

/*
#include "ndndsim_sim.h"
#include <stdlib.h>
*/
import "C"

// --- NS-3 Clock Implementation ---

// globalNextEvID is a process-wide atomic counter so that every Ns3Clock
// produces unique event IDs.  The C++ side stores them in a global
// unordered_map keyed by event ID, so collisions between different nodes
// would cause one node's Cancel to silently remove another node's event.
var globalNextEvID atomic.Uint64

// Ns3Clock implements the Clock interface using ns-3's simulation time.
type Ns3Clock struct {
	nodeID uint32
	mu     sync.Mutex
	events map[EventID]func()
}

// NewNs3Clock creates a clock for a specific node.
func NewNs3Clock(nodeID uint32) *Ns3Clock {
	return &Ns3Clock{
		nodeID: nodeID,
		events: make(map[EventID]func()),
	}
}

func (c *Ns3Clock) Now() time.Time {
	ns := int64(C.callGetTimeNs())
	return time.Unix(0, ns)
}

func (c *Ns3Clock) Schedule(delay time.Duration, callback func()) EventID {
	id := EventID(globalNextEvID.Add(1))

	c.mu.Lock()
	c.events[id] = callback
	c.mu.Unlock()

	delayNs := delay.Nanoseconds()
	if delayNs < 0 {
		delayNs = 0
	}

	C.callScheduleEvent(C.uint32_t(c.nodeID), C.int64_t(delayNs), C.uint64_t(id))
	return id
}

func (c *Ns3Clock) Cancel(id EventID) {
	// Cancel on the C++ scheduler first. If the cancel succeeds the event will
	// never fire; if it fails (already fired / already executing) FireEvent
	// will still find and invoke the callback because we haven't removed it yet.
	C.callCancelEvent(C.uint64_t(id))

	c.mu.Lock()
	delete(c.events, id)
	c.mu.Unlock()
}

// FireEvent is called by ns-3 when a scheduled event fires.
func (c *Ns3Clock) FireEvent(id EventID) {
	c.mu.Lock()
	cb, ok := c.events[id]
	if ok {
		delete(c.events, id)
	}
	c.mu.Unlock()

	if ok && cb != nil {
		cb()
	}
}

// --- Global Runtime (singleton for CGo access) ---

var (
	globalRuntime *Runtime
	globalClocks  sync.Map // nodeID -> *Ns3Clock

	// consumerFlags maps nodeID to all active stop flags for that node's consumer
	// loops.  Using a slice (not sync.Map with a single *int32) ensures that
	// multiple startConsumerLoop calls for the same node can all be stopped via
	// NdndSimStopConsumer.  Guarded by consumerMu.
	consumerMu    sync.Mutex
	consumerFlags map[uint32][]*int32

	dvSpanMu sync.Mutex
	dvSpanByPrefix    map[string]*dvSpanMetric

	// Routing convergence: tracks when each node has reachable routes
	// to all other nodes. Purely event-driven via RouterReachableEvent.
	routingConvMu        sync.Mutex
	routingConvReachable map[string]map[string]bool // nodeRouter -> set of reachableRouters
	routingConvTimeNs    int64                      // sim timestamp when convergence completed (0 = not yet)
	routingConvStartNs   int64                      // sim timestamp of first RouterReachable event
	routingConvTotal     int                        // expected total DV nodes (0 = unknown)
	routingConvFired     bool                       // true after callback has been fired
)

type dvSpanMetric struct {
	firstOriginNs int64
	lastReceiveNs int64
}

// --- Exported CGo functions called by ns-3 C++ code ---

//export NdndSimInit
func NdndSimInit(
	sendPacketCb C.NdndSimSendPacketFunc,
	scheduleEventCb C.NdndSimScheduleEventFunc,
	cancelEventCb C.NdndSimCancelEventFunc,
	getTimeNsCb C.NdndSimGetTimeNsFunc,
	dataProducedCb C.NdndSimDataProducedFunc,
	dataReceivedCb C.NdndSimDataReceivedFunc,
	routingConvergedCb C.NdndSimRoutingConvergedFunc,
) {
	C.setSendPacketCb(sendPacketCb)
	C.setScheduleEventCb(scheduleEventCb)
	C.setCancelEventCb(cancelEventCb)
	C.setGetTimeNsCb(getTimeNsCb)
	C.setDataProducedCb(dataProducedCb)
	C.setDataReceivedCb(dataReceivedCb)
	C.setRoutingConvergedCb(routingConvergedCb)

	// Override NDNd's clock to use ns-3 simulation time
	core.NowFunc = func() time.Time {
		ns := int64(C.callGetTimeNs())
		return time.Unix(0, ns)
	}

	globalRuntime = NewRuntime()

	dvSpanMu.Lock()
	dvSpanByPrefix = make(map[string]*dvSpanMetric)
	dvSpanMu.Unlock()

	consumerMu.Lock()
	consumerFlags = make(map[uint32][]*int32)
	consumerMu.Unlock()

	routingConvMu.Lock()
	routingConvReachable = make(map[string]map[string]bool)
	routingConvTimeNs = 0
	routingConvStartNs = 0
	routingConvTotal = 0
	routingConvFired = false
	routingConvMu.Unlock()

	dv_table.SetPrefixEventObserver(func(ev dv_table.PrefixEvent) {
		key := ev.Name.TlvStr()
		ns := ev.At.UnixNano()

		dvSpanMu.Lock()
		m := dvSpanByPrefix[key]
		if m == nil {
			m = &dvSpanMetric{}
			dvSpanByPrefix[key] = m
		}

		switch ev.Kind {
		case dv_table.PrefixEventGlobalAnnounce:
			if m.firstOriginNs == 0 || ns < m.firstOriginNs {
				m.firstOriginNs = ns
			}
		case dv_table.PrefixEventAddRemotePrefix:
			if ns > m.lastReceiveNs {
				m.lastReceiveNs = ns
			}
		}
		dvSpanMu.Unlock()
	})

	dv_table.SetRouterReachableObserver(func(ev dv_table.RouterReachableEvent) {
		nodeKey := ev.NodeRouter.String()
		reachKey := ev.ReachableRouter.String()
		ns := ev.At.UnixNano()

		// shouldFire is set under routingConvMu and acted on after the lock is
		// released.  This avoids the lock-ordering hazard where callRoutingConverged
		// (→ OnRoutingConverged → g_bridgeMutex) is called while routingConvMu is
		// held, which would establish an implicit routingConvMu → g_bridgeMutex
		// ordering that could deadlock if the bridge ever needs the reverse order.
		shouldFire := false

		routingConvMu.Lock()

		if routingConvStartNs == 0 || ns < routingConvStartNs {
			routingConvStartNs = ns
		}

		s := routingConvReachable[nodeKey]
		if s == nil {
			s = make(map[string]bool)
			routingConvReachable[nodeKey] = s
		}
		s[reachKey] = true

		// Record the sim time of the last event that could complete convergence.
		if ns > routingConvTimeNs {
			routingConvTimeNs = ns
		}

		// Check if convergence is now complete and fire callback once.
		if routingConvTotal > 0 && !routingConvFired {
			n := routingConvTotal
			if len(routingConvReachable) >= n {
				allDone := true
				for _, rs := range routingConvReachable {
					if len(rs) < n-1 {
						allDone = false
						break
					}
				}
				if allDone {
					routingConvFired = true
					shouldFire = true
				}
			}
		}

		routingConvMu.Unlock()

		if shouldFire {
			C.callRoutingConverged()
		}
	})
}

//export NdndSimCreateNode
func NdndSimCreateNode(nodeId C.uint32_t) C.int {
	if globalRuntime == nil {
		return -1
	}

	clock := NewNs3Clock(uint32(nodeId))

	// Create node with its own clock
	node := NewNode(uint32(nodeId), clock)

	// Set the Data received callback so the engine notifies C++
	if eng, ok := node.Engine().(*SimEngine); ok {
		nid := nodeId
		eng.onDataReceived = func(nodeID uint32, dataSize uint32, dataName string) {
			cName := C.CString(dataName)
			C.callDataReceived(nid, C.uint32_t(dataSize), cName, C.uint32_t(len(dataName)))
			C.free(unsafe.Pointer(cName))
		}
	}

	globalRuntime.AddNode(uint32(nodeId), node)

	if err := node.Start(); err != nil {
		// Clean up the node registration so a later NdndSimCreateNode with the
		// same ID starts from a clean state. The clock is not yet in globalClocks
		// so no NdndSimFireEvent can reach it.
		globalRuntime.DestroyNode(uint32(nodeId))
		return -1
	}

	// Only expose the clock after Start() succeeds. This ensures NdndSimFireEvent
	// cannot dispatch into a node that is not yet fully initialised.
	globalClocks.Store(uint32(nodeId), clock)
	return 0
}

//export NdndSimDestroyNode
func NdndSimDestroyNode(nodeId C.uint32_t) {
	if globalRuntime == nil {
		return
	}
	globalRuntime.DestroyNode(uint32(nodeId))
	globalClocks.Delete(uint32(nodeId))
	// Stop and remove any consumer loops for this node so that a subsequent
	// NdndSimCreateNode with the same ID doesn't inherit stale stop flags.
	consumerMu.Lock()
	flags := consumerFlags[uint32(nodeId)]
	delete(consumerFlags, uint32(nodeId))
	consumerMu.Unlock()
	for _, f := range flags {
		atomic.StoreInt32(f, 1)
	}
}

//export NdndSimAddFace
func NdndSimAddFace(nodeId C.uint32_t, ifIndex C.uint32_t) C.uint64_t {
	if globalRuntime == nil {
		return 0
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return 0
	}

	nid := uint32(nodeId)
	iidx := uint32(ifIndex)
	faceID := node.AddNetworkFace(iidx, func(faceID uint64, frame []byte) {
		// NDNd -> ns-3: send packet through callback
		if len(frame) == 0 {
			return
		}
		C.callSendPacket(
			C.uint32_t(nid),
			C.uint32_t(iidx),
			unsafe.Pointer(&frame[0]),
			C.uint32_t(len(frame)),
		)
	})
	return C.uint64_t(faceID)
}

//export NdndSimRemoveFace
func NdndSimRemoveFace(nodeId C.uint32_t, ifIndex C.uint32_t) {
	if globalRuntime == nil {
		return
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return
	}
	node.RemoveNetworkFace(uint32(ifIndex))
}

//export NdndSimReceivePacket
func NdndSimReceivePacket(nodeId C.uint32_t, ifIndex C.uint32_t, data unsafe.Pointer, dataLen C.uint32_t) {
	if globalRuntime == nil {
		return
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return
	}

	// Copy the data (C++ memory may be freed after this call)
	frame := C.GoBytes(data, C.int(dataLen))
	node.ReceiveOnInterface(uint32(ifIndex), frame)
}

//export NdndSimAddRoute
func NdndSimAddRoute(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int, faceId C.uint64_t, cost C.uint64_t) {
	if globalRuntime == nil {
		return
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return
	}
	node.AddRoute(name, uint64(faceId), uint64(cost))
}

//export NdndSimRemoveRoute
func NdndSimRemoveRoute(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int, faceId C.uint64_t) {
	if globalRuntime == nil {
		return
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return
	}
	node.RemoveRoute(name, uint64(faceId))
}

//export NdndSimFireEvent
func NdndSimFireEvent(nodeId C.uint32_t, eventId C.uint64_t) {
	val, ok := globalClocks.Load(uint32(nodeId))
	if !ok {
		return
	}
	clock := val.(*Ns3Clock)
	clock.FireEvent(EventID(eventId))
}

//export NdndSimStartDv
func NdndSimStartDv(nodeId C.uint32_t, networkStr *C.char, networkLen C.int, routerStr *C.char, routerLen C.int, cfgStr *C.char, cfgLen C.int) C.int {
	if globalRuntime == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}

	network := C.GoStringN(networkStr, networkLen)
	router := C.GoStringN(routerStr, routerLen)
	cfgJSON := C.GoStringN(cfgStr, cfgLen)

	if err := node.StartDv(network, router, cfgJSON); err != nil {
		return -1
	}
	return 0
}

//export NdndSimStopDv
func NdndSimStopDv(nodeId C.uint32_t) {
	if globalRuntime == nil {
		return
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return
	}
	node.StopDv()
}

//export NdndSimDestroy
func NdndSimDestroy() {
	dv_table.SetPrefixEventObserver(nil)
	dv_table.SetRouterReachableObserver(nil)

	dvSpanMu.Lock()
	dvSpanByPrefix = make(map[string]*dvSpanMetric)
	dvSpanMu.Unlock()

	routingConvMu.Lock()
	routingConvReachable = make(map[string]map[string]bool)
	routingConvTimeNs = 0
	routingConvStartNs = 0
	routingConvFired = false
	routingConvTotal = 0
	routingConvMu.Unlock()

	// Remove stale clock and consumer-flag entries left by DestroyAll.
	// NdndSimDestroyNode removes them individually; NdndSimDestroy must
	// clean up the remainder so a subsequent re-init starts with empty maps.
	globalClocks.Range(func(k, _ any) bool {
		globalClocks.Delete(k)
		return true
	})

	consumerMu.Lock()
	consumerFlags = make(map[uint32][]*int32)
	consumerMu.Unlock()

	// Allow face.Initialize() and table.Initialize() to run again on
	// next re-init (they may reset internal queues/state that degrade
	// across simulation runs).
	simInitMu.Lock()
	simInitDone = false
	simInitMu.Unlock()

	// Clear the global trust root so a subsequent NdndSimInit with a
	// different network prefix doesn't inherit stale certificates.
	ResetSimTrust()

	if globalRuntime != nil {
		globalRuntime.DestroyAll()
		globalRuntime = nil
	}
}

//export NdndSimGetDvUpdateSpanNs
func NdndSimGetDvUpdateSpanNs(prefixStr *C.char, prefixLen C.int) C.int64_t {
	if globalRuntime == nil || prefixStr == nil || int(prefixLen) == 0 {
		return C.int64_t(-1)
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return C.int64_t(-1)
	}
	key := name.TlvStr()

	dvSpanMu.Lock()
	m := dvSpanByPrefix[key]
	dvSpanMu.Unlock()
	if m == nil || m.firstOriginNs == 0 || m.lastReceiveNs == 0 || m.lastReceiveNs < m.firstOriginNs {
		return C.int64_t(-1)
	}

	return C.int64_t(m.lastReceiveNs - m.firstOriginNs)
}

//export NdndSimSetTotalNodes
func NdndSimSetTotalNodes(totalNodes C.int) {
	routingConvMu.Lock()
	defer routingConvMu.Unlock()
	routingConvTotal = int(totalNodes)
}

//export NdndSimGetRoutingConvergenceNs
func NdndSimGetRoutingConvergenceNs(totalNodes C.int) C.int64_t {
	routingConvMu.Lock()
	defer routingConvMu.Unlock()

	if routingConvStartNs == 0 || int(totalNodes) < 2 {
		return C.int64_t(-1)
	}

	n := int(totalNodes)

	// Check that every node has reachable routes to all other n-1 nodes.
	if len(routingConvReachable) < n {
		return C.int64_t(-1)
	}
	for _, reachSet := range routingConvReachable {
		if len(reachSet) < n-1 {
			return C.int64_t(-1)
		}
	}

	// Walk all events to find the actual completion timestamp:
	// the earliest sim time at which ALL nodes had ALL n-1 routes.
	// Since we only record the global max event time during collection,
	// we need a more precise calculation.
	//
	// For each node, find its (n-1)th reachable event (the timestamp at
	// which it completed). Convergence = max of per-node completion times
	// minus the first event across all nodes.
	//
	// We don't have per-event timestamps stored granularly -- we stored
	// routingConvTimeNs as the max. This IS the timestamp of the last
	// event globally, which by definition is >= every per-node completion
	// time. But it might overcount if the last event was redundant.
	//
	// However, the observer only fires on first-time reachability (the
	// "Router is now reachable" condition is guarded by pfxSubs existence
	// check), so every event is unique and meaningful. The last event IS
	// the one that completed some node's set, making it the convergence
	// moment.

	return C.int64_t(routingConvTimeNs - routingConvStartNs)
}

//export NdndSimGetAppFaceId
func NdndSimGetAppFaceId(nodeId C.uint32_t) C.uint64_t {
	if globalRuntime == nil {
		return 0
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return 0
	}
	return C.uint64_t(node.AppFaceID())
}

//export NdndSimRegisterProducer
func NdndSimRegisterProducer(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int, payloadSize C.uint32_t, freshnessMs C.uint32_t) C.int {
	if globalRuntime == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return -1
	}

	engine := node.Engine()
	pSize := int(payloadSize)
	freshness := time.Duration(freshnessMs) * time.Millisecond
	dataSigner := sig.NewSha256Signer()

	handler := func(args ndn.InterestHandlerArgs) {
		// Fill content with bytes from the Interest name repeated cyclically.
		// This makes each Data payload unique per Interest (deterministic but
		// non-zero), so cache collisions and duplicate deliveries are detectable.
		content := make([]byte, pSize)
		if pSize > 0 {
			nameBytes := []byte(args.Interest.Name().String())
			if len(nameBytes) > 0 {
				for i := range content {
					content[i] = nameBytes[i%len(nameBytes)]
				}
			}
		}
		dataConfig := &ndn.DataConfig{
			ContentType: optional.Some(ndn.ContentTypeBlob),
		}
		if freshness > 0 {
			dataConfig.Freshness = optional.Some(freshness)
		}
		data, err := engine.Spec().MakeData(
			args.Interest.Name(),
			dataConfig,
			enc.Wire{content},
			dataSigner,
		)
		if err != nil {
			return
		}
		args.Reply(data.Wire)
		// Notify C++ that Data was produced
		C.callDataProduced(nodeId, C.uint32_t(data.Wire.Length()))
	}

	if err := engine.AttachHandler(name, handler); err != nil {
		return -1
	}

	// Install a local forwarder route so fw.Thread delivers incoming Interests
	// to the app face where the handler can receive them. Without this, the
	// forwarder drops all Interests for this prefix — they never reach the
	// handler registered above.
	engine.RegisterRoute(name)

	// If DV is running, announce this prefix so it propagates to neighbors
	node.AnnouncePrefixToDv(name, 0)

	return 0
}

//export NdndSimGetRibEntryCount
func NdndSimGetRibEntryCount(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int) C.int {
	if globalRuntime == nil {
		return 0
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return 0
	}

	entries := node.Forwarder.rib.GetAllEntries()

	// No prefix filter -- count all entries
	if prefixStr == nil || int(prefixLen) == 0 {
		return C.int(len(entries))
	}

	// Count only entries whose name starts with the given prefix
	prefix := C.GoStringN(prefixStr, prefixLen)
	filterName, err := parseNameFromString(prefix)
	if err != nil {
		return 0
	}

	count := 0
	for _, entry := range entries {
		if filterName.IsPrefix(entry.Name) {
			count++
		}
	}
	return C.int(count)
}

//export NdndSimGetDvSuppressionStats
func NdndSimGetDvSuppressionStats(nodeId C.uint32_t, enter *C.uint64_t, ok *C.uint64_t, fail *C.uint64_t) C.int {
	if globalRuntime == nil || enter == nil || ok == nil || fail == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}
	dv := node.DvRouter()
	if dv == nil {
		return -1
	}

	stats := dv.PrefixSyncSuppressionStats()
	*enter = C.uint64_t(stats.Enter)
	*ok = C.uint64_t(stats.Ok)
	*fail = C.uint64_t(stats.Fail)
	return 0
}

//export NdndSimAnnouncePrefixToDv
func NdndSimAnnouncePrefixToDv(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int) C.int {
	if globalRuntime == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return -1
	}

	node.AnnouncePrefixToDv(name, 0)
	return 0
}

//export NdndSimWithdrawPrefixFromDv
func NdndSimWithdrawPrefixFromDv(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int) C.int {
	if globalRuntime == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return -1
	}

	node.WithdrawPrefixFromDv(name)
	return 0
}

//export NdndSimRegisterConsumer
func NdndSimRegisterConsumer(nodeId C.uint32_t, prefixStr *C.char, prefixLen C.int, frequencyHz C.double, lifetimeMs C.uint32_t) C.int {
	if globalRuntime == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}

	prefix := C.GoStringN(prefixStr, prefixLen)
	name, err := parseNameFromString(prefix)
	if err != nil {
		return -1
	}

	engine := node.Engine()
	lifetime := time.Duration(lifetimeMs) * time.Millisecond

	val, ok := globalClocks.Load(uint32(nodeId))
	if !ok {
		return -1
	}
	clock := val.(*Ns3Clock)

	stopped := startConsumerLoop(engine, clock, uint32(nodeId), name, float64(frequencyHz), lifetime)
	consumerMu.Lock()
	consumerFlags[uint32(nodeId)] = append(consumerFlags[uint32(nodeId)], stopped)
	consumerMu.Unlock()

	return 0
}

//export NdndSimStopConsumer
func NdndSimStopConsumer(nodeId C.uint32_t) {
	consumerMu.Lock()
	flags := consumerFlags[uint32(nodeId)]
	delete(consumerFlags, uint32(nodeId))
	consumerMu.Unlock()
	for _, f := range flags {
		atomic.StoreInt32(f, 1)
	}
}
