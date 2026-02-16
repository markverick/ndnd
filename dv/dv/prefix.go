package dv

import (
	"sync"
	"time"

	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/dv/nfdc"
	"github.com/named-data/ndnd/dv/table"
	"github.com/named-data/ndnd/dv/tlv"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	ndn_sync "github.com/named-data/ndnd/std/sync"
)

type PrefixModule struct {
	mu                 sync.Mutex
	pfx                *table.PrefixEgreState
	pfxSvs             *ndn_sync.SvsALO
	nfdc               *nfdc.NfdMgmtThread
	replicatePes       bool
	pfxGroup           enc.Name
	insertPrefix       enc.Name
	pfxSeen            map[uint64]enc.Name
	pfxSubs            map[uint64]enc.Name
	petPrefixes        map[uint64]map[string]enc.Name
	seenPrefixVersions map[string]uint64
	prefixPruneStop    chan struct{}
	routerName         enc.Name
}

type petEgressOp struct {
	add    bool
	name   enc.Name
	egress enc.Name
}

// const PrefixSnapThreshold = 50

func NewPrefixModule(config *config.Config, objectClient ndn.Client, nfdcThread *nfdc.NfdMgmtThread) *PrefixModule {
	var ptable *table.PrefixEgreState

	// Subscription List
	pfxSubs := make(map[uint64]enc.Name)
	pfxSeen := make(map[uint64]enc.Name)
	petPrefixes := make(map[uint64]map[string]enc.Name)
	seenPrefixVersions := make(map[string]uint64)

	// SVS delivery agent for syncing prefix egress state across all NDN routers.
	pfxSvs, err := ndn_sync.NewSvsALO(ndn_sync.SvsAloOpts{
		Name: config.RouterName(),
		Svs: ndn_sync.SvSyncOpts{
			Client:      objectClient,
			GroupPrefix: config.PrefixEgreStatePrefix(),
		},
		Snapshot: &ndn_sync.SnapshotNodeLatest{
			Client: objectClient,
			SnapMe: func(name enc.Name) (enc.Wire, error) {
				return ptable.Snap(), nil
			},
			Threshold: PrefixSnapThreshold,
		},
	})
	if err != nil {
		panic(err)
	}

	// Local prefix egress state.
	ptable = table.NewPrefixEgreState(config, func(w enc.Wire) {
		if _, _, err := pfxSvs.Publish(w); err != nil {
			log.Error(ptable, "Failed to publish prefix egress state update", "err", err)
		}
	})

	pfxModule := &PrefixModule{
		mu:           sync.Mutex{},
		pfx:          ptable,
		pfxSvs:       pfxSvs,
		nfdc:         nfdcThread,
		replicatePes: config.PrefixEgreStateReplicationEnabled(),
		pfxGroup:     config.PrefixEgreStatePrefix().Clone(),
		insertPrefix: enc.LOCALHOP.
			Append(enc.NewGenericComponent("route")).
			Append(enc.NewGenericComponent("insert")),
		pfxSeen:            pfxSeen,
		pfxSubs:            pfxSubs,
		petPrefixes:        petPrefixes,
		seenPrefixVersions: seenPrefixVersions,
		routerName:         config.RouterName(),
	}
	pfxSvs.SetOnPublisher(pfxModule.onPublisher)
	if !pfxModule.replicatePes {
		log.Warn(pfxModule, "Prefix egress state replication to PET is disabled")
	}

	return pfxModule
}

func (pfx *PrefixModule) Start() {
	pfx.pfxSvs.Start()
	pfx.startPrefixPrune()
	pfx.pfx.Reset()
}

func (pfx *PrefixModule) Stop() {
	pfx.stopPrefixPrune()
	pfx.pfxSvs.Stop()
}

func (pfx *PrefixModule) onPublisher(name enc.Name) {
	if name.Equal(pfx.routerName) {
		return
	}

	hash := name.Hash()
	var shouldInstall bool
	var shouldSubscribe bool

	pfx.mu.Lock()
	if _, ok := pfx.pfxSeen[hash]; !ok {
		pfx.pfxSeen[hash] = name.Clone()
		shouldInstall = true
	}
	if _, ok := pfx.pfxSubs[hash]; !ok {
		pfx.pfxSubs[hash] = name.Clone()
		shouldSubscribe = true
	}
	pfx.mu.Unlock()

	if shouldInstall && pfx.replicatePes {
		if pfx.nfdc != nil {
			route := pfx.pfxGroup.Append(name...)
			pfx.nfdc.Exec(nfdc.NfdMgmtCmd{
				Module: "pet",
				Cmd:    "add-egress",
				Args: &mgmt.ControlArgs{
					Name:   route,
					Egress: &mgmt.EgressRecord{Name: route},
				},
				Retries: -1,
			})
		}
	}
	if shouldSubscribe {
		err := pfx.pfxSvs.SubscribePublisher(name, func(sp ndn_sync.SvsPub) {
			pfx.mu.Lock()
			_, petOps := pfx.processUpdate(sp.Content)
			pfx.mu.Unlock()
			pfx.applyPetOps(petOps)
		})
		if err == nil {
			log.Info(pfx.pfx, "Subscribed to prefix updates", "name", name)
			return
		}

		log.Warn(pfx.pfx, "Failed to subscribe to prefix updates", "name", name, "err", err)
		pfx.mu.Lock()
		delete(pfx.pfxSubs, hash)
		pfx.mu.Unlock()
	}
}

