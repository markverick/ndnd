/* Broadcast Strategy for ndnd
 *
 * Implements multicast broadcast forwarding as a proper NDN forwarding strategy.
 */

package fw

import (
	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/table"
)

// BroadcastStrategy implements broadcast multicast forwarding.
type BroadcastStrategy struct {
	StrategyBase
}

func init() {
	strategyInit = append(strategyInit, func() Strategy { return &BroadcastStrategy{} })
	StrategyVersions["broadcast"] = []uint64{1}
}

func (s *BroadcastStrategy) Instantiate(fwThread *Thread) {
	s.NewStrategyBase(fwThread, "broadcast", 1)
}

func (s *BroadcastStrategy) AfterContentStoreHit(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
) {
	core.Log.Trace(s, "AfterContentStoreHit", "name", packet.Name, "faceid", inFace)
	s.SendData(packet, pitEntry, inFace, 0)
}

func (s *BroadcastStrategy) AfterReceiveData(
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

func (s *BroadcastStrategy) AfterReceiveInterest(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	nexthops []StrategyCandidateHop,
) {
	core.Log.Error(s, "BroadcastStrategy does not support AfterReceiveInterest (unicast)",
		"name", packet.Name,
		"inFace", inFace,
		"nexthops", len(nexthops),
	)
}

func (s *BroadcastStrategy) AfterReceiveMulticastInterest(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	petEntry table.PetEntry,
) {
	// Deliver to local faces if there exist such legal local faces
	if len(petEntry.NextHops) > 0 {
		for i := range petEntry.NextHops {
			nexthop := petEntry.NextHops[i]
			if nexthop.FaceID == inFace {
				continue
			}
			if pitEntry.InRecords()[nexthop.FaceID] != nil {
				continue
			}
			packet.EgressRouter = nil
			s.SendInterest(packet, pitEntry, nexthop.FaceID, inFace)
		}
	}

	if len(petEntry.EgressRouters) == 0 {
		core.Log.Trace(s, "Broadcast without PET egress routers; drop", "name", packet.Name)
		return
	}
	for _, er := range petEntry.EgressRouters {
		for _, nextHop := range table.FibStrategyTable.FindNextHopsEnc(er) {
			if pitEntry.InRecords()[nextHop.Nexthop] != nil {
				continue
			}
			s.SendInterest(packet, pitEntry, nextHop.Nexthop, inFace)
		}
	}
}

func (s *BroadcastStrategy) BeforeSatisfyInterest(pitEntry table.PitEntry, inFace uint64) {}
