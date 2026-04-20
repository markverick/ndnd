package sim

// DV end-to-end integration tests.
//
// These tests exercise the FULL path:
//
//   producer registers handler
//   -> AnnouncePrefix via DV (NOT a manual AddRoute / AddFib call)
//   -> DV sync propagates prefix to remote nodes
//   -> remote node installs FIB route via NFDC
//   -> consumer Interest is forwarded along that route
//   -> producer replies with Data
//   -> consumer callback fires with InterestResultData
//
// The existing consumer_test.go tests bypass DV entirely: they pre-install
// FIB routes with Node.AddRoute().  This file closes that coverage gap.
//
// NOTE: These tests use the pure-Go DeterministicClock, not ns-3.
// The global event ID collision fix (globalNextEvID in cgo_export.go) only
// applies to Ns3Clock.  If tests still fail, the issue is likely in the
// DV prefix-table propagation path within the pure-Go simulation harness.

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

// --- helpers ----------------------------------------------------------------

// startDvOnNodes starts DV routing on every node in ns, using router names
// /minindn/node0, /minindn/node1, ...  A shared trust root is created for
// the /minindn network.
//
// All network faces MUST be added to every node before this is called.
func startDvOnNodes(t *testing.T, ns []*Node) {
	t.Helper()

	// Pre-generate all node certificates so each node's keychain includes
	// every peer's cert at startup. This mirrors real NDN deployments
	// where certs are distributed out-of-band, and avoids timing-dependent
	// cert fetch failures that cause non-deterministic DV convergence.
	trust, err := GetSimTrust("/minindn")
	if err != nil {
		t.Fatalf("GetSimTrust: %v", err)
	}
	routers := make([]string, len(ns))
	for i := range ns {
		routers[i] = fmt.Sprintf("/minindn/node%d", i)
	}
	if err := trust.PreGenerateCerts(routers); err != nil {
		t.Fatalf("PreGenerateCerts: %v", err)
	}

	for i, n := range ns {
		if err := n.StartDv("/minindn", routers[i], ""); err != nil {
			t.Fatalf("StartDv node%d (%s): %v", i, routers[i], err)
		}
	}
}

// wireLinear connects nodes in a linear chain: n[0] <-> n[1] <-> n[2] <-> ...
// Each adjacent pair shares a bidirectional point-to-point link.
//
//	node i uses ifIndex 0 for its left neighbour (i-1)
//	node i uses ifIndex 1 for its right neighbour (i+1)
//
// (Node 0 only has an ifIndex 0 face toward node 1;
//
//	the last node only has an ifIndex 0 face toward its left neighbour.)
func wireLinear(clock *DeterministicClock, ns []*Node) {
	for i := 0; i < len(ns)-1; i++ {
		left := ns[i]
		right := ns[i+1]

		// left's ifIndex for the right-ward link:  single-hop nodes use 0;
		// multi-hop middle nodes use ifIndex = 1 for their rightward face.
		leftIfIdx := uint32(0)
		if i > 0 {
			leftIfIdx = 1
		}
		rightIfIdx := uint32(0) // every node always connects left-ward on ifIndex 0

		// Capture loop variables for closures.
		l, r := left, right
		ri, li := rightIfIdx, leftIfIdx

		l.AddNetworkFace(li, func(_ uint64, frame []byte) {
			buf := append([]byte(nil), frame...)
			clock.Schedule(0, func() {
				r.ReceiveOnInterface(ri, buf)
			})
		})
		r.AddNetworkFace(ri, func(_ uint64, frame []byte) {
			buf := append([]byte(nil), frame...)
			clock.Schedule(0, func() {
				l.ReceiveOnInterface(li, buf)
			})
		})
	}
}

// wirePair creates one bidirectional point-to-point link between two nodes.
// aIf and bIf are interface indices on each endpoint.
func wirePair(clock *DeterministicClock, a *Node, aIf uint32, b *Node, bIf uint32) {
	a.AddNetworkFace(aIf, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() {
			b.ReceiveOnInterface(bIf, buf)
		})
	})
	b.AddNetworkFace(bIf, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() {
			a.ReceiveOnInterface(aIf, buf)
		})
	})
}

// expressOne sends a few probe Interests and returns 1 if any probe receives
// Data (InterestResultData), 0 otherwise.
//
// A single probe can be lost while DV/FIB updates settle; retrying keeps this
// integration test strict without making it flaky.
func expressOne(t *testing.T, eng ndn.Engine, clock *DeterministicClock, name enc.Name) (gotData int64) {
	t.Helper()

	for attempt := 0; attempt < 5; attempt++ {
		iName := append(enc.Name(nil), name...)
		iName = append(iName,
			enc.NewGenericComponent("probe"),
			enc.NewGenericComponent(fmt.Sprintf("%d", attempt)),
		)

		interest, err := eng.Spec().MakeInterest(iName, &ndn.InterestConfig{
			MustBeFresh: true,
			Lifetime:    optional.Some(1 * time.Second),
			Nonce:       utils.ConvertNonce(eng.Timer().Nonce()),
		}, nil, nil)
		if err != nil {
			t.Fatalf("MakeInterest: %v", err)
		}

		var received int64
		if err := eng.Express(interest, func(args ndn.ExpressCallbackArgs) {
			if args.Result == ndn.InterestResultData {
				atomic.StoreInt64(&received, 1)
			}
		}); err != nil {
			t.Fatalf("Express: %v", err)
		}

		// Allow one full lifetime + timeout slack.
		clock.Advance(1200 * time.Millisecond)
		if atomic.LoadInt64(&received) != 0 {
			return 1
		}
	}

	return 0
}

