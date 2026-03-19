package sim

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

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

// Ns3Clock implements the Clock interface using ns-3's simulation time.
type Ns3Clock struct {
	nodeID   uint32
	nextEvID atomic.Uint64
	mu       sync.Mutex
	events   map[EventID]func()
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
	id := EventID(c.nextEvID.Add(1))

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
	c.mu.Lock()
	delete(c.events, id)
	c.mu.Unlock()

	C.callCancelEvent(C.uint64_t(id))
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
)

// --- Exported CGo functions called by ns-3 C++ code ---

//export NdndSimInit
func NdndSimInit(
	sendPacketCb C.NdndSimSendPacketFunc,
	scheduleEventCb C.NdndSimScheduleEventFunc,
	cancelEventCb C.NdndSimCancelEventFunc,
	getTimeNsCb C.NdndSimGetTimeNsFunc,
	dataProducedCb C.NdndSimDataProducedFunc,
	dataReceivedCb C.NdndSimDataReceivedFunc,
) {
	C.setSendPacketCb(sendPacketCb)
	C.setScheduleEventCb(scheduleEventCb)
	C.setCancelEventCb(cancelEventCb)
	C.setGetTimeNsCb(getTimeNsCb)
	C.setDataProducedCb(dataProducedCb)
	C.setDataReceivedCb(dataReceivedCb)

	// Override NDNd's clock to use ns-3 simulation time
	core.NowFunc = func() time.Time {
		ns := int64(C.callGetTimeNs())
		return time.Unix(0, ns)
	}

	// Create a placeholder clock for the runtime (actual per-node clocks are created per node)
	dummyClock := NewNs3Clock(0)
	globalRuntime = NewRuntime(dummyClock)
}

//export NdndSimCreateNode
func NdndSimCreateNode(nodeId C.uint32_t) C.int {
	if globalRuntime == nil {
		return -1
	}

	clock := NewNs3Clock(uint32(nodeId))
	globalClocks.Store(uint32(nodeId), clock)

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

	globalRuntime.mu.Lock()
	globalRuntime.nodes[uint32(nodeId)] = node
	globalRuntime.mu.Unlock()

	if err := node.Start(); err != nil {
		return -1
	}
	return 0
}

//export NdndSimDestroyNode
func NdndSimDestroyNode(nodeId C.uint32_t) {
	if globalRuntime == nil {
		return
	}
	globalRuntime.DestroyNode(uint32(nodeId))
	globalClocks.Delete(uint32(nodeId))
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
		// NDNd → ns-3: send packet through callback
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
func NdndSimStartDv(nodeId C.uint32_t, networkStr *C.char, networkLen C.int, routerStr *C.char, routerLen C.int) C.int {
	if globalRuntime == nil {
		return -1
	}
	node := globalRuntime.GetNode(uint32(nodeId))
	if node == nil {
		return -1
	}

	network := C.GoStringN(networkStr, networkLen)
	router := C.GoStringN(routerStr, routerLen)

	if err := node.StartDv(network, router); err != nil {
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
	if globalRuntime != nil {
		globalRuntime.DestroyAll()
		globalRuntime = nil
	}
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
		content := make([]byte, pSize)
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

	// No prefix filter — count all entries
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
		if len(entry.Name) >= len(filterName) {
			match := true
			for i, comp := range filterName {
				if !comp.Equal(entry.Name[i]) {
					match = false
					break
				}
			}
			if match {
				count++
			}
		}
	}
	return C.int(count)
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
