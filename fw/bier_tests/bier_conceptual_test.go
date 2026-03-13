package bier_tests

// Tests for BIER forwarding in NDN: Sync Interest multicast delivery,
// prefix-to-routing mapping, BFIR encoding, and BFR/BFER per-hop handling.

import (
	"fmt"
	"testing"

	fw "github.com/named-data/ndnd/fw/fw"
	enc "github.com/named-data/ndnd/std/encoding"
)

func TestSyncInterestMulticastDelivery(t *testing.T) {
	// Simulate a Sync group with 8 members spread across a star topology.
	// Hub (router 0) is the BFIR; leaves 1-8 are group members (BFERs).
	// A Sync Interest injected at the hub should reach every group member.
	g := buildStar(8)

	t.Run("Sync Interest reaches all group members", func(t *testing.T) {
		// All leaves are Sync group members
		groupMembers := []int{1, 2, 3, 4, 5, 6, 7, 8}
		bs := g.buildBitstring(groupMembers...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, groupMembers...)
	})

	t.Run("Sync Interest reaches subset of group members", func(t *testing.T) {
		// Only some group members need this Sync Interest
		subset := []int{2, 5, 7}
		bs := g.buildBitstring(subset...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, subset...)

		// Non-members must NOT receive
		for _, nonMember := range []int{1, 3, 4, 6, 8} {
			if res.delivered[nonMember] {
				t.Errorf("non-member router %d should not receive Sync Interest", nonMember)
			}
		}
	})

	t.Run("Sync Interest from any member reaches all others", func(t *testing.T) {
		// Member at leaf 3 sends Sync Interest to all other members
		allOthers := []int{1, 2, 4, 5, 6, 7, 8}
		bs := g.buildBitstring(allOthers...)
		res := g.simulate(3, bs)
		assertDeliveredExactly(t, res, allOthers...)
	})
}

func TestSyncInterestStatelessForwarding(t *testing.T) {
	// Verify that BIER forwarding is truly stateless: each transit router
	// forwards based solely on the bit-string and BIFT, without creating
	// any per-flow state.
	//
	// In a linear chain 0-1-2-3-4, router 0 sends a Sync Interest
	// to routers 2 and 4. Transit router 1 and 3 must forward the
	// Interest without needing to "remember" it.
	g := buildLinear(5)

	t.Run("Transit routers forward without local delivery", func(t *testing.T) {
		bs := g.buildBitstring(2, 4)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 2, 4)

		// Routers 1 and 3 are transit (BFR) — they must NOT get local delivery
		if res.delivered[1] {
			t.Error("transit router 1 should not get local delivery")
		}
		if res.delivered[3] {
			t.Error("transit router 3 should not get local delivery")
		}
	})

	t.Run("Same Sync Interest sent twice produces identical results", func(t *testing.T) {
		bs := g.buildBitstring(1, 3, 4)
		res1 := g.simulate(0, bs)
		res2 := g.simulate(0, fw.BierClone(bs))

		// Both runs must deliver to exactly the same set
		for id := range res1.delivered {
			if !res2.delivered[id] {
				t.Errorf("second run missing delivery to router %d", id)
			}
		}
		for id := range res2.delivered {
			if !res1.delivered[id] {
				t.Errorf("first run missing delivery to router %d", id)
			}
		}
	})
}