// expressOnceStrict sends one Interest and returns true only if Data arrives.
// No retries are done; this is useful for strict burst-quality assertions.
func expressOnceStrict(t *testing.T, eng ndn.Engine, clock *DeterministicClock, name enc.Name, seq int) bool {
	t.Helper()

	iName := append(enc.Name(nil), name...)
	iName = append(iName,
		enc.NewGenericComponent("burst"),
		enc.NewGenericComponent(fmt.Sprintf("%d", seq)),
	)

	interest, err := eng.Spec().MakeInterest(iName, &ndn.InterestConfig{
		MustBeFresh: true,
		Lifetime:    optional.Some(800 * time.Millisecond),
		Nonce:       utils.ConvertNonce(eng.Timer().Nonce()),
	}, nil, nil)
	if err != nil {
		t.Fatalf("MakeInterest: %v", err)
	}

	var got int64
	if err := eng.Express(interest, func(args ndn.ExpressCallbackArgs) {
		if args.Result == ndn.InterestResultData {
			atomic.StoreInt64(&got, 1)
		}
	}); err != nil {
		t.Fatalf("Express: %v", err)
	}

	clock.Advance(1 * time.Second)
	return atomic.LoadInt64(&got) != 0
}

func expressBurstStrict(t *testing.T, eng ndn.Engine, clock *DeterministicClock, name enc.Name, count int) int {
	t.Helper()
	ok := 0
	for i := 0; i < count; i++ {
		if expressOnceStrict(t, eng, clock, name, i) {
			ok++
		}
	}
	return ok
}

// attachProducer registers a handler for prefix on eng that replies with
// signed Data and counts every served Interest.  Returns a pointer to the
// counter.
func attachProducer(t *testing.T, node *Node, prefix enc.Name) *int64 {
	t.Helper()
	eng := node.Engine()
	var served int64
	signer := sig.NewSha256Signer()
	if err := eng.AttachHandler(prefix, func(args ndn.InterestHandlerArgs) {
		atomic.AddInt64(&served, 1)
		data, err := eng.Spec().MakeData(
			args.Interest.Name(),
			&ndn.DataConfig{Freshness: optional.Some(1 * time.Second)},
			nil,
			signer,
		)
		if err == nil {
			_ = args.Reply(data.Wire)
		}
	}); err != nil {
		t.Fatalf("AttachHandler: %v", err)
	}

	// Producer application route: deliver matching Interests to the app face.
	// DV announcement propagates this prefix to other nodes, but the local node
	// still needs a route from the forwarder to its own producer handler.
	node.AddRoute(prefix, node.AppFaceID(), 0)

	return &served
}

// --- link simulation helpers ------------------------------------------------

// linkGate controls a bidirectional link. When blocked, packets in both
// directions are silently dropped (simulating link failure), but forwarder
// faces persist — matching production UDP behavior.
type linkGate struct {
	down atomic.Bool
}

func (g *linkGate) Block()   { g.down.Store(true) }
func (g *linkGate) Unblock() { g.down.Store(false) }

// wirePairGated creates a bidirectional link that can be toggled on/off.
func wirePairGated(clock *DeterministicClock, a *Node, aIf uint32, b *Node, bIf uint32) *linkGate {
	gate := &linkGate{}
	a.AddNetworkFace(aIf, func(_ uint64, frame []byte) {
		if gate.down.Load() {
			return
		}
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() {
			b.ReceiveOnInterface(bIf, buf)
		})
	})
	b.AddNetworkFace(bIf, func(_ uint64, frame []byte) {
		if gate.down.Load() {
			return
		}
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() {
			a.ReceiveOnInterface(aIf, buf)
		})
	})
	return gate
}

// wirePairWithDelay creates a bidirectional link with propagation delay.
func wirePairWithDelay(clock *DeterministicClock, a *Node, aIf uint32, b *Node, bIf uint32, delay time.Duration) {
	a.AddNetworkFace(aIf, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(delay, func() {
			b.ReceiveOnInterface(bIf, buf)
		})
	})
	b.AddNetworkFace(bIf, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(delay, func() {
			a.ReceiveOnInterface(aIf, buf)
		})
	})
}

// wireLinearWithDelay connects nodes in a linear chain with propagation delay.
// Same ifIndex layout as wireLinear.
func wireLinearWithDelay(clock *DeterministicClock, ns []*Node, delay time.Duration) {
	for i := 0; i < len(ns)-1; i++ {
		leftIfIdx := uint32(0)
		if i > 0 {
			leftIfIdx = 1
		}
		wirePairWithDelay(clock, ns[i], leftIfIdx, ns[i+1], 0, delay)
	}
}

