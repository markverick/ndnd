/**
 * Replicast forwarding strategy
 *
 * Using this strategy, there will be exactly one request sent per unique ER.
 *
 * This will eventually be replaced by BIER multicast
 */

package fw

import (
	"sort"

	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/table"
)

// The strategy to forward interests to all nexthops.
type Replicast struct {
	StrategyBase
}

// (AI GENERATED DESCRIPTION): Registers the Replicast strategy with version 1 in the strategy registry by appending its constructor to the init list.
func init() {
	strategyInit = append(strategyInit, func() Strategy { return &Replicast{} })
	StrategyVersions["replicast"] = []uint64{1}
}

// (AI GENERATED DESCRIPTION): Initializes the *Replicast strategy by setting up its base with the name “replicast” and priority 1 on the provided forwarding thread.
func (s *Replicast) Instantiate(fwThread *Thread) {
	s.NewStrategyBase(fwThread, "replicast", 1)
}

// (AI GENERATED DESCRIPTION): Sends a cached Data packet (retrieved from the Content Store) back to the requester via the specified PIT entry, using the requesting face and indicating the Content Store as the data source.
func (s *Replicast) AfterContentStoreHit(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
) {
	core.Log.Trace(s, "AfterContentStoreHit", "name", packet.Name, "faceid", inFace)
	s.SendData(packet, pitEntry, inFace, 0) // 0 indicates ContentStore is source
}

// (AI GENERATED DESCRIPTION): Forwards a received Data packet to every face recorded in the PIT entry, logging each forwarding step and invoking SendData for each destination.
func (s *Replicast) AfterReceiveData(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
) {
	core.Log.Trace(s, "AfterReceiveData", "name", packet.Name, "inrecords", len(pitEntry.InRecords()))
	for faceID := range pitEntry.InRecords() {
		core.Log.Trace(s, "Forwarding Data", "name", packet.Name, "faceid", faceID)
		s.SendData(packet, pitEntry, faceID, inFace)
	}
}

// Forwards n interest packets to n unique egress routers
func (s *Replicast) AfterReceiveInterest(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	nexthops []StrategyCandidateHop,
) {
	if len(nexthops) == 0 {
		core.Log.Debug(s, "No nexthop found - DROP", "name", packet.Name)
		return
	}

	// interest with ER tag should only be unicast as dest router is known
	if len(packet.EgressRouter) > 0 {
		core.Log.Debug(s, "Interest with ER tag passed to replicast - DROP", "name", packet.Name)
		return
	}

	// sort nexthops by cost and send to best-possible nexthop for each unique ER
	sort.Slice(nexthops, func(i, j int) bool {
		return nexthops[i].HopEntry.Cost < nexthops[j].HopEntry.Cost
	})

	sentER := make(map[uint64]bool)

	for _, nh := range nexthops {
		if sentER[nh.EgressRouter.Hash()] {
			core.Log.Trace(s, "Avoiding duplicate interest", "name", packet.Name, "sentER", sentER, "faceid", nh.HopEntry.Nexthop, "er", nh.EgressRouter, "inFace", inFace)
			continue
		}

		outgoingPkt := *packet
		outgoingPkt.EgressRouter = nh.EgressRouter
		if sent := s.SendInterest(&outgoingPkt, pitEntry, nh.HopEntry.Nexthop, inFace); sent {
			core.Log.Trace(s, "Forwarded Interest", "name", outgoingPkt.Name, "faceid", nh.HopEntry.Nexthop, "er", nh.EgressRouter, "inFace", inFace)
			sentER[nh.EgressRouter.Hash()] = true
		}
	}

	for _, nh := range nexthops {
		er := nh.EgressRouter
		if !sentER[er.Hash()] {
			core.Log.Debug(s, "No usable replicast nexthop for Interest specific ER tag - DROP", "name", packet.Name, "ER", er)
		}
	}
}

func (s *Replicast) AfterReceiveMulticastInterest(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	petEntry table.PetEntry,
) {
	core.Log.Error(s, "Replicast does not support AfterReceiveMulticastInterest",
		"name", packet.Name,
		"inFace", inFace,
		"petNextHops", len(petEntry.NextHops),
		"petEgress", len(petEntry.EgressRouters),
	)
}

// (AI GENERATED DESCRIPTION): No‑op hook invoked before satisfying an Interest in the Replicast strategy – it performs no action.
func (s *Replicast) BeforeSatisfyInterest(pitEntry table.PitEntry, inFace uint64) {
	// This does nothing in Replicast
}
