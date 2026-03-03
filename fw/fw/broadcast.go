/**
 * Broadcast forwarding strategy.
 *
 * Using this strategy, the interest packet will be forwarded to all nexthops.
 */

package fw

import (
	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
)

// The strategy to forward interests to all nexthops.
type Broadcast struct {
	StrategyBase
}

// (AI GENERATED DESCRIPTION): Registers the Broadcast strategy with version 1 in the strategy registry by appending its constructor to the init list.
func init() {
	strategyInit = append(strategyInit, func() Strategy { return &Broadcast{} })
	StrategyVersions["broadcast"] = []uint64{1}
}

// (AI GENERATED DESCRIPTION): Initializes the *Broadcast strategy by setting up its base with the name “broadcast” and priority 1 on the provided forwarding thread.
func (s *Broadcast) Instantiate(fwThread *Thread) {
	s.NewStrategyBase(fwThread, "broadcast", 1)
}

// (AI GENERATED DESCRIPTION): Sends a cached Data packet (retrieved from the Content Store) back to the requester via the specified PIT entry, using the requesting face and indicating the Content Store as the data source.
func (s *Broadcast) AfterContentStoreHit(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
) {
	core.Log.Trace(s, "AfterContentStoreHit", "name", packet.Name, "faceid", inFace)
	s.SendData(packet, pitEntry, inFace, 0) // 0 indicates ContentStore is source
}

// (AI GENERATED DESCRIPTION): Forwards a received Data packet to every face recorded in the PIT entry, logging each forwarding step and invoking SendData for each destination.
func (s *Broadcast) AfterReceiveData(
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

// Forwards an incoming Interest to all next-hops
func (s *Broadcast) AfterReceiveInterest(
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

	successfulForward := false

	for _, nh := range nexthops {
		if sent := s.SendInterest(packet, pitEntry, nh.Nexthop, inFace); sent {
			core.Log.Trace(s, "Forwarded Interest", "name", packet.Name, "faceid", nh.Nexthop)
			successfulForward = true
		} else {
			core.Log.Trace(s, "Error forwarding interest", "name", packet.Name, "faceid", nh.Nexthop)
		}
	}

	if !successfulForward {
		core.Log.Debug(s, "No usable nexthop for Interest - DROP", "name", packet.Name)
	}
}

// (AI GENERATED DESCRIPTION): No‑op hook invoked before satisfying an Interest in the Broadcast strategy – it performs no action.
func (s *Broadcast) BeforeSatisfyInterest(pitEntry table.PitEntry, inFace uint64) {
	// This does nothing in Broadcast
}