func TestPrefixToRoutingMapping(t *testing.T) {
	// Simulate the mapping protocol: each Sync group member announces the
	// group prefix to its attached router. The result is a PET egress router
	// list that BuildBierBitString transforms into a BIER bit-string.

	t.Run("Group members map to correct bit positions", func(t *testing.T) {
		bift := &fw.BiftState{}

		// Mapping protocol populates router-to-BFR-ID assignments
		// (In production this comes from DV advertisements)
		routerA := enc.Name{enc.NewGenericComponent("ndn"), enc.NewGenericComponent("router-a")}
		routerB := enc.Name{enc.NewGenericComponent("ndn"), enc.NewGenericComponent("router-b")}
		routerC := enc.Name{enc.NewGenericComponent("ndn"), enc.NewGenericComponent("router-c")}
		routerD := enc.Name{enc.NewGenericComponent("ndn"), enc.NewGenericComponent("router-d")}

		bift.RegisterRouter(routerA, 0)
		bift.RegisterRouter(routerB, 1)
		bift.RegisterRouter(routerC, 2)
		bift.RegisterRouter(routerD, 3)

		// PET egress router list for the Sync group prefix
		// (In production this is populated by the mapping protocol)
		egressRouters := []enc.Name{routerA, routerC, routerD}

		bs := bift.BuildBierBitString(egressRouters)

		// Verify: bits 0, 2, 3 set (routerA, routerC, routerD)
		if !fw.BierGetBit(bs, 0) {
			t.Error("routerA (bit 0) should be set")
		}
		if fw.BierGetBit(bs, 1) {
			t.Error("routerB (bit 1) should NOT be set — not in egress list")
		}
		if !fw.BierGetBit(bs, 2) {
			t.Error("routerC (bit 2) should be set")
		}
		if !fw.BierGetBit(bs, 3) {
			t.Error("routerD (bit 3) should be set")
		}
	})

	t.Run("Unknown routers in egress list are safely ignored", func(t *testing.T) {
		bift := &fw.BiftState{}

		known := enc.Name{enc.NewGenericComponent("known-router")}
		unknown := enc.Name{enc.NewGenericComponent("unknown-router")}

		bift.RegisterRouter(known, 5)

		// PET has both a known and unknown egress router
		egressRouters := []enc.Name{known, unknown}
		bs := bift.BuildBierBitString(egressRouters)

		// Only the known router should appear in the bit-string
		if !fw.BierGetBit(bs, 5) {
			t.Error("known router (bit 5) should be set")
		}
		// Bit-string should be minimal — no stray bits
		for i := 0; i < 5; i++ {
			if fw.BierGetBit(bs, i) {
				t.Errorf("bit %d should not be set", i)
			}
		}
	})

	t.Run("Single egress router produces single-bit string", func(t *testing.T) {
		bift := &fw.BiftState{}
		r := enc.Name{enc.NewGenericComponent("solo")}
		bift.RegisterRouter(r, 7)

		bs := bift.BuildBierBitString([]enc.Name{r})
		if !fw.BierGetBit(bs, 7) {
			t.Error("single-router bit-string should have bit 7 set")
		}
		// All other bits must be clear
		count := 0
		for _, b := range bs {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<uint(bit)) != 0 {
					count++
				}
			}
		}
		if count != 1 {
			t.Errorf("single-router bit-string should have exactly 1 bit set, got %d", count)
		}
	})

	t.Run("Producer multihoming — producer reachable via multiple egress routers", func(t *testing.T) {
		// A producer announces the same prefix to two different routers.
		// The mapping protocol creates egress entries for both.
		bift := &fw.BiftState{}

		router1 := enc.Name{enc.NewGenericComponent("edge-1")}
		router2 := enc.Name{enc.NewGenericComponent("edge-2")}
		router3 := enc.Name{enc.NewGenericComponent("edge-3")}

		bift.RegisterRouter(router1, 0)
		bift.RegisterRouter(router2, 1)
		bift.RegisterRouter(router3, 2)

		// Producer attached to both router1 and router2 (multihoming)
		egressRouters := []enc.Name{router1, router2}
		bs := bift.BuildBierBitString(egressRouters)

		if !fw.BierGetBit(bs, 0) {
			t.Error("multihomed router1 (bit 0) should be set")
		}
		if !fw.BierGetBit(bs, 1) {
			t.Error("multihomed router2 (bit 1) should be set")
		}
		if fw.BierGetBit(bs, 2) {
			t.Error("router3 (bit 2) should NOT be set — producer not attached there")
		}
	})
}