// forward APIs from PrefixEgreState
// threadsafe as original caller used a mutex, to maintain existing assumptions

// (AI GENERATED DESCRIPTION): Returns the literal string `"prefix-daemon"` as the string representation of a PrefixModule instance.
func (pfx *PrefixModule) String() string {
	return "prefix-daemon"
}

// (AI GENERATED DESCRIPTION): Retrieves the PrefixEgreStateRouter for a given name, creating a new router with an empty Prefixes map if one does not already exist.
func (pfx *PrefixModule) GetRouter(name enc.Name) *table.PrefixEgreStateRouter {
	pfx.mu.Lock()
	defer pfx.mu.Unlock()

	return pfx.pfx.GetRouter(name)
}

// Reset clears local prefix egress state and publishes a reset update.
func (pfx *PrefixModule) Reset() {
	pfx.mu.Lock()
	petOps := pfx.resetRouterPet(pfx.routerName)
	pfx.pfx.Reset()
	pfx.mu.Unlock()

	pfx.applyPetOps(petOps)
}

// Announce adds or updates a local prefix in prefix egress state.
func (pfx *PrefixModule) Announce(name enc.Name, face uint64, cost uint64) {
	pfx.mu.Lock()
	petOps := pfx.addRouterPrefixPet(pfx.routerName, name)
	pfx.pfx.Announce(name, face, cost)
	pfx.mu.Unlock()

	pfx.applyPetOps(petOps)
}

func (pfx *PrefixModule) AnnounceWithValidity(name enc.Name, face uint64, cost uint64, validity *spec.ValidityPeriod) {
	pfx.mu.Lock()
	petOps := pfx.addRouterPrefixPet(pfx.routerName, name)
	pfx.pfx.AnnounceWithValidity(name, face, cost, validity)
	pfx.mu.Unlock()

	pfx.applyPetOps(petOps)
}

// (AI GENERATED DESCRIPTION): Removes a next‑hop for the specified name and face from the local prefix egress state and republishes the entry if its cost changes.
func (pfx *PrefixModule) Withdraw(name enc.Name, face uint64) {
	pfx.mu.Lock()
	petOps := make([]petEgressOp, 0)
	pfx.pfx.Withdraw(name, face)
	if _, ok := pfx.pfx.GetRouter(pfx.routerName).Prefixes[name.TlvStr()]; !ok {
		petOps = append(petOps, pfx.removeRouterPrefixPet(pfx.routerName, name)...)
	}
	pfx.mu.Unlock()

	pfx.applyPetOps(petOps)
}

// Applies ops from a list. Returns if dirty.
func (pfx *PrefixModule) Apply(wire enc.Wire) (dirty bool) {
	pfx.mu.Lock()
	dirty, petOps := pfx.processUpdate(wire)
	pfx.mu.Unlock()

	pfx.applyPetOps(petOps)
	return dirty
}

// (AI GENERATED DESCRIPTION): Creates a wire‑encoded TLV PrefixOpList that resets the prefix egress state and lists all current prefixes for the local router.
func (pfx *PrefixModule) Snap() enc.Wire {
	pfx.mu.Lock()
	defer pfx.mu.Unlock()

	return pfx.pfx.Snap()
}

// information for svs group, need to expose for now to register with mgmt
func (pfx *PrefixModule) SyncPrefix() enc.Name {
	return pfx.pfxSvs.SyncPrefix()
}

func (pfx *PrefixModule) DataPrefix() enc.Name {
	return pfx.pfxSvs.DataPrefix()
}

func (pfx *PrefixModule) InsertionPrefix() enc.Name {
	return pfx.insertPrefix.Clone()
}

func (pfx *PrefixModule) OnInsertion(args ndn.InterestHandlerArgs) {
	pfx.onInsertion(args)
}