// wireGrid connects nodes as a rows×cols grid with horizontal and vertical
// links. Node index = row*cols + col. Each node uses sequential ifIndex values.
func wireGrid(clock *DeterministicClock, nodes []*Node, cols int) {
	rows := len(nodes) / cols
	ifIdx := make([]uint32, len(nodes))
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			id := r*cols + c
			if c+1 < cols {
				rid := r*cols + c + 1
				wirePair(clock, nodes[id], ifIdx[id], nodes[rid], ifIdx[rid])
				ifIdx[id]++
				ifIdx[rid]++
			}
			if r+1 < rows {
				did := (r+1)*cols + c
				wirePair(clock, nodes[id], ifIdx[id], nodes[did], ifIdx[did])
				ifIdx[id]++
				ifIdx[did]++
			}
		}
	}
}

// startDvOnNodesWithConfig is like startDvOnNodes but passes cfgJSON to each node.
func startDvOnNodesWithConfig(t *testing.T, ns []*Node, cfgJSON string) {
	t.Helper()

	trust, err := GetSimTrust("/minindn")
	if err != nil {
		t.Fatalf("GetSimTrust: %v", err)
	}
	routers := make([]string, len(ns))
	for i := range ns {
		routers[i] = fmt.Sprintf("/minindn/node%d", i)
	}
	if err := trust.PreGenerateCerts(routers); err != nil {
		t.Fatalf("PreGenerateCerts: %v", err)
	}
	for i, n := range ns {
		if err := n.StartDv("/minindn", routers[i], cfgJSON); err != nil {
			t.Fatalf("StartDv node%d (%s): %v", i, routers[i], err)
		}
	}
}

// --- tests ------------------------------------------------------------------

// TestDvTwoNodeEndToEnd verifies that a consumer on node1 can fetch Data from
// a producer on node0 when routing is provided entirely by DV.
//
// This is the simplest possible DV integration path: one direct link.
func TestDvTwoNodeEndToEnd(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	n0 := NewNode(0, clock)
	n1 := NewNode(1, clock)

	if err := n0.Start(); err != nil {
		t.Fatalf("n0.Start: %v", err)
	}
	if err := n1.Start(); err != nil {
		t.Fatalf("n1.Start: %v", err)
	}
	defer n0.Stop()
	defer n1.Stop()

	// Wire before StartDv so both link faces are registered when StartDv
	// iterates ifaceFaces to install sync-prefix routes.
	n0.AddNetworkFace(0, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() { n1.ReceiveOnInterface(0, buf) })
	})
	n1.AddNetworkFace(0, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() { n0.ReceiveOnInterface(0, buf) })
	})

	if err := n0.StartDv("/minindn", "/minindn/node0", ""); err != nil {
		t.Fatalf("n0.StartDv: %v", err)
	}
	if err := n1.StartDv("/minindn", "/minindn/node1", ""); err != nil {
		t.Fatalf("n1.StartDv: %v", err)
	}

	prefix := mustName(t, "/ndn/test")

	// Register producer on n0's app engine.
	served := attachProducer(t, n0, prefix)

	// Announce the prefix via DV -- NOT via Node.AddRoute.
	// This is what the real simulation uses: cgo_export.go calls
	// node.AnnouncePrefixToDv from NdndSimRegisterProducer.
	n0.AnnouncePrefixToDv(prefix, 0)

	// Advance 30 s of simulation time -- six full heartbeat cycles with the
	// default 5 s AdvertisementSyncInterval.  If DV prefix propagation
	// worked correctly this is far more than enough for:
	//   1. router-to-router DV convergence (first heartbeat cycle)
	//   2. prefix-table SVS sync (triggered immediately on announce)
	//   3. FIB installation on n1 via NFDC
	clock.Advance(30 * time.Second)

	// n1 now sends a probe Interest.  If the FIB route was installed, the
	// Interest reaches n0's producer and we get Data.  If not, it times out.
	if got := expressOne(t, n1.Engine(), clock, prefix); got == 0 {
		t.Fatalf(
			"node1 did not receive Data for %s after 30 s of DV simulation -- "+
				"DV route installation or forwarding is broken (producer served %d interests; expected at least 1)",
			prefix, atomic.LoadInt64(served),
		)
	}
}

