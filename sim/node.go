package sim

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/fw/defn"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
)

// Node represents a single simulated NDN node with isolated state.
// Each ns-3 node that runs NDNd gets one SimNode.
type Node struct {
	id uint32

	// Simulation clock (shared across all nodes, provided by ns-3)
	clock Clock

	// Forwarder for this node
	Forwarder *SimForwarder

	// Application-layer engine (for NDN apps on this node)
	appEngine ndn.Engine
	appFace   *SimFace
	appTimer  *SimTimer
	appFaceID uint64

	// DV router for this node (nil if not enabled)
	dvRouter *SimDvRouter

	// Mapping from ns-3 interface index to forwarder face ID
	ifaceFaces map[uint32]uint64

	mu sync.Mutex
}

// NewNode creates a new simulation node. The clock is typically shared
// across all nodes (backed by ns-3 Simulator::Now).
func NewNode(id uint32, clock Clock) *Node {
	n := &Node{
		id:         id,
		clock:      clock,
		ifaceFaces: make(map[uint32]uint64),
	}

	// Create the forwarder
	n.Forwarder = NewSimForwarder(clock)

	// Create the application-layer timer
	n.appTimer = NewSimTimer(clock)

	// Create the app face — sendFunc forwards to the forwarder's app face.
	// We use a closure that captures n so it can look up appFaceID at send time.
	n.appFace = NewSimFace(func(frame []byte) {
		n.Forwarder.ReceivePacket(n.appFaceID, frame)
	}, true)

	n.appEngine = NewSimEngine(n.appFace, n.appTimer, id, nil)

	return n
}

// Start initializes the node's forwarder and application engine.
func (n *Node) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Create internal application face in the forwarder
	n.appFaceID = n.Forwarder.AddFace(defn.Local, defn.PointToPoint, func(faceID uint64, frame []byte) {
		// Forwarder → App: deliver to the application face
		n.appFace.Receive(frame)
	})

	// Set forwarder and appFaceID on the engine for ExecMgmtCmd
	if eng, ok := n.appEngine.(*SimEngine); ok {
		eng.forwarder = n.Forwarder
		eng.appFaceID = n.appFaceID
	}

	n.Forwarder.Start()

	// Start the engine (this also opens the app face)
	if err := n.appEngine.Start(); err != nil {
		return fmt.Errorf("failed to start engine: %w", err)
	}

	return nil
}

// Stop shuts down the node.
func (n *Node) Stop() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.dvRouter != nil {
		n.dvRouter.Stop()
		n.dvRouter = nil
	}

	n.appEngine.Stop()
	n.appFace.Close()
	n.Forwarder.Stop()
}

// AddNetworkFace creates a new forwarder face for an ns-3 network interface.
// sendFunc is called when the forwarder wants to transmit a packet on this interface.
// Returns the face ID.
func (n *Node) AddNetworkFace(ifIndex uint32, sendFunc FwSendFunc) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	faceID := n.Forwarder.AddFace(defn.NonLocal, defn.PointToPoint, sendFunc)
	n.ifaceFaces[ifIndex] = faceID
	return faceID
}

// RemoveNetworkFace removes a forwarder face for an ns-3 network interface.
func (n *Node) RemoveNetworkFace(ifIndex uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if faceID, ok := n.ifaceFaces[ifIndex]; ok {
		n.Forwarder.RemoveFace(faceID)
		delete(n.ifaceFaces, ifIndex)
	}
}

// ReceiveOnInterface injects a packet received on an ns-3 network interface.
// ifIndex == 0xFFFFFFFF is the special app-face interface.
func (n *Node) ReceiveOnInterface(ifIndex uint32, frame []byte) {
	if ifIndex == 0xFFFFFFFF {
		// App face: deliver directly to the forwarder on the app face
		n.Forwarder.ReceivePacket(n.appFaceID, frame)
		return
	}

	n.mu.Lock()
	faceID, ok := n.ifaceFaces[ifIndex]
	n.mu.Unlock()

	if ok {
		n.Forwarder.ReceivePacket(faceID, frame)
	}
}

// GetFaceForInterface returns the forwarder face ID for an ns-3 interface.
func (n *Node) GetFaceForInterface(ifIndex uint32) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	faceID, ok := n.ifaceFaces[ifIndex]
	return faceID, ok
}

// AddRoute adds a FIB entry for this node.
func (n *Node) AddRoute(name enc.Name, faceID uint64, cost uint64) {
	n.Forwarder.AddRoute(name, faceID, cost)
}

