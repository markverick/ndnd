package dv

import (
	"fmt"
	"sync"
	"time"

	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/dv/nfdc"
	"github.com/named-data/ndnd/dv/table"
	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	"github.com/named-data/ndnd/std/object"
	"github.com/named-data/ndnd/std/object/storage"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/security/keychain"
	"github.com/named-data/ndnd/std/security/trust_schema"
	ndn_sync "github.com/named-data/ndnd/std/sync"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

const PrefixSnapThreshold = 50

// bundledDvSchema is the compiled LVS trust schema embedded in the dv/config package.
// It is captured here at package level to avoid shadowing by the 'config' function
// parameter inside NewRouter.
var bundledDvSchema = config.SchemaBytes

// PrefixSyncSuppressionStats returns SVS suppression statistics.
func (dv *Router) PrefixSyncSuppressionStats() ndn_sync.SuppressStats {
	if dv.pfx == nil {
		return ndn_sync.SuppressStats{}
	}
	return dv.pfx.SuppressionStats()
}

type Router struct {
	// go-ndn app that this router is attached to
	engine ndn.Engine
	// config for this router
	config *config.Config
	// trust configuration
	trust *sec.TrustConfig
	// prefix insertion trust configuration
	insertionTrust *sec.TrustConfig
	// object client
	client ndn.Client
	// nfd management thread
	nfdc *nfdc.NfdMgmtThread
	// single mutex for all operations
	mutex sync.Mutex

	// channel to stop the DV
	stop chan bool
	// heartbeat for outgoing Advertisements
	heartbeat *time.Ticker
	// deadcheck for neighbors
	deadcheck *time.Ticker

	// advertisement module
	advert advertModule

	// prefix egress state daemon
	pfx *PrefixModule
	// neighbor table
	neighbors *table.NeighborTable
	// routing information base
	rib *table.Rib
	// forwarding table
	fib *table.Fib

	// The following fields must be set before calling Start() or Init().
	// Changing them after either method is called has no effect.

	// GoFunc dispatches work asynchronously.
	// Defaults to "go f()" in production; overridden by sim.
	GoFunc func(func())
	// NowFunc returns current time. Defaults to core.Now.
	NowFunc func() time.Time
	// AfterFunc schedules f after d. Returns cancel function.
	// Defaults to time.AfterFunc wrapper.
	AfterFunc func(time.Duration, func()) func()
	// EnableFaceEvents controls the face-event polling loop used for local
	// prefix pruning. Simulation disables it because the pure-Go harness uses
	// a single-threaded forwarder/engine and this loop would introduce
	// concurrent management traffic.
	EnableFaceEvents bool
}

// Create a new DV router.
func NewRouter(config *config.Config, engine ndn.Engine) (*Router, error) {
	// Validate configuration
	err := config.Parse()
	if err != nil {
		return nil, err
	}

	// Create packet store
	var store ndn.Store = storage.NewMemoryStore()
	if config.Store != nil {
		store = config.Store
	}

	// Create security configuration
	var trust *sec.TrustConfig = nil
	if config.KeyChain != nil {
		// Simulation: use the same bundled LVS schema as production so that
		// Check() enforces the same signing policy and Suggest() selects
		// the matching identity key from the pre-built keychain.
		anchors := config.SimTrustAnchors
		if len(anchors) == 0 {
			anchors = config.TrustAnchorNames()
		}
		schema, err2 := trust_schema.NewLvsSchema(bundledDvSchema)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse bundled DV trust schema: %w", err2)
		}
		trust, err2 = sec.NewTrustConfig(config.KeyChain, schema, anchors)
		if err2 != nil {
			return nil, err2
		}
		trust.UseDataNameFwHint = true
	} else if config.KeyChainUri == "insecure" {
		log.Warn(nil, "Security is disabled - insecure mode")
	} else {
		kc, err := keychain.NewKeyChain(config.KeyChainUri, store)
		if err != nil {
			return nil, err
		}
		var schema ndn.TrustSchema
		if schemaBytes := config.SchemaBytes(); len(schemaBytes) > 0 {
			schema, err = trust_schema.NewLvsSchema(schemaBytes)
			if err != nil {
				return nil, err
			}
		} else {
			schema = trust_schema.NewNullSchema()
		}
		anchors := config.TrustAnchorNames()
		trust, err = sec.NewTrustConfig(kc, schema, anchors)
		if err != nil {
			return nil, err
		}

		// Attach data name as forwarding hint to cert Interests
		trust.UseDataNameFwHint = true
	}

	// Prefix insertion security is enabled via schema configured in dv config.
	var insertionTrust *sec.TrustConfig
	if config.KeyChain != nil {
		// Simulation: same bundled LVS schema for insertion trust, enforcing the same policy.
		anchors := config.SimTrustAnchors
		if len(anchors) == 0 {
			anchors = config.PrefixInsertionTrustAnchorNames()
		}
		insertionSchema, err2 := trust_schema.NewLvsSchema(bundledDvSchema)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse bundled DV insertion trust schema: %w", err2)
		}
		insertionTrust, err = sec.NewTrustConfig(config.KeyChain, insertionSchema, anchors)
		if err != nil {
			return nil, fmt.Errorf("failed to create sim prefix insertion trust config: %w", err)
		}
		insertionTrust.UseDataNameFwHint = true
	} else {
		insertionStore := storage.NewMemoryStore()
		kc, err := keychain.NewKeyChain(config.PrefixInsertionKeychainUri, insertionStore)
		if err != nil {
			return nil, fmt.Errorf("failed to open prefix insertion keychain: %w", err)
		}

		var insertionSchema ndn.TrustSchema
		if schemaBytes := config.PrefixInsertionSchemaBytes(); len(schemaBytes) > 0 {
			insertionSchema, err = trust_schema.NewLvsSchema(schemaBytes)
			if err != nil {
				return nil, fmt.Errorf("failed to parse prefix insertion trust schema: %w", err)
			}
		} else {
			insertionSchema = trust_schema.NewNullSchema()
		}

		anchors := config.PrefixInsertionTrustAnchorNames()
		insertionTrust, err = sec.NewTrustConfig(kc, insertionSchema, anchors)
		if err != nil {
			return nil, fmt.Errorf("failed to create prefix insertion trust config: %w", err)
		}
		insertionTrust.UseDataNameFwHint = true
	}

	// Create the DV router
	dv := &Router{
		engine: engine,
		config: config,
		trust:  trust,
		insertionTrust: insertionTrust,
		client: object.NewClient(engine, store, trust),
		nfdc:   nfdc.NewNfdMgmtThread(engine),
		mutex:  sync.Mutex{},
		GoFunc:  func(f func()) { go f() },
		NowFunc: core.Now,
		AfterFunc: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
		EnableFaceEvents: true,
	}

	// NOTE: bootTime must NOT be computed here — NowFunc may still point to
	// core.Now (wall clock). In simulation, SimDvRouter overrides NowFunc to
	// the sim clock AFTER construction. bootTime is computed lazily in
	// Start() / Init() when NowFunc is guaranteed to be correct.

	// Create DV tables
	dv.neighbors = table.NewNeighborTable(config, dv.nfdc)
	dv.rib = table.NewRib(config)
	dv.fib = table.NewFib(config, dv.nfdc)

	return dv, nil
}

