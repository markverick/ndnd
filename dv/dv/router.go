package dv

import (
	"sync"
	"time"

	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/dv/nfdc"
	"github.com/named-data/ndnd/dv/table"
	"github.com/named-data/ndnd/fw/core"
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

func (dv *Router) PrefixSyncEnabled() bool {
	dv.mutex.Lock()
	defer dv.mutex.Unlock()
	return dv.pfxSvs != nil
}

func (dv *Router) PrefixSyncSuppressionStats() ndn_sync.SuppressStats {
	dv.mutex.Lock()
	defer dv.mutex.Unlock()
	if dv.pfxSvs == nil {
		return ndn_sync.SuppressStats{}
	}
	return dv.pfxSvs.SVS().SuppressionStats()
}

func (dv *Router) prefixSnapshotStrategy() ndn_sync.Snapshot {
	if dv.config.DisablePrefixSnap {
		return &ndn_sync.SnapshotNull{}
	}

	threshold := dv.config.PrefixSnapThreshold
	if threshold == 0 {
		threshold = PrefixSnapThreshold
	}

	return &ndn_sync.SnapshotNodeLatest{
		Client: dv.client,
		SnapMe: func(name enc.Name) (enc.Wire, error) {
			return dv.pfx.Snap(), nil
		},
		Threshold: threshold,
	}
}

type Router struct {
	// go-ndn app that this router is attached to
	engine ndn.Engine
	// config for this router
	config *config.Config
	// trust configuration
	trust *sec.TrustConfig
	// object client
	client ndn.Client
	// nfd management thread
	nfdc *nfdc.NfdMgmtThread
	// single mutex for all operations
	mutex sync.Mutex

	// GoFunc dispatches work asynchronously.
	// In production, this launches a goroutine.
	// In simulation, this schedules via the simulation clock.
	GoFunc func(func())

	// NowFunc returns current time. Defaults to time.Now.
	// In simulation, returns simulation clock time.
	NowFunc func() time.Time

	// AfterFunc schedules f after d. Returns cancel function.
	// Defaults to time.AfterFunc wrapper.
	AfterFunc func(time.Duration, func()) func()

	// channel to stop the DV
	stop chan bool
	// heartbeat for outgoing Advertisements
	heartbeat *time.Ticker
	// deadcheck for neighbors
	deadcheck *time.Ticker

	// advertisement module
	advert advertModule

	// prefix table
	pfx *table.PrefixTable
	// prefix table svs instance
	pfxSvs *ndn_sync.SvsALO
	// prefix table svs subscriptions
	pfxSubs map[uint64]enc.Name

	// neighbor table
	neighbors *table.NeighborTable
	// routing information base
	rib *table.Rib
	// forwarding table
	fib *table.Fib
}

// Create a new DV router.
func NewRouter(config *config.Config, engine ndn.Engine) (*Router, error) {
	// Validate configuration
	err := config.Parse()
	if err != nil {
		return nil, err
	}

	// Create packet store (use pre-built store if provided)
	var store ndn.Store
	if config.Store != nil {
		store = config.Store
	} else {
		store = storage.NewMemoryStore()
	}

	// Create security configuration
	var trust *sec.TrustConfig = nil
	if config.KeyChain != nil {
		// Use pre-built keychain (e.g. from simulation trust setup)
		schema, err := trust_schema.NewLvsSchema(config.SchemaBytes())
		if err != nil {
			return nil, err
		}
		anchors := config.TrustAnchorNames()
		trust, err = sec.NewTrustConfig(config.KeyChain, schema, anchors)
		if err != nil {
			return nil, err
		}
		trust.UseDataNameFwHint = true
	} else if config.KeyChainUri == "insecure" {
		log.Warn(nil, "Security is disabled - insecure mode")
	} else {
		kc, err := keychain.NewKeyChain(config.KeyChainUri, store)
		if err != nil {
			return nil, err
		}
		schema, err := trust_schema.NewLvsSchema(config.SchemaBytes())
		if err != nil {
			return nil, err
		}
		anchors := config.TrustAnchorNames()
		trust, err = sec.NewTrustConfig(kc, schema, anchors)
		if err != nil {
			return nil, err
		}

		// Attach data name as forwarding hint to cert Interests
		trust.UseDataNameFwHint = true
	}

	// Create the DV router
	dv := &Router{
		engine:  engine,
		config:  config,
		trust:   trust,
		client:  object.NewClient(engine, store, trust),
		nfdc:    nfdc.NewNfdMgmtThread(engine),
		mutex:   sync.Mutex{},
		GoFunc:  func(f func()) { go f() },
		NowFunc: core.Now,
		AfterFunc: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
	}

	// Initialize advertisement module.
	// NOTE: bootTime must NOT be computed here — NowFunc may still point to
	// the real wall clock at construction time.  For production Start() and
	// simulation Init(), bootTime is computed lazily via dv.NowFunc() at the
	// start of those methods, after any override (e.g. SimDvRouter) is in place.
	// createPrefixTable() is also deferred for the same reason: it embeds
	// bootTime into pfxSvs, which would then compare incoming BootstrapTimes
	// against the wrong clock.
	dv.advert = advertModule{
		dv:     dv,
		seq:    0,
		objDir: storage.NewMemoryFifoDir(32), // keep last few advertisements
	}

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

// Start the DV router. Blocks until Stop() is called.
func (dv *Router) Start() (err error) {
	log.Info(dv, "Starting DV router", "version", utils.NDNdVersion)
	defer log.Info(dv, "Stopped DV router")

	// Lazily initialize bootTime and prefix table so that any NowFunc
	// override (e.g. from simulation) is in effect before pfxSvs is created.
	dv.advert.bootTime = max(uint64(dv.NowFunc().Unix()), 1)
	dv.createPrefixTable()

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

	// Start sync groups (skip PrefixSync SVS in one-step mode)
	if !dv.config.OneStep {
		dv.pfxSvs.Start()
		defer dv.pfxSvs.Stop()
	}

	// Add self to the RIB and make initial advertisement
	dv.rib.Set(dv.config.RouterName(), dv.config.RouterName(), 0)
	dv.advert.generate()

	// Initialize prefix table (two-step only)
	if !dv.config.OneStep {
		dv.pfx.Reset()
	}

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

	// Lazily initialize bootTime and prefix table so that the NowFunc
	// override installed by SimDvRouter (clock.Now) is used instead of the
	// wall clock.  If bootTime were set at NewRouter() time, it would carry
	// the real Unix timestamp (~1773989593 in 2026), which SVS on every peer
	// would reject as "far future" because the simulation clock starts near
	// the Unix epoch.
	dv.advert.bootTime = max(uint64(dv.NowFunc().Unix()), 1)
	dv.createPrefixTable()

	// Start object client
	dv.client.Start()

	// Register interest handlers
	if err := dv.register(); err != nil {
		return err
	}

	// Start sync groups (skip PrefixSync SVS in one-step mode)
	if !dv.config.OneStep {
		if dv.config.PrefixSyncDelay_ms > 0 {
			delay := time.Duration(dv.config.PrefixSyncDelay_ms) * time.Millisecond
			dv.AfterFunc(delay, func() {
				dv.pfxSvs.Start()
				dv.pfx.Reset()
			})
		} else {
			dv.pfxSvs.Start()
		}
	}

	// Add self to the RIB and make initial advertisement
	dv.rib.Set(dv.config.RouterName(), dv.config.RouterName(), 0)
	dv.advert.generate()

	// Initialize prefix table (two-step only, immediate start)
	if !dv.config.OneStep && dv.config.PrefixSyncDelay_ms == 0 {
		dv.pfx.Reset()
	}

	return nil
}

// RunHeartbeat sends a sync Interest to all neighbors.
// In production this is driven by time.Ticker; in simulation it is
// called by the simulation clock scheduler.
func (dv *Router) RunHeartbeat() {
	dv.advert.sendSyncInterest()
}

// RunDeadcheck checks for dead neighbors and prunes routes.
// In production this is driven by time.Ticker; in simulation it is
// called by the simulation clock scheduler.
func (dv *Router) RunDeadcheck() {
	dv.checkDeadNeighbors()
}

// Cleanup tears down the DV router (simulation variant of deferred cleanup in Start).
func (dv *Router) Cleanup() {
	if !dv.config.OneStep {
		dv.pfxSvs.Stop()
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

	// Register routes to forwarder
	pfxs := []enc.Name{
		dv.config.AdvertisementSyncPrefix(),
		dv.config.AdvertisementDataPrefix(),
		dv.config.MgmtPrefix(),
	}
	// In two-step mode, also register PrefixSync SVS routes.
	if !dv.config.OneStep {
		pfxs = append(pfxs,
			dv.pfxSvs.SyncPrefix(),
			dv.pfxSvs.DataPrefix(),
		)
	}
	for _, prefix := range pfxs {
		dv.nfdc.Exec(nfdc.NfdMgmtCmd{
			Module: "rib",
			Cmd:    "register",
			Args: &mgmt.ControlArgs{
				Name:   prefix,
				Cost:   optional.Some(uint64(0)),
				Origin: optional.Some(config.NlsrOrigin),
			},
			Retries: -1,
		})
	}

	// Set strategy to multicast for sync prefixes
	pfxs = []enc.Name{
		dv.config.AdvertisementSyncPrefix(),
	}
	if !dv.config.OneStep {
		pfxs = append(pfxs, dv.pfxSvs.SyncPrefix())
	}
	for _, prefix := range pfxs {
		dv.nfdc.Exec(nfdc.NfdMgmtCmd{
			Module: "strategy-choice",
			Cmd:    "set",
			Args: &mgmt.ControlArgs{
				Name: prefix,
				Strategy: &mgmt.Strategy{
					Name: config.MulticastStrategy,
				},
			},
			Retries: -1,
		})
	}

	return nil
}

// createFaces creates faces to all neighbors.
func (dv *Router) createFaces() {
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

		dv.nfdc.Exec(nfdc.NfdMgmtCmd{
			Module: "rib",
			Cmd:    "register",
			Args: &mgmt.ControlArgs{
				Name:   dv.config.AdvertisementSyncActivePrefix(),
				Cost:   optional.Some(uint64(1)),
				Origin: optional.Some(config.NlsrOrigin),
				FaceId: optional.Some(faceId),
			},
			Retries: 3,
		})
	}
}

// destroyFaces synchronously destroys our faces to neighbors.
func (dv *Router) destroyFaces() {
	for _, neighbor := range dv.config.Neighbors {
		if neighbor.FaceId == 0 {
			continue
		}

		dv.engine.ExecMgmtCmd("rib", "unregister", &mgmt.ControlArgs{
			Name:   dv.config.AdvertisementSyncActivePrefix(),
			Origin: optional.Some(config.NlsrOrigin),
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

// (AI GENERATED DESCRIPTION): Initializes the router’s prefix table by creating a subscription map, setting up an SVS synchronization agent (with snapshot support) for publishing updates, and constructing a local prefix table that forwards changes to the SVS.
func (dv *Router) createPrefixTable() {
	// Subscription list
	dv.pfxSubs = make(map[uint64]enc.Name)

	// SVS delivery agent
	var err error
	dv.pfxSvs, err = ndn_sync.NewSvsALO(ndn_sync.SvsAloOpts{
		Name: dv.config.RouterName(),
		Svs: ndn_sync.SvSyncOpts{
			Client:      dv.client,
			GroupPrefix: dv.config.PrefixTableGroupPrefix(),
			BootTime:    dv.advert.bootTime,
			GoFunc:      func(f func()) { dv.GoFunc(f) },
			NowFunc:     func() time.Time { return dv.NowFunc() },
			AfterFunc:   func(d time.Duration, f func()) func() { return dv.AfterFunc(d, f) },
		},
		Snapshot: dv.prefixSnapshotStrategy(),
	})
	if err != nil {
		panic(err)
	}

	// Local prefix table
	dv.pfx = table.NewPrefixTable(dv.config, func(w enc.Wire) {
		if _, _, err := dv.pfxSvs.Publish(w); err != nil {
			log.Error(dv, "Failed to publish prefix table update", "err", err)
		}
	}, table.PrefixTableOptions{
		NowFunc: dv.NowFunc,
	})
}