// TestDvThreeNodeMultiHopEndToEnd verifies prefix reachability across a
// two-hop path: producer on node0, consumer on node2, with node1 in between.
//
// Topology:  node0 -- node1 -- node2
//
// node0: producer for /ndn/test
// node1: pure forwarder (no app handler for /ndn/test)
// node2: consumer
func TestDvThreeNodeMultiHopEndToEnd(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 3)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	// Wire: node0 <-> node1 <-> node2 (all faces added before StartDv).
	wireLinear(clock, nodes)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/test")

	// Producer lives on node0.
	served := attachProducer(t, nodes[0], prefix)

	// Announce only on node0 -- DV must propagate it to node1 and node2.
	nodes[0].AnnouncePrefixToDv(prefix, 0)

	// 60 s = 12 heartbeat cycles.  More than enough for two-hop propagation
	// if the protocol implementation is correct.
	clock.Advance(60 * time.Second)

	// Consumer sends from node2.  Its only route to /ndn/test must come
	// from DV.  Without the fix, the Interest is dropped (no FIB entry).
	if got := expressOne(t, nodes[2].Engine(), clock, prefix); got == 0 {
		t.Fatalf(
			"node2 (2-hop consumer) did not receive Data for %s after 60 s -- "+
				"DV prefix propagation did not reach the indirect node "+
				"(producer served %d interests; if 0, route was never installed on node1 either)",
			prefix, atomic.LoadInt64(served),
		)
	}
}

// TestDvPrefixWithdrawalStopsTraffic verifies that after a prefix is
// withdrawn via DV, consumers can no longer reach the producer.
// Phase 1 confirms traffic flows after announcement, then phase 2
// withdraws the prefix and verifies traffic stops.
func TestDvPrefixWithdrawalStopsTraffic(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	n0 := NewNode(0, clock)
	n1 := NewNode(1, clock)

	if err := n0.Start(); err != nil {
		t.Fatalf("n0.Start: %v", err)
	}
	if err := n1.Start(); err != nil {
		t.Fatalf("n1.Start: %v", err)
	}
	defer n0.Stop()
	defer n1.Stop()

	n0.AddNetworkFace(0, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() { n1.ReceiveOnInterface(0, buf) })
	})
	n1.AddNetworkFace(0, func(_ uint64, frame []byte) {
		buf := append([]byte(nil), frame...)
		clock.Schedule(0, func() { n0.ReceiveOnInterface(0, buf) })
	})

	if err := n0.StartDv("/minindn", "/minindn/node0", ""); err != nil {
		t.Fatalf("n0.StartDv: %v", err)
	}
	if err := n1.StartDv("/minindn", "/minindn/node1", ""); err != nil {
		t.Fatalf("n1.StartDv: %v", err)
	}

	prefix := mustName(t, "/ndn/test")
	served := attachProducer(t, n0, prefix)

	// Phase 1: announce and verify traffic flows.
	n0.AnnouncePrefixToDv(prefix, 0)
	clock.Advance(30 * time.Second)

	if got := expressOne(t, n1.Engine(), clock, prefix); got == 0 {
		t.Fatalf(
			"phase 1 (announce): node1 got no Data -- "+
				"DV prefix propagation is broken " +
				"(producer served %d interests; withdrawal test cannot proceed)",
			atomic.LoadInt64(served),
		)
	}

	// Phase 2: withdraw and verify traffic stops.
	n0.WithdrawPrefixFromDv(prefix)
	clock.Advance(30 * time.Second) // let withdrawal propagate

	// All pending Interests will have expired.  Send a fresh one.
	if got := expressOne(t, n1.Engine(), clock, prefix); got != 0 {
		t.Fatalf(
			"phase 2 (withdraw): node1 still received Data after withdrawal -- "+
				"DV prefix withdrawal propagation is broken",
		)
	}
}

// TestDvDiamondFailoverBurstTraffic exercises a realistic multi-hop failover:
//
//   node0 (producer)
//     /   \
//   node1 node2
//     \   /
//     node3 (consumer)
//
// Traffic should succeed before failure. Then we cut the node1-node3 link and
// expect node3 to continue receiving data via node2 after DV re-convergence.
//
// This test is intentionally strict and aims to fail if failover propagation
// or forwarding-table updates lag/flake in realistic burst traffic.
func TestDvDiamondFailoverBurstTraffic(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	// Diamond links with explicit per-node ifIndex mapping.
	// node0: if0->node1, if1->node2
	// node1: if0->node0, if1->node3
	// node2: if0->node0, if1->node3
	// node3: if0->node1, if1->node2
	wirePair(clock, nodes[0], 0, nodes[1], 0)
	wirePair(clock, nodes[0], 1, nodes[2], 0)
	wirePair(clock, nodes[1], 1, nodes[3], 0)
	wirePair(clock, nodes[2], 1, nodes[3], 1)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/failover")
	served := attachProducer(t, nodes[0], prefix)
	nodes[0].AnnouncePrefixToDv(prefix, 0)

	// Initial convergence and steady state.
	clock.Advance(60 * time.Second)

	before := expressBurstStrict(t, nodes[3].Engine(), clock, prefix, 20)
	if before < 16 {
		t.Fatalf("pre-failure burst quality too low: got %d/20 successful fetches (producer served=%d)", before, atomic.LoadInt64(served))
	}

	// Fail one of two equal-cost paths: node1 <-> node3.
	nodes[1].RemoveNetworkFace(1)
	nodes[3].RemoveNetworkFace(0)

	// Allow dead-neighbor detection + advertisement + FIB update.
	// Default dead interval is 30s, so 90s is a strict but realistic window.
	clock.Advance(90 * time.Second)

	after := expressBurstStrict(t, nodes[3].Engine(), clock, prefix, 20)
	if after < 14 {
		t.Fatalf(
			"post-failure failover quality too low: got %d/20 successful fetches via alternate path (pre=%d/20, producer served=%d)",
			after, before, atomic.LoadInt64(served),
		)
	}
}