func (pfx *PrefixModule) processUpdate(wire enc.Wire) (dirty bool, petOps []petEgressOp) {
	petOps = make([]petEgressOp, 0)

	ops, err := tlv.ParsePrefixOpList(enc.NewWireView(wire), true)
	if err == nil && ops != nil && ops.EgressRouter != nil && len(ops.EgressRouter.Name) > 0 {
		router := ops.EgressRouter.Name.Clone()
		if ops.PrefixOpReset {
			petOps = append(petOps, pfx.resetRouterPet(router)...)
		}
		for _, add := range ops.PrefixOpAdds {
			petOps = append(petOps, pfx.addRouterPrefixPet(router, add.Name)...)
		}
		for _, remove := range ops.PrefixOpRemoves {
			petOps = append(petOps, pfx.removeRouterPrefixPet(router, remove.Name)...)
		}
	}

	return pfx.pfx.Apply(wire), petOps
}

func (pfx *PrefixModule) resetRouterPet(router enc.Name) []petEgressOp {
	routerHash := router.Hash()
	prefixes, ok := pfx.petPrefixes[routerHash]
	if !ok || len(prefixes) == 0 {
		return nil
	}

	egress := pfx.pfxGroup.Append(router...)
	ops := make([]petEgressOp, 0, len(prefixes))
	for _, name := range prefixes {
		ops = append(ops, petEgressOp{
			add:    false,
			name:   name.Clone(),
			egress: egress.Clone(),
		})
	}
	delete(pfx.petPrefixes, routerHash)
	return ops
}

func (pfx *PrefixModule) addRouterPrefixPet(router enc.Name, prefix enc.Name) []petEgressOp {
	routerHash := router.Hash()
	prefixes := pfx.petPrefixes[routerHash]
	if prefixes == nil {
		prefixes = make(map[string]enc.Name)
		pfx.petPrefixes[routerHash] = prefixes
	}

	key := prefix.TlvStr()
	if _, exists := prefixes[key]; exists {
		return nil
	}
	prefixes[key] = prefix.Clone()

	egress := pfx.pfxGroup.Append(router...)
	return []petEgressOp{{
		add:    true,
		name:   prefix.Clone(),
		egress: egress,
	}}
}

func (pfx *PrefixModule) removeRouterPrefixPet(router enc.Name, prefix enc.Name) []petEgressOp {
	routerHash := router.Hash()
	prefixes, ok := pfx.petPrefixes[routerHash]
	if !ok {
		return nil
	}

	key := prefix.TlvStr()
	if _, exists := prefixes[key]; !exists {
		return nil
	}
	delete(prefixes, key)
	if len(prefixes) == 0 {
		delete(pfx.petPrefixes, routerHash)
	}

	egress := pfx.pfxGroup.Append(router...)
	return []petEgressOp{{
		add:    false,
		name:   prefix.Clone(),
		egress: egress,
	}}
}

func (pfx *PrefixModule) applyPetOps(ops []petEgressOp) {
	if !pfx.replicatePes || pfx.nfdc == nil || len(ops) == 0 {
		return
	}

	for _, op := range ops {
		cmd := "remove-egress"
		if op.add {
			cmd = "add-egress"
		}

		pfx.nfdc.Exec(nfdc.NfdMgmtCmd{
			Module: "pet",
			Cmd:    cmd,
			Args: &mgmt.ControlArgs{
				Name:   op.name,
				Egress: &mgmt.EgressRecord{Name: op.egress},
			},
			Retries: -1,
		})
	}
}

func (pfx *PrefixModule) startPrefixPrune() {
	pfx.mu.Lock()
	if pfx.prefixPruneStop != nil {
		pfx.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	pfx.prefixPruneStop = stop
	pfx.mu.Unlock()

	go func(stopCh <-chan struct{}) {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pfx.pruneExpired()
			case <-stopCh:
				return
			}
		}
	}(stop)
}

func (pfx *PrefixModule) stopPrefixPrune() {
	pfx.mu.Lock()
	stop := pfx.prefixPruneStop
	pfx.prefixPruneStop = nil
	pfx.mu.Unlock()

	if stop != nil {
		close(stop)
	}
}

func (pfx *PrefixModule) pruneExpired() {
	now := time.Now().UTC()

	pfx.mu.Lock()
	expired, _ := pfx.pfx.PruneExpired(now)
	petOps := make([]petEgressOp, 0, len(expired))
	for _, item := range expired {
		petOps = append(petOps, pfx.removeRouterPrefixPet(item.Router, item.Name)...)
	}
	pfx.mu.Unlock()

	pfx.applyPetOps(petOps)
}