func TestBfirEncodingFromEgressRouters(t *testing.T) {
	// Simulate the full BFIR flow: PET has multiple egress routers for a
	// Sync group prefix → BFIR builds bit-string → stateless replication.
	//
	// Uses the topology simulator to show end-to-end BFIR behavior.

	t.Run("BFIR encodes egress routers and delivers to all", func(t *testing.T) {
		// 5x5 grid: router 0 is the BFIR (first router receiving Sync Interest)
		g := buildGrid(5, 5)

		// Sync group members are at corners: 4, 20, 24
		members := []int{4, 20, 24}
		bs := g.buildBitstring(members...)

		// BFIR at router 0 injects the bit-string
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, members...)
	})

	t.Run("BFIR encoding preserves per-router bit accuracy", func(t *testing.T) {
		bift := &fw.BiftState{}

		// Register routers at specific BFR-IDs (operator-assigned)
		names := make([]enc.Name, 10)
		for i := 0; i < 10; i++ {
			names[i] = enc.Name{enc.NewGenericComponent(fmt.Sprintf("r%d", i))}
			bift.RegisterRouter(names[i], i)
		}

		// BFIR has egress routers at positions 0, 3, 7, 9
		egressRouters := []enc.Name{names[0], names[3], names[7], names[9]}
		bs := bift.BuildBierBitString(egressRouters)

		// Verify exact bit positions
		expected := map[int]bool{0: true, 3: true, 7: true, 9: true}
		for i := 0; i < 10; i++ {
			if expected[i] {
				if !fw.BierGetBit(bs, i) {
					t.Errorf("BFIR encoding: bit %d should be set (egress router present)", i)
				}
			} else {
				if fw.BierGetBit(bs, i) {
					t.Errorf("BFIR encoding: bit %d should NOT be set (no egress router)", i)
				}
			}
		}
	})

	t.Run("BFIR with no valid egress routers returns nil", func(t *testing.T) {
		bift := &fw.BiftState{}
		// No routers registered — empty mapping
		bs := bift.BuildBierBitString(nil)
		if bs != nil {
			t.Error("BFIR with no egress routers should produce nil bit-string")
		}
	})
}

func TestBfrReplicationAtEachHop(t *testing.T) {
	// Linear topology: 0 - 1 - 2 - 3 - 4
	// BFIR=0, BFERs=2,4. Routers 1 and 3 are pure BFR (transit).
	g := buildLinear(5)

	t.Run("BFR router 1 forwards without local delivery", func(t *testing.T) {
		bs := g.buildBitstring(2, 4)
		res := g.simulate(0, bs)

		// Router 1 is BFR — it must forward but NOT deliver locally
		if res.delivered[1] {
			t.Error("BFR router 1 must not deliver locally")
		}
		// BFERs 2 and 4 must receive
		assertDeliveredExactly(t, res, 2, 4)
	})

	t.Run("BFR router 3 forwards toward router 4", func(t *testing.T) {
		bs := g.buildBitstring(4)
		res := g.simulate(0, bs)

		// Routers 1, 2, 3 are all transit — none should get local delivery
		for _, transitRouter := range []int{1, 2, 3} {
			if res.delivered[transitRouter] {
				t.Errorf("transit router %d must not deliver locally", transitRouter)
			}
		}
		assertDeliveredExactly(t, res, 4)
	})
}

func TestBferLocalDelivery(t *testing.T) {
	t.Run("BFER strips BIER and delivers to local app", func(t *testing.T) {
		// In a 3-node linear chain (0-1-2), BFIR=0, BFER=2.
		// At router 2, the local bit is set → strip BIER, deliver locally.
		g := buildLinear(3)
		bs := g.buildBitstring(2)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 2)
	})

	t.Run("BFER at intermediate position delivers and does not forward stale bits", func(t *testing.T) {
		// Linear: 0-1-2-3-4, BFIR=0, BFERs=2,4
		// Router 2 is BFER: delivers locally AND forwards remaining bits
		g := buildLinear(5)
		bs := g.buildBitstring(2, 4)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 2, 4)

		// Verify router 2 delivered (BFER behavior)
		if !res.delivered[2] {
			t.Error("BFER router 2 must deliver locally")
		}
		// Verify router 4 also delivered (BFER via forwarding past router 2)
		if !res.delivered[4] {
			t.Error("BFER router 4 must receive delivery via forwarding")
		}
	})
}