// TestDvDiamondFastFailoverStrict is a strict multi-hop failover test
// that demands recovery within 10s of link loss (below the default 30s
// dead interval). It verifies that the best-route strategy can reroute
// traffic through the surviving path without waiting for dead-neighbor
// detection.
func TestDvDiamondFastFailoverStrict(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wirePair(clock, nodes[0], 0, nodes[1], 0)
	wirePair(clock, nodes[0], 1, nodes[2], 0)
	wirePair(clock, nodes[1], 1, nodes[3], 0)
	wirePair(clock, nodes[2], 1, nodes[3], 1)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/fast-failover")
	served := attachProducer(t, nodes[0], prefix)
	nodes[0].AnnouncePrefixToDv(prefix, 0)

	clock.Advance(60 * time.Second)

	baseline := expressBurstStrict(t, nodes[3].Engine(), clock, prefix, 10)
	if baseline < 9 {
		t.Fatalf("baseline too low before failure: %d/10 (producer served=%d)", baseline, atomic.LoadInt64(served))
	}

	// Fail one branch of the diamond.
	nodes[1].RemoveNetworkFace(1)
	nodes[3].RemoveNetworkFace(0)

	// Intentionally strict: require recovery in 10s (well below default 30s dead interval).
	clock.Advance(10 * time.Second)

	fast := expressBurstStrict(t, nodes[3].Engine(), clock, prefix, 10)
	if fast < 8 {
		t.Fatalf(
			"fast failover target not met (expected >=8/10 within 10s, got %d/10; baseline=%d/10; producer served=%d)",
			fast, baseline, atomic.LoadInt64(served),
		)
	}
}

// TestDvProducerMobilityFastRecoveryStrict verifies producer mobility:
// the producer moves from node3 to node1 (closer to the consumer on node0)
// and we require high delivery quality within 2s of the move.
//
// Topology (line):  node0 -- node1 -- node2 -- node3
//
// After baseline traffic to producer A on node3, node3 withdraws and
// node1 starts serving and announces the same prefix.
func TestDvProducerMobilityFastRecoveryStrict(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	// 0-1-2-3 line.
	wireLinear(clock, nodes)
	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/mobile")
	servedA := attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	clock.Advance(60 * time.Second)

	baseline := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if baseline < 9 {
		t.Fatalf("baseline too low before mobility: %d/10 (servedA=%d)", baseline, atomic.LoadInt64(servedA))
	}

	// Producer mobility: old producer leaves, new producer appears.
	nodes[3].WithdrawPrefixFromDv(prefix)
	nodes[3].Engine().DetachHandler(prefix)
	nodes[3].RemoveRoute(prefix, nodes[3].AppFaceID())

	servedB := attachProducer(t, nodes[1], prefix)
	nodes[1].AnnouncePrefixToDv(prefix, 0)

	// Intentionally strict recovery budget.
	clock.Advance(2 * time.Second)

	fast := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if fast < 8 {
		t.Fatalf(
			"mobility fast-recovery target not met (expected >=8/10 within 2s, got %d/10; baseline=%d/10; servedA=%d servedB=%d)",
			fast, baseline, atomic.LoadInt64(servedA), atomic.LoadInt64(servedB),
		)
	}
}

// TestDvBranchSwitchMobilityStrict verifies producer mobility across
// different branches of the topology.
//
// Topology:
//
//           node3 (producer A)
//             |
// node0 --- node1
//   |         
// node2 --- node4 (producer B)
//
// Both producers are 2 hops from consumer node0. We move the producer from
// branch A (node3) to branch B (node4) and require high-quality recovery
// within 2s.
func TestDvBranchSwitchMobilityStrict(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 5)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	// node0-if0 <-> node1-if0
	// node0-if1 <-> node2-if0
	// node1-if1 <-> node3-if0
	// node2-if1 <-> node4-if0
	wirePair(clock, nodes[0], 0, nodes[1], 0)
	wirePair(clock, nodes[0], 1, nodes[2], 0)
	wirePair(clock, nodes[1], 1, nodes[3], 0)
	wirePair(clock, nodes[2], 1, nodes[4], 0)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/branch-switch")
	servedA := attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	clock.Advance(60 * time.Second)

	baseline := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if baseline < 9 {
		t.Fatalf("baseline too low before move: %d/10 (servedA=%d)", baseline, atomic.LoadInt64(servedA))
	}

	// Move producer from branch A to branch B.
	nodes[3].WithdrawPrefixFromDv(prefix)
	nodes[3].Engine().DetachHandler(prefix)
	nodes[3].RemoveRoute(prefix, nodes[3].AppFaceID())

	servedB := attachProducer(t, nodes[4], prefix)
	nodes[4].AnnouncePrefixToDv(prefix, 0)

	// Aggressive SLO: recover high quality within 2s.
	clock.Advance(2 * time.Second)

	fast := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if fast < 8 {
		t.Fatalf(
			"branch-switch fast recovery target not met (expected >=8/10 within 2s, got %d/10; baseline=%d/10; servedA=%d servedB=%d)",
			fast, baseline, atomic.LoadInt64(servedA), atomic.LoadInt64(servedB),
		)
	}
}

