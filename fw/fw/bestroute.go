/* YaNFD - Yet another NDN Forwarding Daemon
 *
 * Copyright (C) 2020-2021 Eric Newberry.
 *
 * This file is licensed under the terms of the MIT License, as found in LICENSE.md.
 */

package fw

import (
	"sort"
	"time"

	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
)

// BestRouteSuppressionTime is the time to suppress retransmissions of the same Interest.
const BestRouteSuppressionTime = 400 * time.Millisecond

// BestRoute is a forwarding strategy that forwards Interests
// to the nexthop with the lowest cost.
type BestRoute struct {
	StrategyBase
}

// (AI GENERATED DESCRIPTION): Registers the BestRoute strategy with version 1 in the strategy registry by appending its constructor to the init list.
func init() {
	strategyInit = append(strategyInit, func() Strategy { return &BestRoute{} })
	StrategyVersions["best-route"] = []uint64{1}
}

// (AI GENERATED DESCRIPTION): Initializes the *BestRoute strategy by setting up its base with the name “best‑route” and priority 1 on the provided forwarding thread.
func (s *BestRoute) Instantiate(fwThread *Thread) {
	s.NewStrategyBase(fwThread, "best-route", 1)
}

// (AI GENERATED DESCRIPTION): Sends a cached Data packet (retrieved from the Content Store) back to the requester via the specified PIT entry, using the requesting face and indicating the Content Store as the data source.
func (s *BestRoute) AfterContentStoreHit(
	packet *defn.Pkt,
	pitEntry table.PitEntry,
	inFace uint64,
) {
	core.Log.Trace(s, "AfterContentStoreHit", "name", packet.Name, "faceid", inFace)
	s.SendData(packet, pitEntry, inFace, 0) // 0 indicates ContentStore is source
}

// (AI GENERATED DESCRIPTION): Forwards a received Data packet to every face recorded in the PIT entry, logging each forwarding step and invoking SendData for each destination.
func (s *BestRoute) AfterReceiveData(
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

// (AI GENERATED DESCRIPTION): Forwards an incoming Interest to the lowest‑cost next‑hop, suppressing retransmissions within a set time window, and drops the Interest if no usable nexthop is found.
func (s *BestRoute) AfterReceiveInterest(
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

	now := time.Now()
	for pass := range 2 {
		for _, ernh := range erHops {
			er := ernh.Er
			nh := ernh.Nh
			// In the first pass, skip hops that already have a out record
			if pass == 0 {
				if oR := pitEntry.OutRecords()[nh.Nexthop]; oR != nil {
					// Suppress retransmissions of the same Interest within suppression time
					if oR.LatestTimestamp.Add(BestRouteSuppressionTime).After(now) {
						core.Log.Debug(s, "Suppressed Interest - DROP", "name", packet.Name)
						return
					}

					// If an out record exists, skip this hop
					continue
				}
			}

			// For the second pass, we should ideally use the least recently tried hop.
			// But then we need to resort the list - this is just faster for now.
			// In densely connected networks, this is not a big deal.

			core.Log.Trace(s, "Forwarding Interest", "name", packet.Name, "faceid", nh.Nexthop)

			// if there is an associated EgressRouter tag with this new route, then set packet.EgressRouter to the tag
			if er != nil {
				oldEgress := packet.EgressRouter
				packet.EgressRouter = er
				if sent := s.SendInterest(packet, pitEntry, nh.Nexthop, inFace); sent {
					return
				}
				packet.EgressRouter = oldEgress
				// otherwise, normal forwarding
			} else {
				if sent := s.SendInterest(packet, pitEntry, nh.Nexthop, inFace); sent {
					return
				}
			}
		}
	}

	core.Log.Debug(s, "No usable nexthop for Interest - DROP", "name", packet.Name)
}

// (AI GENERATED DESCRIPTION): No‑op; the BestRoute strategy performs no action before satisfying an Interest.
func (s *BestRoute) BeforeSatisfyInterest(pitEntry table.PitEntry, inFace uint64) {
	// This does nothing in BestRoute
}