// Log identifier for the DV router.
func (dv *Router) String() string {
	return "dv-router"
}

func (dv *Router) ensurePrefixModule() {
	if dv.pfx != nil {
		return
	}
	dv.pfx = NewPrefixModule(dv.config, dv.client, dv.insertionTrust, dv.nfdc,
		dv.GoFunc, dv.NowFunc, dv.AfterFunc, dv.EnableFaceEvents)
}

// Start the DV router. Blocks until Stop() is called.
func (dv *Router) Start() (err error) {
	log.Info(dv, "Starting DV router", "version", utils.NDNdVersion)
	defer log.Info(dv, "Stopped DV router")
	dv.ensurePrefixModule()

	// Lazily initialize bootTime and advert module so that any NowFunc
	// override is in effect.
	dv.advert = advertModule{
		dv:       dv,
		bootTime: max(uint64(dv.NowFunc().Unix()), 1),
		seq:      0,
		objDir:   storage.NewMemoryFifoDir(32),
	}

	// Initialize channels
	dv.stop = make(chan bool, 1)

	// Register neighbor faces
	dv.createFaces()
	defer dv.destroyFaces()

	// Start timers
	dv.heartbeat = time.NewTicker(dv.config.AdvertisementSyncInterval())
	dv.deadcheck = time.NewTicker(dv.config.RouterDeadInterval())
	defer dv.heartbeat.Stop()
	defer dv.deadcheck.Stop()

	// Start object client
	dv.client.Start()
	defer dv.client.Stop()

	// Start management thread
	dv.GoFunc(func() { dv.nfdc.Start() })
	defer dv.nfdc.Stop()

	// Configure face
	if err = dv.configureFace(); err != nil {
		return err
	}

	// Register interest handlers
	if err = dv.register(); err != nil {
		return err
	}

	// Start prefix information daemon
	dv.pfx.Start()
	defer dv.pfx.Stop()

	// Add self to the RIB and make initial advertisement
	dv.rib.Set(dv.config.RouterName(), dv.config.RouterName(), 0)
	dv.advert.generate()

	// Initialize prefix egress state
	dv.pfx.Reset()

	for {
		select {
		case <-dv.heartbeat.C:
			dv.advert.sendSyncInterest()
		case <-dv.deadcheck.C:
			dv.checkDeadNeighbors()
		case <-dv.stop:
			return nil
		}
	}
}

