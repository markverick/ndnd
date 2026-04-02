package dv

import (
	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/dv/table"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/sync"
)

// postUpdateRib should be called after the RIB has been updated.
// It triggers a corresponding fib update and advert generation.
// Run it in a separate goroutine to avoid deadlocks.
func (dv *Router) postUpdateRib() {
	dv.updateFib()
	dv.advert.generate()
	dv.updatePrefixSubs()
}

// updateRib computes the RIB chnages for this neighbor
func (dv *Router) updateRib(ns *table.NeighborState) {
	dv.mutex.Lock()
	defer dv.mutex.Unlock()

	if ns.Advert == nil {
		return
	}

	// TODO: use cost to neighbor
	localCost := uint64(1)

	// Trigger our own advertisement if needed
	var dirty bool = false

	// Reset destinations for this neighbor
	dv.rib.DirtyResetNextHop(ns.Name)

	for _, entry := range ns.Advert.Entries {
		// Use the advertised cost by default
		cost := entry.Cost + localCost

		// Poison reverse - try other cost if next hop is us
		if entry.NextHop.Name.Equal(dv.config.RouterName()) {
			if entry.OtherCost < config.CostInfinity {
				cost = entry.OtherCost + localCost
			} else {
				cost = config.CostInfinity
			}
		}

		// Skip unreachable destinations
		if cost >= config.CostInfinity {
			continue
		}

		// Check advertisement changes
		dirty = dv.rib.Set(entry.Destination.Name, ns.Name, cost) || dirty
	}

	// Drop dead entries
	dirty = dv.rib.Prune() || dirty

	// If advert changed, increment sequence number
	if dirty {
		dv.GoFunc(dv.postUpdateRib)
	}
}

// Check for dead neighbors
func (dv *Router) checkDeadNeighbors() {
	dv.mutex.Lock()
	defer dv.mutex.Unlock()

	dirty := false
	for _, ns := range dv.neighbors.GetAll() {
		// Check if the neighbor is entirely dead
		if ns.IsDead() {
			log.Info(dv, "Neighbor is dead", "router", ns.Name,
				"age_ms", dv.NowFunc().Sub(ns.LastSeen()).Milliseconds())

			// This is the ONLY place that can remove neighbors
			dv.neighbors.Remove(ns.Name)

			// Remove neighbor from RIB and prune
			dirty = dv.rib.RemoveNextHop(ns.Name) || dirty
			dirty = dv.rib.Prune() || dirty
		}
	}

	if dirty {
		dv.GoFunc(dv.postUpdateRib)
	}
}

// updateFib synchronizes the FIB with the RIB.
func (dv *Router) updateFib() {
	log.Debug(dv, "Sychronizing updates to forwarding table")

	dv.mutex.Lock()
	defer dv.mutex.Unlock()

	// Name prefixes from global prefix table as well as RIB
	names := make(map[uint64]enc.Name)
	fibEntries := make(map[uint64][]table.FibEntry)

	// Helper to add fib entries
	register := func(name enc.Name, fes []table.FibEntry, cost uint64) {
		nameH := name.Hash()
		names[nameH] = name

		// Append to existing entries with new cost
		for _, fe := range fes { // fe byval
			fe.Cost += cost
			fibEntries[nameH] = append(fibEntries[nameH], fe)
		}
	}

	// Update paths to all routers from RIB
	for hash, router := range dv.rib.Entries() {
		// Skip if this is us
		if router.Name().Equal(dv.config.RouterName()) {
			continue
		}

		// Get FIB entry to reach this router
		fes := dv.rib.GetFibEntries(dv.neighbors, hash)

		if dv.config.OneStep {
			// One-step mode: RIB contains both router entries and prefix
			// entries.  Router entries have names starting with the network
			// prefix (e.g. /minindn/nodeN); everything else is an
			// application prefix that should be installed directly in the
			// FIB without adding PrefixSync or advert-data routes.
			if dv.config.NetworkName().IsPrefix(router.Name()) {
						// This is a router entry -- install DV advert data route
				advRoute := enc.LOCALHOP.
					Append(router.Name()...).
					Append(enc.NewKeywordComponent("DV")).
					Append(enc.NewKeywordComponent("ADV"))
				register(advRoute, fes, 0)
			} else {
						// This is a prefix entry -- install FIB route directly
				register(router.Name(), fes, 0)
			}
		} else {
			// Two-step mode (default): all RIB entries are routers.
			// Add entry for the router's prefix sync group prefix
			proute := dv.config.PrefixTableGroupPrefix().
				Append(router.Name()...)
			register(proute, fes, 0)

			// Add entry for the router's DV advertisement data namespace.
			advRoute := enc.LOCALHOP.
				Append(router.Name()...).
				Append(enc.NewKeywordComponent("DV")).
				Append(enc.NewKeywordComponent("ADV"))
			register(advRoute, fes, 0)

			// Add entries to all prefixes announced by this router
			for _, prefix := range dv.pfx.GetRouter(router.Name()).Prefixes {
				register(prefix.Name, fes, prefix.Cost)
			}
		}
	}

	// Update all FIB entries to NFD
	dv.fib.UnmarkAll()
	for nameH, fes := range fibEntries {
		if dv.fib.UpdateH(nameH, names[nameH], fes) {
			dv.fib.MarkH(nameH)
		}
	}
	dv.fib.RemoveUnmarked()
}

// updatePrefixSubs updates the prefix table subscriptions
func (dv *Router) updatePrefixSubs() {
	dv.mutex.Lock()
	defer dv.mutex.Unlock()

	// Get all prefixes from the RIB
	for hash, router := range dv.rib.Entries() {
		if router.Name().Equal(dv.config.RouterName()) {
			continue
		}

		// In one-step mode the RIB contains both router and prefix entries.
		// Only track router entries for reachability / prefix subscriptions.
		if dv.config.OneStep && !dv.config.NetworkName().IsPrefix(router.Name()) {
			continue
		}

		if _, ok := dv.pfxSubs[hash]; !ok {
			log.Info(dv, "Router is now reachable", "name", router.Name())
			dv.pfxSubs[hash] = router.Name()

			table.NotifyRouterReachable(table.RouterReachableEvent{
				At:              dv.NowFunc(),
				NodeRouter:      dv.config.RouterName(),
				ReachableRouter: router.Name(),
			})

			// In one-step mode there is no PrefixSync SVS to subscribe to.
			if !dv.config.OneStep {
				dv.pfxSvs.SubscribePublisher(router.Name(), func(sp sync.SvsPub) {
					dv.mutex.Lock()
					defer dv.mutex.Unlock()

					// Both snapshots and normal data are handled the same way
					if dirty := dv.pfx.Apply(sp.Content); dirty {
						// Update the local fib if prefix table changed
						dv.GoFunc(dv.updateFib) // expensive
					}
				})
			}
		}
	}

	// Remove dead subscriptions
	for hash, name := range dv.pfxSubs {
		if !dv.rib.Has(name) {
			log.Info(dv, "Router is now unreachable", "name", name)
			if !dv.config.OneStep {
				dv.pfxSvs.UnsubscribePublisher(name)
			}
			delete(dv.pfxSubs, hash)
		}
	}
}