// RemoveRoute removes a FIB entry for this node.
func (n *Node) RemoveRoute(name enc.Name, faceID uint64) {
	n.Forwarder.RemoveRoute(name, faceID)
}

// Engine returns the application-layer engine for this node.
func (n *Node) Engine() ndn.Engine {
	return n.appEngine
}

// Clock returns the simulation clock.
func (n *Node) Clock() Clock {
	return n.clock
}

// ID returns the node identifier.
func (n *Node) ID() uint32 {
	return n.id
}

// AppFaceID returns the face ID for the internal application face.
func (n *Node) AppFaceID() uint64 {
	return n.appFaceID
}

// StartDv creates and starts a DV router on this node.
// network is the network prefix (e.g., "/ndn"), routerName is the full
// router name (e.g., "/ndn/node0"). The DV router discovers neighbors
// dynamically via sync Interests on all connected faces.
func (n *Node) StartDv(network, router string, cfgJSON string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	cfg := config.DefaultConfig()
	cfg.Network = network
	cfg.Router = router
	if cfgJSON != "" {
		if err := json.Unmarshal([]byte(cfgJSON), cfg); err != nil {
			return fmt.Errorf("bad DV config JSON: %w", err)
		}
		// Restore fields that must not be overridden from the outside
		cfg.Network = network
		cfg.Router = router
	}

	// Set up Ed25519 trust (same pipeline as emulation)
	trust, err := GetSimTrust(network)
	if err != nil {
		return fmt.Errorf("failed to init trust: %w", err)
	}
	kc, store, anchors, err := trust.NodeKeychain(router)
	if err != nil {
		return fmt.Errorf("failed to build node keychain: %w", err)
	}
	cfg.KeyChain = kc
	cfg.Store = store
	cfg.TrustAnchors = anchors

	sdv, err := NewSimDvRouter(n.clock, n.appEngine, cfg)
	if err != nil {
		return err
	}

	if err := sdv.Start(); err != nil {
		return err
	}

	// Register DV sync prefixes on all link faces for neighbor reachability.
	// This replaces the production DV's createFaces() which creates and
	// configures neighbor faces — in simulation, faces already exist.
	syncActivePrefix := cfg.AdvertisementSyncActivePrefix()
	pfxSyncGroup := cfg.PrefixTableGroupPrefix()
	for _, faceID := range n.ifaceFaces {
		n.Forwarder.AddRoute(syncActivePrefix, faceID, 1)
		n.Forwarder.AddRoute(pfxSyncGroup, faceID, 1)
	}

	// Also add app face at the ACT prefix so incoming sync Interests from
	// neighbors can be delivered to the DV handler. Without this, the ACT
	// node in the FIB (link faces only) shadows the parent ADS node (app
	// face), causing /localhop scope enforcement to drop all nexthops.
	n.Forwarder.AddRoute(syncActivePrefix, n.appFaceID, 0)

	// Set multicast strategy for sync prefixes so Interests go to ALL neighbors.
	multicastStrategy := config.MulticastStrategy.Append(enc.NewVersionComponent(1))
	n.Forwarder.SetStrategy(cfg.AdvertisementSyncPrefix(), multicastStrategy)
	n.Forwarder.SetStrategy(pfxSyncGroup, multicastStrategy)

	n.dvRouter = sdv
	return nil
}

// StopDv stops the DV router if running.
func (n *Node) StopDv() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dvRouter != nil {
		n.dvRouter.Stop()
		n.dvRouter = nil
	}
}

// DvRouter returns the DV router wrapper, or nil if not started.
func (n *Node) DvRouter() *SimDvRouter {
	return n.dvRouter
}

// AnnouncePrefixToDv announces a prefix to the DV router if DV is running.
// This triggers DV prefix table propagation to all neighbors.
func (n *Node) AnnouncePrefixToDv(name enc.Name, cost uint64) {
	n.mu.Lock()
	dv := n.dvRouter
	n.mu.Unlock()
	if dv != nil {
		dv.AnnouncePrefix(name, n.appFaceID, cost)
	}
}

// WithdrawPrefixFromDv withdraws a prefix from the DV router if DV is running.
// This triggers DV prefix table removal propagation to all neighbors.
func (n *Node) WithdrawPrefixFromDv(name enc.Name) {
	n.mu.Lock()
	dv := n.dvRouter
	n.mu.Unlock()
	if dv != nil {
		dv.WithdrawPrefix(name, n.appFaceID)
	}
}