// Stop the DV router.
func (dv *Router) Stop() {
	dv.stop <- true
}

// Init initializes the DV router without blocking.
// This is the simulation-compatible variant of Start() — it performs all
// setup (faces, handlers, sync groups, initial advertisement) but returns
// immediately instead of entering a ticker-driven select loop.
// The caller is responsible for periodically calling RunHeartbeat() and
// RunDeadcheck() using the simulation clock.
func (dv *Router) Init() error {
	log.Info(dv, "Initializing DV router (sim)", "version", utils.NDNdVersion)
	dv.ensurePrefixModule()

	// Lazily initialize bootTime and advert module so that the NowFunc
	// override installed by SimDvRouter (clock.Now) is used instead of
	// the wall clock.
	dv.advert = advertModule{
		dv:       dv,
		bootTime: max(uint64(dv.NowFunc().Unix()), 1),
		seq:      0,
		objDir:   storage.NewMemoryFifoDir(32),
	}

	// Start object client
	dv.client.Start()

	// Register interest handlers
	if err := dv.register(); err != nil {
		return err
	}

	// Start prefix information daemon
	dv.pfx.Start()

	// Add self to the RIB and make initial advertisement
	dv.rib.Set(dv.config.RouterName(), dv.config.RouterName(), 0)
	dv.advert.generate()

	// Initialize prefix egress state
	dv.pfx.Reset()

	return nil
}

// RunHeartbeat sends a sync Interest to all neighbors.
func (dv *Router) RunHeartbeat() {
	dv.advert.sendSyncInterest()
}

// RunDeadcheck checks for dead neighbors and prunes routes.
func (dv *Router) RunDeadcheck() {
	dv.checkDeadNeighbors()
}

// Cleanup tears down the DV router (simulation variant).
func (dv *Router) Cleanup() {
	if dv.pfx != nil {
		dv.pfx.Stop()
	}
	dv.client.Stop()
	log.Info(dv, "Cleaned up DV router (sim)")
}

// Nfdc returns the NFD management thread.
func (dv *Router) Nfdc() *nfdc.NfdMgmtThread {
	return dv.nfdc
}

// Configure the face to forwarder.
func (dv *Router) configureFace() (err error) {
	// Enable local fields on face. This includes incoming face indication.
	dv.nfdc.Exec(nfdc.NfdMgmtCmd{
		Module: "faces",
		Cmd:    "update",
		Args: &mgmt.ControlArgs{
			Mask:  optional.Some(mgmt.FaceFlagLocalFieldsEnabled),
			Flags: optional.Some(mgmt.FaceFlagLocalFieldsEnabled),
		},
		Retries: -1,
	})

	return nil
}

