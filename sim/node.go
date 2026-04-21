package sim

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ndndsim "github.com/named-data/ndndsim"
	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/face"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
)

// simInitMu guards one-time initialisation of the face and table subsystems.
// Using a mutex+bool rather than sync.Once lets NdndSimDestroy reset the flag
// so a subsequent re-init runs face.Initialize() and table.Initialize() again.
var (
	simInitMu   sync.Mutex
	simInitDone bool
)

// Node represents a single simulated NDN node with isolated state.
// Each ns-3 node that runs NDNd gets one SimNode.
type Node struct {
	id uint32

	// Simulation clock (shared across all nodes, provided by ns-3)
	clock Clock

	// Per-node simulation hooks (scheduler, clock, FIB, PET, keychain).
	// All ndnd code running on behalf of this node reads these via
	// goroutine-local storage after a BindNode call at each CGo boundary.
	hooks *_ndndsim.NodeHooks

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
	simInitMu.Lock()
	if !simInitDone {
		face.Initialize()
		table.Initialize()
		simInitDone = true
	}
	simInitMu.Unlock()

	// Override the global NDNd clock so that the Content Store (and any other
	// time-sensitive code that calls core.Now()) uses simulation time instead
	// of the wall clock. Without this, CS entries created in phase 1 of a test
	// would still appear "fresh" in phase 2 despite 30s of simulated time
	// elapsing (because only ~80ms of wall time passes).
	core.NowFunc = clock.Now

	// Create per-node hooks.  Scheduling defaults to real goroutines;
	// DV-specific overrides are applied later in StartDv / NewSimDvRouter.
	hooks := &_ndndsim.NodeHooks{
		GoFunc: func(f func()) { go f() },
		AfterFunc: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
		Now:              clock.Now,
		Synchronous:      true,
		EnableFaceEvents: false,
	}

	n := &Node{
		id:         id,
		clock:      clock,
		hooks:      hooks,
		ifaceFaces: make(map[uint32]uint64),
	}

	// Create the forwarder — this also sets Fib and Pet in the hooks.
	n.Forwarder = NewSimForwarder(clock, hooks)

	// Create the application-layer timer
	n.appTimer = NewSimTimer(clock)

	// Create the app face -- sendFunc forwards to the forwarder's app face.
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
			// Forwarder -> App: deliver to the application face
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

	if faceID, ok := n.ifaceFaces[ifIndex]; ok {
		return faceID
	}

	faceID := n.Forwarder.AddFace(defn.NonLocal, defn.PointToPoint, sendFunc)
	n.ifaceFaces[ifIndex] = faceID
	if n.dvRouter != nil {
		n.Forwarder.AddRouteWithOrigin(n.dvRouter.neighborsPrefix, faceID, 1, config.NlsrOrigin)
	}
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
// Binds this node's hooks for the duration of processing so that goroutine-
// local simFib/simPet return the correct per-node instances.
func (n *Node) ReceiveOnInterface(ifIndex uint32, frame []byte) {
	_ndndsim.BindNode(n.hooks)
	defer _ndndsim.UnbindNode()
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

// Hooks returns the per-node simulation hooks.
// The caller must call ndndsim.BindNode(n.Hooks()) before executing any
// ndnd code on behalf of this node.
func (n *Node) Hooks() *_ndndsim.NodeHooks {
	return n.hooks
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

	sdv, err := NewSimDvRouter(n.clock, n.appEngine, cfg, n.hooks, n.Forwarder)
	if err != nil {
		return err
	}

	if err := sdv.Start(n.hooks); err != nil {
		return err
	}

	// Replicate production createFaces(): register /localhop/neighbors on
	// each link face so multicast sync traffic can reach neighbors. All other
	// routes (ADS data, PFS sync/data, user prefixes) are installed by the
	// production DV code through routeRegister() and updateFib().
	neighborsPrefix := sdv.neighborsPrefix
	for _, faceID := range n.ifaceFaces {
		n.Forwarder.AddRouteWithOrigin(neighborsPrefix, faceID, 1, config.NlsrOrigin)
	}

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
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dvRouter
}

// AnnouncePrefixToDv announces a prefix to the DV router if DV is running.
// This triggers DV prefix table propagation to all neighbors.
func (n *Node) AnnouncePrefixToDv(name enc.Name, cost uint64) {
	n.mu.Lock()
	dv := n.dvRouter
	faceID := n.appFaceID
	n.mu.Unlock()
	if dv != nil {
		dv.AnnouncePrefix(name, faceID, cost)
	}
}

// WithdrawPrefixFromDv withdraws a prefix from the DV router if DV is running.
// This triggers DV prefix table removal propagation to all neighbors.
func (n *Node) WithdrawPrefixFromDv(name enc.Name) {
	n.mu.Lock()
	dv := n.dvRouter
	faceID := n.appFaceID
	n.mu.Unlock()
	if dv != nil {
		dv.WithdrawPrefix(name, faceID)
	}
}