func TestCombinedBfrBferBehavior(t *testing.T) {
	// A router can be BOTH BFR (forward to others) AND BFER (deliver locally)
	// simultaneously when its own bit is set plus other bits remain.

	t.Run("Router is simultaneously BFER and BFR", func(t *testing.T) {
		// Star topology: hub=0, leaves=1,2,3
		// BFIR=1, bitstring={0, 2, 3}
		// Router 0 (hub) has its own bit set (BFER) PLUS needs to forward
		// to leaves 2 and 3 (BFR).
		g := buildStar(3)
		bs := g.buildBitstring(0, 2, 3)
		res := g.simulate(1, bs)

		// Hub 0 must deliver locally (BFER) AND forward to 2,3 (BFR)
		assertDeliveredExactly(t, res, 0, 2, 3)
	})

	t.Run("All routers are both BFER and BFR in full-mesh multicast", func(t *testing.T) {
		// 6-node full mesh, all nodes in Sync group
		g := buildFullMesh(6)
		all := []int{0, 1, 2, 3, 4, 5}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})
}

func TestBierBitStringPropagation(t *testing.T) {
	// Verify that the BIER bit-string is correctly modified at each hop:
	// - Bits for routers reachable via a forwarded face are masked out
	// - Only the relevant bits are forwarded to each neighbor
	// - Loop suppression prevents double-forwarding

	t.Run("Bit-string is correctly reduced at each hop in linear chain", func(t *testing.T) {
		// Linear 0-1-2-3: BFIR=0, BFERs={1,2,3}
		// At hop 0→1: bit-string includes {1,2,3}
		// At hop 1→2: router 1 delivers locally (clears bit 1), forwards {2,3}
		// At hop 2→3: router 2 delivers locally (clears bit 2), forwards {3}
		g := buildLinear(4)
		bs := g.buildBitstring(1, 2, 3)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 1, 2, 3)

		// Verify hop counts increase along the chain
		if res.hopCounts[1] >= res.hopCounts[2] {
			t.Errorf("router 1 (hop %d) should be reached before router 2 (hop %d)",
				res.hopCounts[1], res.hopCounts[2])
		}
		if res.hopCounts[2] >= res.hopCounts[3] {
			t.Errorf("router 2 (hop %d) should be reached before router 3 (hop %d)",
				res.hopCounts[2], res.hopCounts[3])
		}
	})

	t.Run("F-BM masking splits bit-string correctly at branching points", func(t *testing.T) {
		// Binary tree depth 3 (7 nodes): root=0, left subtree={1,3,4}, right subtree={2,5,6}
		// BFIR=0, BFERs=all leaves {3,4,5,6}
		//
		// At root 0: F-BM for child 1 covers {1,3,4}, F-BM for child 2 covers {2,5,6}
		// Root ANDs bitstring {3,4,5,6} with each F-BM:
		//   → child 1 gets AND({3,4,5,6}, {1,3,4}) = {3,4}
		//   → child 2 gets AND({3,4,5,6}, {2,5,6}) = {5,6}
		g := buildBinaryTree(3)
		bs := g.buildBitstring(3, 4, 5, 6)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 3, 4, 5, 6)

		// Ensure no internal nodes received delivery
		if res.delivered[1] {
			t.Error("internal node 1 should not get delivery")
		}
		if res.delivered[2] {
			t.Error("internal node 2 should not get delivery")
		}
	})

	t.Run("Loop suppression prevents duplicate delivery in ring", func(t *testing.T) {
		// Ring of 8 nodes: 0-1-2-3-4-5-6-7-0
		// Despite redundant paths, each BFER should receive exactly once.
		g := buildRing(8)
		all := []int{0, 1, 2, 3, 4, 5, 6, 7}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)

		// Every node delivered exactly once
		assertDeliveredExactly(t, res, all...)
	})
}

