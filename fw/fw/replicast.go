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
	enc "github.com/named-data/ndnd/std/encoding"
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
	nexthops []*table.FibNextHopEntry,
	nextER []enc.Name,
) {
	if len(nexthops) == 0 {
		core.Log.Debug(s, "No nexthop found - DROP", "name", packet.Name)
		return
	}

	// if we are unable to map 1:1 next hop to intended ER, fallback to best-route strategy
	bestRouteFallback := false

	if len(nexthops) != len(nextER) {
		core.Log.Debug(
			s,
			"Warning - no 1:1 association between next hop and intended ER",
			"name", packet.Name,
			"len(nexthops)", len(nexthops),
			"len(nextER)", len(nextER),
		)
		bestRouteFallback = true
	}

	if len(nextER) == 0 {
		core.Log.Debug(s, "Warning - no ER tag set in forwarding")
		bestRouteFallback = true
	}

	if bestRouteFallback {
		core.Log.Debug(s, "Falling back to best-route forwarding", "name", packet.Name)
		for _, nh := range nexthops {
			if sent := s.SendInterest(packet, pitEntry, nh.Nexthop, inFace); sent {
				return
			}
		}
		core.Log.Debug(s, "No usable nexthop for Interest - DROP", "name", packet.Name)
		return
	}

	erHops := make([]struct {
		Er enc.Name
		Nh *table.FibNextHopEntry
	}, len(nextER))
	for i := range nexthops {
		erHops[i].Er = nextER[i]
		erHops[i].Nh = nexthops[i]
	}
	// sort nexthops / nextER by cost and send to best-possible nexthop for each unique ER
	sort.Slice(erHops, func(i, j int) bool { return erHops[i].Nh.Cost < erHops[i].Nh.Cost })

	sentER := make(map[uint64]bool)

	for _, ernh := range erHops {
		nh := ernh.Nh
		er := ernh.Er
		if sentER[er.Hash()] {
			core.Log.Trace(s, "Avoiding duplicate interest", "name", packet.Name, "sentER", sentER, "faceid", nh, "er", er, "inFace", inFace)
			continue
		}

		oldEgress := packet.EgressRouter
		packet.EgressRouter = er
		if sent := s.SendInterest(packet, pitEntry, nh.Nexthop, inFace); sent {
			core.Log.Trace(s, "Forwarded Interest", "name", packet.Name, "faceid", nh.Nexthop, "er", er, "inFace", inFace)
			sentER[er.Hash()] = true
		}
		packet.EgressRouter = oldEgress
	}

	for _, er := range nextER {
		if !sentER[er.Hash()] {
			core.Log.Debug(s, "No usable replicast nexthop for Interest specific ER tag - DROP", "name", packet.Name, "ER", er)
		}
	}
}

// (AI GENERATED DESCRIPTION): No‑op hook invoked before satisfying an Interest in the Replicast strategy – it performs no action.
func (s *Replicast) BeforeSatisfyInterest(pitEntry table.PitEntry, inFace uint64) {
	// This does nothing in Replicast
}
