/* BIER Strategy for ndnd
 *
 * Implements BIER (Bit Index Explicit Replication) multicast forwarding
 * as a proper NDN forwarding strategy, working in tandem with the PIT.
 *
 * Roles:
 *   BFIR: thread.go detects multiple egress routers, pre-encodes bit-string,
 *         then calls this strategy. Strategy replicates to BIFT neighbors via
 *         SendInterest (creating PIT out-records for tracked Data return).
 *
 *   BFR:  Transit router receives Interest with BIER header. Goes through full
 *         PIT pipeline (no bypass). Strategy replicates to neighbors via BIFT
 *         F-BM AND operations, using SendInterest for PIT tracking.
 *
 *   BFER: Local bit is set. thread.go delivers to local app via allowedLocalNexthops
 *         (PET-based). Strategy clears local bit then replicates remaining bits.
 */

package fw

import (
	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
	ndnlog "github.com/named-data/ndnd/std/log"
)

// BierStrategyName is the canonical name of the BIER strategy.
// Exported so thread.go can use it for auto-override when packet.Bier is set.
var BierStrategyName enc.Name

// BierStrategy implements BIER multicast forwarding via the NDN strategy interface.
// It works in tandem with the PIT: replication uses SendInterest (PIT out-records)
// rather than raw face sends, enabling Data return path tracking and loop suppression.
type BierStrategy struct {
	StrategyBase
}

func init() {
	strategyInit = append(strategyInit, func() Strategy { return &BierStrategy{} })
	StrategyVersions["bier"] = []uint64{1}
}

func (s *BierStrategy) Instantiate(fwThread *Thread) {
	s.NewStrategyBase(fwThread, "bier", 1)
	BierStrategyName = s.GetName()
}

func (s *BierStrategy) AfterContentStoreHit(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
) {
	core.Log.Trace(s, "AfterContentStoreHit", "name", packet.Name, "faceid", inFace)
	s.SendData(packet, pitEntry, inFace, 0)
}

func (s *BierStrategy) AfterReceiveData(
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

func (s *BierStrategy) AfterReceiveInterest(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	nexthops []StrategyCandidateHop,
) {
	core.Log.Error(s, "BierStrategy does not support AfterReceiveInterest (unicast)",
		"name", packet.Name,
		"inFace", inFace,
		"nexthops", len(nexthops),
	)
}

func (s *BierStrategy) AfterReceiveMulticastInterest(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	petEntry table.PetEntry,
	deliveredToLocal bool,
) {
	if len(packet.Bier) == 0 {
		core.Log.Trace(s, "BFIR: encoding BIER bit-string", "name", packet.Name, "egress-count", len(petEntry.EgressRouters))
		packet.Bier = Bift.BuildBierBitString(petEntry.EgressRouters)
	}

	if deliveredToLocal && len(packet.Bier) > 0 && IsBierEnabled() {
		bs := BierClone(packet.Bier)
		BierClearBit(bs, CfgBierIndex())
		packet.Bier = bs
		if BierIsZero(bs) {
			return
		}
	}

	s.replicateBier(packet, pitEntry, inFace)
}

// bierReplicate performs BIFT-based BIER replication through the PIT.
// Local bit is cleared first (local delivery already done by thread.go's
// allowedLocalNexthops block). Remaining bits are replicated to BIFT
// neighbors using sendInterest, which creates PIT out-records.
// Shared by BierStrategy and Multicast so both strategies are BIER-aware.
func bierReplicate(
	logCtx ndnlog.Tag,
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
	sendInterest func(*defn.Pkt, table.PitEntry, uint64, uint64) bool,
) {
	incomingBs := BierClone(packet.Bier)

	// Clear local bit — local delivery was already handled by thread.go.
	// Also clear it if no local app is registered, to avoid forwarding our
	// own bit position to downstream neighbors.
	if IsBierEnabled() {
		localId := CfgBierIndex()
		if localId >= 0 && BierGetBit(incomingBs, localId) {
			BierClearBit(incomingBs, localId)
		}
	}

	if BierIsZero(incomingBs) {
		return
	}

	core.Log.Trace(logCtx, "BIER replication", "name", packet.Name, "bs-len", len(incomingBs))

	neighbors := Bift.GetNeighborEntries()
	for _, neighbor := range neighbors {
		if neighbor.FaceID == inFace {
			continue // Never send back on incoming face
		}

		replicationMask := BierAnd(incomingBs, neighbor.Fbm)
		if BierIsZero(replicationMask) {
			continue
		}

		// Clone packet with per-neighbor replication mask
		clonePkt := &defn.Pkt{
			Name:           packet.Name,
			L3:             packet.L3,
			Raw:            packet.Raw,
			IncomingFaceID: packet.IncomingFaceID,
			CongestionMark: packet.CongestionMark,
			Bier:           replicationMask,
		}

		core.Log.Trace(logCtx, "BIER: replicating to neighbor", "name", packet.Name, "faceid", neighbor.FaceID)

		// KEY: SendInterest creates PIT out-record — Data return path is tracked
		sendInterest(clonePkt, pitEntry, neighbor.FaceID, inFace)

		// Loop suppression: clear forwarded bits from working mask
		incomingBs = BierAndNot(incomingBs, neighbor.Fbm)
		if BierIsZero(incomingBs) {
			break
		}
	}
}

func (s *BierStrategy) replicateBier(packet *defn.Pkt, pitEntry table.PitEntry, inFace uint64) {
	bierReplicate(s, packet, pitEntry, inFace, s.SendInterest)
}

func (s *BierStrategy) BeforeSatisfyInterest(pitEntry table.PitEntry, inFace uint64) {}
