package sim

import (
	"fmt"
	"time"

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
func NewSimDvRouter(clock Clock, engine ndn.Engine, cfg *config.Config) (*SimDvRouter, error) {
	router, err := dv.NewRouter(cfg, engine)
	if err != nil {
		return nil, fmt.Errorf("failed to create DV router: %w", err)
	}

	// Override GoFunc to use simulation clock scheduling
	// Schedule(0, f) runs f after the current event completes, which is
	// the simulation-safe equivalent of launching a goroutine.
	router.GoFunc = func(f func()) {
		clock.Schedule(0, f)
	}

	// Override NowFunc to use simulation clock
	router.NowFunc = clock.Now

	// Override AfterFunc to use simulation clock scheduling
	router.AfterFunc = func(d time.Duration, f func()) func() {
		id := clock.Schedule(d, f)
		return func() { clock.Cancel(id) }
	}

	// Set nfdc to synchronous mode -- no goroutine, direct ExecMgmtCmd
	router.Nfdc().Synchronous = true
	router.EnableFaceEvents = false

	return &SimDvRouter{
		router:            router,
		clock:             clock,
		engine:            engine,
		heartbeatInterval: cfg.AdvertisementSyncInterval(),
		deadcheckInterval: cfg.RouterDeadInterval(),
		neighborsPrefix: enc.LOCALHOP.
			Append(enc.NewGenericComponent("neighbors")),
	}, nil
}

// Start initializes the DV router and schedules heartbeat/deadcheck events.
func (sd *SimDvRouter) Start() error {
	if err := sd.router.Init(); err != nil {
		return err
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
	sd.heartbeatEvent = sd.clock.Schedule(sd.heartbeatInterval, func() {
		sd.router.RunHeartbeat()
		sd.scheduleHeartbeat()
	})
}

func (sd *SimDvRouter) scheduleDeadcheck() {
	sd.deadcheckEvent = sd.clock.Schedule(sd.deadcheckInterval, func() {
		sd.router.RunDeadcheck()
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