// Register interest handlers for DV prefixes.
func (dv *Router) register() (err error) {
	neighborsPrefix := enc.LOCALHOP.Append(enc.NewGenericComponent("neighbors"))

	// Advertisement Sync (active)
	err = dv.engine.AttachHandler(dv.config.AdvertisementSyncActivePrefix(),
		func(args ndn.InterestHandlerArgs) {
			dv.GoFunc(func() { dv.advert.OnSyncInterest(args, true) })
		})
	if err != nil {
		return err
	}

	// Advertisement Sync (passive)
	err = dv.engine.AttachHandler(dv.config.AdvertisementSyncPassivePrefix(),
		func(args ndn.InterestHandlerArgs) {
			dv.GoFunc(func() { dv.advert.OnSyncInterest(args, false) })
		})
	if err != nil {
		return err
	}

	// Router management
	err = dv.engine.AttachHandler(dv.config.MgmtPrefix(),
		func(args ndn.InterestHandlerArgs) {
			dv.GoFunc(func() { dv.mgmtOnInterest(args) })
		})
	if err != nil {
		return err
	}

	insertPrefix := dv.pfx.InsertionPrefix()
	err = dv.engine.AttachHandler(insertPrefix,
		func(args ndn.InterestHandlerArgs) {
			dv.GoFunc(func() { dv.pfx.OnInsertion(args) })
		})
	if err != nil {
		return err
	}

	// Register routes to forwarder
	pfxs := []enc.Name{
		dv.config.AdvertisementSyncPrefix(),
		dv.config.AdvertisementDataPrefix(),
		dv.pfx.SyncPrefix(),
		dv.pfx.DataPrefix(),
		dv.config.MgmtPrefix(),
		insertPrefix,
	}

	for _, prefix := range pfxs {
		dv.execMgmtRetry("pet", "add-nexthop", &mgmt.ControlArgs{
			Name: prefix,
		})
	}
	// Allow outgoing local-prefix-sync Interests to use two-phase forwarding.
	// Incoming Interests still terminate locally on the same prefix.
	dv.execMgmtRetry("pet", "add-egress", &mgmt.ControlArgs{
		Name:      dv.pfx.SyncPrefix(),
		Egress:    &mgmt.EgressRecord{Name: neighborsPrefix.Clone()},
		Multicast: true,
	})
	// Set Advertisement Sync to localhop neighbors
	dv.execMgmtRetry("pet", "add-egress", &mgmt.ControlArgs{
		Name:      dv.config.AdvertisementSyncPrefix(),
		Egress:    &mgmt.EgressRecord{Name: neighborsPrefix.Clone()},
		Multicast: true,
	})

	// Force multicast strategy for sync prefixes to broadcast.
	broadcastPrefixes := []enc.Name{
		dv.pfx.SyncPrefix(),
		dv.config.AdvertisementSyncPrefix(),
	}
	for _, prefix := range broadcastPrefixes {
		dv.execMgmtRetry("multicast-strategy-choice", "set", &mgmt.ControlArgs{
			Name:     prefix,
			Strategy: &mgmt.Strategy{Name: defn.BROADCAST_STRATEGY},
		})
	}

	return nil
}

func (dv *Router) execMgmtRetry(module, cmd string, args *mgmt.ControlArgs) {
	var err error
	for i := 0; ; i++ {
		if _, err = dv.engine.ExecMgmtCmd(module, cmd, args); err == nil {
			break
		}
		log.Error(dv, "Forwarder command failed", "err", err, "attempt", i,
			"module", module, "cmd", cmd, "args", args)
		time.Sleep(100 * time.Millisecond)
	}
}

// createFaces creates faces to all neighbors.
func (dv *Router) createFaces() {
	neighborsPrefix := enc.LOCALHOP.Append(enc.NewGenericComponent("neighbors"))

	for i, neighbor := range dv.config.Neighbors {
		var mtu optional.Optional[uint64]
		if neighbor.Mtu > 0 {
			mtu = optional.Some(neighbor.Mtu)
		}

		faceId, created, err := dv.nfdc.CreateFace(&mgmt.ControlArgs{
			Uri:             optional.Some(neighbor.Uri),
			FacePersistency: optional.Some(uint64(mgmt.PersistencyPermanent)),
			Mtu:             mtu,
		})
		if err != nil {
			log.Error(dv, "Failed to create face to neighbor", "uri", neighbor.Uri, "err", err)
			continue
		}
		log.Info(dv, "Created face to neighbor", "uri", neighbor.Uri, "faceId", faceId)

		dv.mutex.Lock()
		dv.config.Neighbors[i].FaceId = faceId
		dv.config.Neighbors[i].Created = created
		dv.mutex.Unlock()

		// Add neighbor to localhop neighbors
		dv.execMgmtRetry("fib", "add-nexthop", &mgmt.ControlArgs{
			Name:   neighborsPrefix.Clone(),
			Cost:   optional.Some(uint64(1)),
			FaceId: optional.Some(faceId),
		})
	}
}

// destroyFaces synchronously destroys our faces to neighbors.
func (dv *Router) destroyFaces() {
	neighborsPrefix := enc.LOCALHOP.Append(enc.NewGenericComponent("neighbors"))

	for _, neighbor := range dv.config.Neighbors {
		if neighbor.FaceId == 0 {
			continue
		}

		dv.engine.ExecMgmtCmd("fib", "remove-nexthop", &mgmt.ControlArgs{
			Name:   neighborsPrefix.Clone(),
			FaceId: optional.Some(neighbor.FaceId),
		})

		// only destroy faces that we created
		if neighbor.Created {
			dv.engine.ExecMgmtCmd("faces", "destroy", &mgmt.ControlArgs{
				FaceId: optional.Some(neighbor.FaceId),
			})
		}
	}
}