// TestDvLinePartitionDisconnectsTrafficStrict verifies that a hard partition
// in a line topology causes end-to-end traffic to stop.
//
// Topology: node0 -- node1 -- node2 -- node3(producer), consumer at node0.
//
// After removing the middle link (node1<->node2), node0 and node3 are in
// different connected components. The correct behavior is 0 successful fetches.
func TestDvLinePartitionDisconnectsTrafficStrict(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireLinear(clock, nodes)
	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/partition")
	served := attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	clock.Advance(60 * time.Second)

	baseline := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if baseline < 9 {
		t.Fatalf("baseline too low before partition: %d/10 (served=%d)", baseline, atomic.LoadInt64(served))
	}

	// Hard partition in the middle of the 3-hop path.
	nodes[1].RemoveNetworkFace(1)
	nodes[2].RemoveNetworkFace(0)

	clock.Advance(5 * time.Second)

	post := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if post != 0 {
		t.Fatalf(
			"hard partition should disconnect traffic (expected 0/10 after partition, got %d/10; baseline=%d/10; served=%d)",
			post, baseline, atomic.LoadInt64(served),
		)
	}
}

// --- regression tests for the global event ID collision bug -----------------

// TestDvNoDeadNeighborsStable runs a 4-node linear topology for 120 seconds
// of simulated time (24 heartbeat cycles at 5s interval, 4 deadcheck cycles
// at 30s interval) and verifies that data delivery remains perfect
// throughout. This is the integration-level regression test for the global
// event ID collision bug: if heartbeat or deadcheck events are silently
// dropped because of a cancel/ID collision, neighbors will be declared dead
// and FIB routes withdrawn, causing probe Interests to time out.
func TestDvNoDeadNeighborsStable(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireLinear(clock, nodes)
	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/stable")
	served := attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	// Converge: 30s is enough for 4-node DV + prefix propagation.
	clock.Advance(30 * time.Second)

	// Verify baseline reachability (node0 -> node3, 3 hops).
	if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
		t.Fatalf("baseline probe failed after 30s convergence (served=%d)",
			atomic.LoadInt64(served))
	}

	// Now run periodic probes every 10s for another 120s of simulated time.
	// This spans multiple heartbeat and deadcheck cycles.
	const probeInterval = 10 * time.Second
	const totalDuration = 120 * time.Second
	nProbes := int(totalDuration / probeInterval)
	failures := 0

	for i := 0; i < nProbes; i++ {
		clock.Advance(probeInterval)
		if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
			failures++
			t.Logf("probe %d/%d at t=%s failed (served=%d)",
				i+1, nProbes, clock.Now().Sub(time.Unix(0, 0)),
				atomic.LoadInt64(served))
		}
	}

	if failures > 0 {
		t.Fatalf("%d/%d probes failed -- neighbors likely declared dead due to "+
			"missed heartbeat/deadcheck events (served=%d)",
			failures, nProbes, atomic.LoadInt64(served))
	}
}

// TestDvSixNodeMeshStable runs a 6-node mesh (two parallel 3-node chains
// sharing endpoints) for 90s of simulated time. Multiple nodes scheduling
// heartbeats and deadchecks simultaneously exercises the event scheduler
// more aggressively than a simple linear topology.
//
//	n0 -- n1 -- n2
//	 \              /
//	  n3 -- n4 -- n5 (n5 wired to n2)
//
// Actually wired as a diamond with extra nodes for stress:
//
//	n0 -- n1 -- n3
//	|           |
//	n2 -- n4 -- n5
func TestDvSixNodeMeshStable(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	const N = 6
	nodes := make([]*Node, N)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	// Build mesh:
	//   n0 --(if0/if0)-- n1
	//   n1 --(if1/if0)-- n3
	//   n3 --(if1/if0)-- n5
	//   n5 --(if1/if0)-- n4
	//   n4 --(if1/if0)-- n2
	//   n2 --(if1/if1)-- n0
	wirePair(clock, nodes[0], 0, nodes[1], 0)
	wirePair(clock, nodes[1], 1, nodes[3], 0)
	wirePair(clock, nodes[3], 1, nodes[5], 0)
	wirePair(clock, nodes[5], 1, nodes[4], 0)
	wirePair(clock, nodes[4], 1, nodes[2], 0)
	wirePair(clock, nodes[2], 1, nodes[0], 1)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/mesh/data")
	served := attachProducer(t, nodes[5], prefix)
	nodes[5].AnnouncePrefixToDv(prefix, 0)

	// Converge
	clock.Advance(45 * time.Second)

	if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
		t.Fatalf("baseline probe n0->n5 failed (served=%d)",
			atomic.LoadInt64(served))
	}

	// Sustained probing for 90s
	const probeInterval = 10 * time.Second
	const totalDuration = 90 * time.Second
	nProbes := int(totalDuration / probeInterval)
	failures := 0

	for i := 0; i < nProbes; i++ {
		clock.Advance(probeInterval)
		if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
			failures++
			t.Logf("probe %d/%d at t=%s failed (served=%d)",
				i+1, nProbes, clock.Now().Sub(time.Unix(0, 0)),
				atomic.LoadInt64(served))
		}
	}

	if failures > 0 {
		t.Fatalf("mesh: %d/%d probes failed -- event scheduling may be broken "+
			"(served=%d)", failures, nProbes, atomic.LoadInt64(served))
	}
}

