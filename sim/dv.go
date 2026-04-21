package sim

import (
	"fmt"
	"time"

	_ndndsim "github.com/named-data/ndndsim"
	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/dv/dv"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	sig "github.com/named-data/ndnd/std/security/signer"
	ndn_sync "github.com/named-data/ndnd/std/sync"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

// SimDvRouter wraps a DV Router for simulation. It manages the DV lifecycle
// using the simulation clock instead of time.Ticker and goroutines.
type SimDvRouter struct {
	router *dv.Router
	clock  Clock
	engine ndn.Engine
	hooks  *_ndndsim.NodeHooks
	fwd    *SimForwarder // per-node forwarder, used to activate withNodeFib

	// Scheduled heartbeat and deadcheck events
	heartbeatEvent EventID
	deadcheckEvent EventID

	// Configuration intervals
	heartbeatInterval time.Duration
	deadcheckInterval time.Duration
	neighborsPrefix   enc.Name
}

// NewSimDvRouter creates a DV router for a simulation node.
// The engine must be started before calling this.
// hooks is the node's hooks set; this function updates GoFunc, AfterFunc, Now,
// and KeyChain in it to use simulation-clock-based scheduling.
func NewSimDvRouter(clock Clock, engine ndn.Engine, cfg *config.Config, hooks *_ndndsim.NodeHooks, fwd *SimForwarder) (*SimDvRouter, error) {
	router, err := dv.NewRouter(cfg, engine)
	if err != nil {
		return nil, fmt.Errorf("failed to create DV router: %w", err)
	}

	// Override scheduling functions on both the Router struct (used by the
	// fork's existing code paths that call dv.GoFunc/AfterFunc directly) and
	// the node hooks (used by transformed code via _ndndsim.Go / AfterFunc).
	//
	// Each scheduled callback is wrapped in BindNode + withNodeFib so that
	// any FIB/PET writes (e.g., DV route updates, NFDC commands) go to this
	// node's per-node instances rather than the global tables.
	makeGoFunc := func(f func()) {
		h := hooks
		clock.Schedule(0, func() {
			_ndndsim.BindNode(h)
			defer _ndndsim.UnbindNode()
			fwd.withNodeFib(f)
		})
	}
	makeAfterFunc := func(d time.Duration, f func()) func() {
		h := hooks
		id := clock.Schedule(d, func() {
			_ndndsim.BindNode(h)
			defer _ndndsim.UnbindNode()
			fwd.withNodeFib(f)
		})
		return func() { clock.Cancel(id) }
	}

	// Set struct fields (used by fork's router.go which calls dv.GoFunc directly)
	router.GoFunc = makeGoFunc
	router.NowFunc = clock.Now
	router.AfterFunc = makeAfterFunc
	router.EnableFaceEvents = false
	router.Nfdc().Synchronous = true

	// Also update node hooks (used by transformed code and BindNode paths)
	hooks.GoFunc = makeGoFunc
	hooks.AfterFunc = makeAfterFunc
	hooks.Now = clock.Now
	if cfg.KeyChain != nil {
		hooks.KeyChain = cfg.KeyChain
	}

	return &SimDvRouter{
		router:            router,
		clock:             clock,
		engine:            engine,
		hooks:             hooks,
		fwd:               fwd,
		heartbeatInterval: cfg.AdvertisementSyncInterval(),
		deadcheckInterval: cfg.RouterDeadInterval(),
		neighborsPrefix: enc.LOCALHOP.
			Append(enc.NewGenericComponent("neighbors")),
	}, nil
}

// Start initializes the DV router and schedules heartbeat/deadcheck events.
// hooks must be the same *NodeHooks passed to NewSimDvRouter; they are bound
// to the current goroutine for the duration of Init() so that the correct
// simulation clock is used for boot-time computation and initial callbacks.
func (sd *SimDvRouter) Start(hooks *_ndndsim.NodeHooks) error {
	_ndndsim.BindNode(hooks)
	defer _ndndsim.UnbindNode()

	var initErr error
	sd.fwd.withNodeFib(func() {
		initErr = sd.router.Init()
	})
	if initErr != nil {
		return initErr
	}

	sd.scheduleHeartbeat()
	sd.scheduleDeadcheck()

	log.Info(nil, "DV router started in simulation mode")
	return nil
}

// Stop cancels scheduled events and cleans up the router.
func (sd *SimDvRouter) Stop() {
	if sd.heartbeatEvent != 0 {
		sd.clock.Cancel(sd.heartbeatEvent)
		sd.heartbeatEvent = 0
	}
	if sd.deadcheckEvent != 0 {
		sd.clock.Cancel(sd.deadcheckEvent)
		sd.deadcheckEvent = 0
	}
	sd.router.Cleanup()
}

func (sd *SimDvRouter) scheduleHeartbeat() {
	hooks := sd.hooks
	fwd := sd.fwd
	sd.heartbeatEvent = sd.clock.Schedule(sd.heartbeatInterval, func() {
		_ndndsim.BindNode(hooks)
		defer _ndndsim.UnbindNode()
		fwd.withNodeFib(func() {
			sd.router.RunHeartbeat()
		})
		sd.scheduleHeartbeat()
	})
}

func (sd *SimDvRouter) scheduleDeadcheck() {
	hooks := sd.hooks
	fwd := sd.fwd
	sd.deadcheckEvent = sd.clock.Schedule(sd.deadcheckInterval, func() {
		_ndndsim.BindNode(hooks)
		defer _ndndsim.UnbindNode()
		fwd.withNodeFib(func() {
			sd.router.RunDeadcheck()
		})
		sd.scheduleDeadcheck()
	})
}

// Router returns the underlying DV router.
func (sd *SimDvRouter) Router() *dv.Router {
	return sd.router
}

func (sd *SimDvRouter) PrefixSyncSuppressionStats() ndn_sync.SuppressStats {
	return sd.router.PrefixSyncSuppressionStats()
}

// AnnouncePrefix sends a readvertise Interest to the DV router's management
// handler, causing it to announce the prefix to all DV neighbors.
// This replicates what the production forwarder's NlsrReadvertiser does when
// a new RIB entry is created.
func (sd *SimDvRouter) AnnouncePrefix(name enc.Name, faceId uint64, cost uint64) {
	eng, ok := sd.engine.(*SimEngine)
	if !ok {
		return
	}

	params := &mgmt.ControlParameters{
		Val: &mgmt.ControlArgs{
			Name:   name,
			FaceId: optional.Some(faceId),
			Cost:   optional.Some(cost),
		},
	}

	cmd := enc.Name{
		enc.LOCALHOST,
		enc.NewGenericComponent("dv"),
		enc.NewGenericComponent("prefix"),
		enc.NewGenericComponent("announce"),
		enc.NewGenericBytesComponent(params.Encode().Join()),
	}

	signer := sig.NewSha256Signer()
	interest, err := sd.engine.Spec().MakeInterest(cmd, &ndn.InterestConfig{
		MustBeFresh: true,
		Nonce:       utils.ConvertNonce(sd.engine.Timer().Nonce()),
	}, enc.Wire{}, signer)
	if err != nil {
		log.Warn(nil, "Failed to encode readvertise Interest", "err", err)
		return
	}

	// Dispatch directly to the local handler, bypassing the forwarder.
	// Going through the forwarder would fail because the Interest would
	// arrive on the app face and the only nexthop is the same app face,
	// triggering same-face loop prevention.
	eng.DispatchInterest(interest)
}

// WithdrawPrefix sends a readvertise-withdraw Interest to the DV router's
// management handler, causing it to remove the prefix from all DV neighbors.
func (sd *SimDvRouter) WithdrawPrefix(name enc.Name, faceId uint64) {
	eng, ok := sd.engine.(*SimEngine)
	if !ok {
		return
	}

	params := &mgmt.ControlParameters{
		Val: &mgmt.ControlArgs{
			Name:   name,
			FaceId: optional.Some(faceId),
		},
	}

	cmd := enc.Name{
		enc.LOCALHOST,
		enc.NewGenericComponent("dv"),
		enc.NewGenericComponent("prefix"),
		enc.NewGenericComponent("withdraw"),
		enc.NewGenericBytesComponent(params.Encode().Join()),
	}

	signer := sig.NewSha256Signer()
	interest, err := sd.engine.Spec().MakeInterest(cmd, &ndn.InterestConfig{
		MustBeFresh: true,
		Nonce:       utils.ConvertNonce(sd.engine.Timer().Nonce()),
	}, enc.Wire{}, signer)
	if err != nil {
		log.Warn(nil, "Failed to encode readvertise-withdraw Interest", "err", err)
		return
	}

	eng.DispatchInterest(interest)
}