func TestSyncGroupEndToEndScenario(t *testing.T) {
	// Scenario: 5x5 grid network. A Sync group has 6 members at various
	// positions (corners and center). When any member sends a Sync Interest,
	// all other members should receive it via BIER multicast.

	const rows, cols = 5, 5
	g := buildGrid(rows, cols)
	id := func(r, c int) int { return r*cols + c }

	// Sync group members (simulating prefix-to-routing mapping results)
	groupMembers := []int{
		id(0, 0),           // top-left corner
		id(0, cols-1),      // top-right corner
		id(rows-1, 0),      // bottom-left corner
		id(rows-1, cols-1), // bottom-right corner
		id(rows/2, cols/2), // center
		id(rows/2, 0),      // left edge center
	}

	t.Run("Any member can multicast to all others", func(t *testing.T) {
		// Each member sends a Sync Interest to all other members
		for _, sender := range groupMembers {
			// Build egress list: all members except sender
			var receivers []int
			for _, m := range groupMembers {
				if m != sender {
					receivers = append(receivers, m)
				}
			}

			bs := g.buildBitstring(receivers...)
			res := g.simulate(sender, bs)
			assertDeliveredExactly(t, res, receivers...)

			// Verify no non-member received delivery
			receiverSet := make(map[int]bool)
			for _, r := range receivers {
				receiverSet[r] = true
			}
			for delivered := range res.delivered {
				if !receiverSet[delivered] {
					t.Errorf("sender=%d: non-member router %d received Sync Interest", sender, delivered)
				}
			}
		}
	})

	t.Run("Full group multicast from each member", func(t *testing.T) {
		// Every member sends to ALL members (including self for BFER test)
		for _, sender := range groupMembers {
			bs := g.buildBitstring(groupMembers...)
			res := g.simulate(sender, bs)
			assertDeliveredExactly(t, res, groupMembers...)
		}
	})

	t.Run("Non-members never receive Sync Interest", func(t *testing.T) {
		memberSet := make(map[int]bool)
		for _, m := range groupMembers {
			memberSet[m] = true
		}

		bs := g.buildBitstring(groupMembers...)
		res := g.simulate(groupMembers[0], bs)

		for id := 0; id < rows*cols; id++ {
			if !memberSet[id] && res.delivered[id] {
				t.Errorf("non-member router %d received Sync Interest", id)
			}
		}
	})
}

func TestNdnLpBierCarriage(t *testing.T) {
	// Verify that BIER bit-strings survive multi-hop forwarding.
	// In the real system, the bit-string is carried in NDN LP TLV 0x035a.
	// In our simulation, this is modeled by the bit-string being passed
	// through the simulate() function at each hop.

	t.Run("Bit-string integrity across 10 hops", func(t *testing.T) {
		g := buildLinear(11) // 0-1-2-...-10
		bs := g.buildBitstring(10)
		res := g.simulate(0, bs)

		assertDeliveredExactly(t, res, 10)
		if res.hopCounts[10] != 10 {
			t.Errorf("expected 10 hops to reach router 10, got %d", res.hopCounts[10])
		}
	})

	t.Run("Bit-string with many bits survives multi-hop delivery", func(t *testing.T) {
		// 64-node linear chain, every odd node is a BFER
		const n = 64
		g := buildLinear(n)
		var oddNodes []int
		for i := 1; i < n; i += 2 {
			oddNodes = append(oddNodes, i)
		}
		bs := g.buildBitstring(oddNodes...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, oddNodes...)
	})
}