// --- scenario-aligned tests -------------------------------------------------
// These tests cover gaps between the existing integration tests and the actual
// simulation scenarios (churn, routing, one-step) run by the project.

// TestDvLinkFlapRecovery verifies that DV recovers after a link goes down
// and comes back up. This matches the real churn scenario's link_down/link_up
// event pattern (link down for ~5s then restored).
//
// Topology: node0 -- node1 --[gate]-- node2 -- node3(producer)
//
// The gated link drops packets when blocked but preserves forwarder faces,
// matching production behavior where UDP faces persist through brief outages.
func TestDvLinkFlapRecovery(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wirePair(clock, nodes[0], 0, nodes[1], 0)
	gate := wirePairGated(clock, nodes[1], 1, nodes[2], 0)
	wirePair(clock, nodes[2], 1, nodes[3], 0)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/flap")
	served := attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	// Phase 1: steady state.
	clock.Advance(60 * time.Second)
	baseline := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if baseline < 9 {
		t.Fatalf("baseline too low: %d/10 (served=%d)", baseline, atomic.LoadInt64(served))
	}

	// Phase 2: link down for 5s (matches churn scenario recovery delay).
	gate.Block()
	clock.Advance(5 * time.Second)

	during := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 5)
	if during != 0 {
		t.Fatalf("expected 0 deliveries during link-down, got %d/5", during)
	}

	// Phase 3: link restored. The neighbor was never declared dead (downtime
	// < 30s dead interval), so FIB routes are intact. Allow 2 heartbeat
	// cycles (10s) for the forwarder to confirm the path and for any stale
	// PIT entries to expire.
	gate.Unblock()
	clock.Advance(15 * time.Second)

	recovered := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if recovered < 8 {
		t.Fatalf("post-flap recovery too low: %d/10 (baseline=%d/10, served=%d)",
			recovered, baseline, atomic.LoadInt64(served))
	}
}

// TestDvGridTopologyReachability verifies DV convergence on a 3×3 grid,
// the primary topology used in churn simulations.
//
//	n0 -- n1 -- n2
//	|     |     |
//	n3 -- n4 -- n5
//	|     |     |
//	n6 -- n7 -- n8
//
// Producer on n8 (bottom-right), consumer on n0 (top-left).
// Shortest path is 4 hops; multiple equal-cost paths exist.
func TestDvGridTopologyReachability(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	const gridSize = 3
	const N = gridSize * gridSize
	nodes := make([]*Node, N)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireGrid(clock, nodes, gridSize)
	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/grid")
	served := attachProducer(t, nodes[N-1], prefix)
	nodes[N-1].AnnouncePrefixToDv(prefix, 0)

	// 3×3 grid diameter is 4 hops. DV needs 4 advertisement rounds
	// (5s each = 20s) plus PFS sync. 60s is generous.
	clock.Advance(60 * time.Second)

	// Consumer at the farthest corner.
	if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
		t.Fatalf("node0 (corner) did not receive Data for %s on 3×3 grid (served=%d)",
			prefix, atomic.LoadInt64(served))
	}

	// Also verify from the center node.
	if got := expressOne(t, nodes[4].Engine(), clock, prefix); got == 0 {
		t.Fatalf("node4 (center) did not receive Data for %s on 3×3 grid (served=%d)",
			prefix, atomic.LoadInt64(served))
	}

	// Burst quality from the farthest corner.
	quality := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if quality < 8 {
		t.Fatalf("grid burst quality too low from corner: %d/10 (served=%d)",
			quality, atomic.LoadInt64(served))
	}
}

// TestDvWithLinkPropagationDelay verifies DV convergence with 10ms per-hop
// propagation delay, matching real simulation parameters.
//
// Topology: node0 -- node1 -- node2 -- node3 (10ms per hop)
// RTT from node0 to node3 = 6 × 10ms = 60ms.
func TestDvWithLinkPropagationDelay(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireLinearWithDelay(clock, nodes, 10*time.Millisecond)
	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/delayed")
	served := attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	clock.Advance(60 * time.Second)

	if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
		t.Fatalf("node0 did not receive Data with 10ms link delay (served=%d)",
			atomic.LoadInt64(served))
	}

	quality := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if quality < 9 {
		t.Fatalf("burst quality with 10ms delay too low: %d/10 (served=%d)",
			quality, atomic.LoadInt64(served))
	}
}

// TestDvOneStepMode verifies DV in one-step routing mode where prefixes are
// carried directly in DV advertisements (no PrefixSync SVS).
//
// Topology: node0 -- node1 -- node2 (producer)
func TestDvOneStepMode(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 3)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireLinear(clock, nodes)
	startDvOnNodesWithConfig(t, nodes, `{"one_step": true}`)

	prefix := mustName(t, "/ndn/onestep")
	served := attachProducer(t, nodes[2], prefix)
	nodes[2].AnnouncePrefixToDv(prefix, 0)

	// One-step: prefix travels in DV adverts, not PFS. A few heartbeat
	// cycles (5s each) is enough for 2-hop propagation.
	clock.Advance(30 * time.Second)

	if got := expressOne(t, nodes[0].Engine(), clock, prefix); got == 0 {
		t.Fatalf("node0 did not receive Data in one-step mode (served=%d)",
			atomic.LoadInt64(served))
	}

	quality := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if quality < 9 {
		t.Fatalf("one-step burst quality too low: %d/10 (served=%d)",
			quality, atomic.LoadInt64(served))
	}
}

// TestDvMultiplePrefixes verifies that DV correctly propagates and routes
// multiple independent prefixes from different producers.
//
// Topology: node0(consumer) -- node1 -- node2
//
// node1 announces /ndn/prefix/A.
// node2 announces /ndn/prefix/B and /ndn/prefix/C.
// node0 must be able to fetch Data for all three.
func TestDvMultiplePrefixes(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 3)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireLinear(clock, nodes)
	startDvOnNodes(t, nodes)

	prefixA := mustName(t, "/ndn/prefix/A")
	prefixB := mustName(t, "/ndn/prefix/B")
	prefixC := mustName(t, "/ndn/prefix/C")

	servedA := attachProducer(t, nodes[1], prefixA)
	servedB := attachProducer(t, nodes[2], prefixB)
	servedC := attachProducer(t, nodes[2], prefixC)

	nodes[1].AnnouncePrefixToDv(prefixA, 0)
	nodes[2].AnnouncePrefixToDv(prefixB, 0)
	nodes[2].AnnouncePrefixToDv(prefixC, 0)

	clock.Advance(60 * time.Second)

	for _, tc := range []struct {
		desc   string
		prefix enc.Name
		served *int64
	}{
		{"A from node1", prefixA, servedA},
		{"B from node2", prefixB, servedB},
		{"C from node2", prefixC, servedC},
	} {
		if got := expressOne(t, nodes[0].Engine(), clock, tc.prefix); got == 0 {
			t.Fatalf("node0 did not receive Data for %s (%s, served=%d)",
				tc.prefix, tc.desc, atomic.LoadInt64(tc.served))
		}
	}
}

// TestDvConvergenceTimeBound measures actual DV convergence time on a 4-node
// linear topology and asserts it stays within a bound.
//
// Topology: node0(consumer) -- node1 -- node2 -- node3(producer)
//
// A 4-node line needs 3 DV advertisement rounds (5s each = 15s) for router
// reachability, then PFS SVS must sync the prefix. The initial PFS sync
// Interest is lost (PFS routes are not installed until neighbor discovery
// at ~5s), so propagation waits for the next PFS periodic sync (~30s).
// We assert convergence within 35s.
func TestDvConvergenceTimeBound(t *testing.T) {
	ResetSimTrust()

	clock := NewDeterministicClock(time.Unix(0, 0))
	nodes := make([]*Node, 4)
	for i := range nodes {
		nodes[i] = NewNode(uint32(i), clock)
		if err := nodes[i].Start(); err != nil {
			t.Fatalf("nodes[%d].Start: %v", i, err)
		}
		defer nodes[i].Stop()
	}

	wireLinear(clock, nodes)
	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/convergence")
	served := attachProducer(t, nodes[3], prefix)

	announceTime := clock.Now()
	nodes[3].AnnouncePrefixToDv(prefix, 0)

	// Probe every ~2s of sim time (1s advance + 1s inside expressOnceStrict).
	const maxProbes = 30
	var convergedAt time.Duration
	for i := 0; i < maxProbes; i++ {
		clock.Advance(1 * time.Second)
		if expressOnceStrict(t, nodes[0].Engine(), clock, prefix, i) {
			convergedAt = clock.Now().Sub(announceTime)
			break
		}
	}

	if convergedAt == 0 {
		t.Fatalf("DV did not converge within %d probes (served=%d)",
			maxProbes, atomic.LoadInt64(served))
	}

	t.Logf("DV converged at t=%s (4-node linear, 3 hops)", convergedAt)

	if convergedAt > 35*time.Second {
		t.Fatalf("convergence too slow: %s (expected <=35s, served=%d)",
			convergedAt, atomic.LoadInt64(served))
	}

	// Verify sustained quality after convergence.
	quality := expressBurstStrict(t, nodes[0].Engine(), clock, prefix, 10)
	if quality < 9 {
		t.Fatalf("post-convergence quality too low: %d/10 (converged at %s, served=%d)",
			quality, convergedAt, atomic.LoadInt64(served))
	}
}
