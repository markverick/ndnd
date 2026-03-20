package sim

// DV end-to-end integration tests.
//
// These tests exercise the FULL path:
//
//   producer registers handler
//   → AnnouncePrefix via DV (NOT a manual AddRoute / AddFib call)
//   → DV sync propagates prefix to remote nodes
//   → remote node installs FIB route via NFDC
//   → consumer Interest is forwarded along that route
//   → producer replies with Data
//   → consumer callback fires with InterestResultData
//
// The existing consumer_test.go tests bypass DV entirely: they pre-install
// FIB routes with Node.AddRoute().  This file closes that coverage gap.
//
// WHY THESE TESTS MUST CURRENTLY FAIL
// ────────────────────────────────────
// The simulation run (run.sh sim scalability --grids 2 --trials 1 --window 10)
// shows that DV prefix-table propagation is incomplete: nodes log
//   "Reset remote prefixes router=/minindn/nodeX"
// but the expected follow-up
//   "Add remote prefix router=/minindn/nodeX name=/ndn/test"
// never arrives for the indirectly connected nodes.  As a result the FIB on
// those nodes has no route for /ndn/test and every consumer Interest times out,
// producing an empty rate trace → the fail-loud validation in _helpers.py
// raises RuntimeError.
//
// These tests expose the same bug without requiring a full ns-3 binary.

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

// ─── helpers ────────────────────────────────────────────────────────────────

// startDvOnNodes starts DV routing on every node in ns, using router names
// /minindn/node0, /minindn/node1, …  A shared trust root is created for
// the /minindn network.
//
// All network faces MUST be added to every node before this is called.
func startDvOnNodes(t *testing.T, ns []*Node) {
	t.Helper()
	for i, n := range ns {
		routerName := fmt.Sprintf("/minindn/node%d", i)
		if err := n.StartDv("/minindn", routerName, ""); err != nil {
			t.Fatalf("StartDv node%d (%s): %v", i, routerName, err)
		}
	}
}

// wireLinear connects nodes in a linear chain: n[0] ↔ n[1] ↔ n[2] ↔ …
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

// ─── tests ──────────────────────────────────────────────────────────────────

// TestDvTwoNodeEndToEnd verifies that a consumer on node1 can fetch Data from
// a producer on node0 when routing is provided entirely by DV.
//
// This is the simplest possible DV integration path: one direct link.
// The test MUST FAIL with the current code because DV prefix-table
// propagation via SVS does not complete: node1 logs "Reset remote prefixes"
// for node0 but never receives "Add remote prefix /ndn/test", so the FIB
// on node1 has no route for /ndn/test and the Interest times out.
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

	// Announce the prefix via DV — NOT via Node.AddRoute.
	// This is what the real simulation uses: cgo_export.go calls
	// node.AnnouncePrefixToDv from NdndSimRegisterProducer.
	n0.AnnouncePrefixToDv(prefix, 0)

	// Advance 30 s of simulation time — six full heartbeat cycles with the
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
			"node1 did not receive Data for %s after 30 s of DV simulation — "+
				"DV route installation or forwarding is broken (producer served %d interests; expected at least 1)",
			prefix, atomic.LoadInt64(served),
		)
	}
}

// TestDvThreeNodeMultiHopEndToEnd verifies prefix reachability across a
// two-hop path: producer on node0, consumer on node2, with node1 in between.
//
// This mirrors the 2×2 grid simulation failure where nodes diagonally
// opposite the producer (highest hop-count) never received "Add remote prefix"
// notifications.  The test MUST FAIL with the current code.
//
// Topology:  node0 ── node1 ── node2
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

	// Wire: node0 ↔ node1 ↔ node2 (all faces added before StartDv).
	wireLinear(clock, nodes)

	startDvOnNodes(t, nodes)

	prefix := mustName(t, "/ndn/test")

	// Producer lives on node0.
	served := attachProducer(t, nodes[0], prefix)

	// Announce only on node0 — DV must propagate it to node1 and node2.
	nodes[0].AnnouncePrefixToDv(prefix, 0)

	// 60 s = 12 heartbeat cycles.  More than enough for two-hop propagation
	// if the protocol implementation is correct.
	clock.Advance(60 * time.Second)

	// Consumer sends from node2.  Its only route to /ndn/test must come
	// from DV.  Without the fix, the Interest is dropped (no FIB entry).
	if got := expressOne(t, nodes[2].Engine(), clock, prefix); got == 0 {
		t.Fatalf(
			"node2 (2-hop consumer) did not receive Data for %s after 60 s — "+
				"DV prefix propagation did not reach the indirect node "+
				"(producer served %d interests; if 0, route was never installed on node1 either)",
			prefix, atomic.LoadInt64(served),
		)
	}
}

// TestDvPrefixWithdrawalStopsTraffic verifies that after a prefix is
// withdrawn via DV, consumers can no longer reach the producer.
//
// This test MUST FAIL with the current code because:
//   - If forward propagation is broken (TestDvTwoNodeEndToEnd fails),
//     withdrawal has no observable effect and the test logic cannot verify
//     withdrawal semantics.
//   - Even if forward propagation were fixed, withdrawal propagation has
//     the same SVS-based mechanism and is equally untested.
//
// The test is written in "pass after forward fix" style: once the prefix
// can be announced and used (the first assertion), it then withdraws and
// checks that traffic stops.
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
			"phase 1 (announce): node1 got no Data — "+
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
			"phase 2 (withdraw): node1 still received Data after withdrawal — "+
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

// TestDvDiamondFastFailoverStrict is an intentionally strict multi-hop test
// that is expected to fail with current DV timers.
//
// Why it should fail now:
// - Router dead interval defaults to 30s.
// - This test demands good recovery within 10s after link loss.
//
// This captures a realistic operator expectation for near-real-time failover
// under burst traffic and provides a concrete failing target for future work.
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

// TestDvProducerMobilityFastRecoveryStrict is an intentionally strict
// multi-hop mobility test expected to fail today.
//
// Scenario (line topology):
//   node0 (consumer) -- node1 -- node2 -- node3 (producer A)
//
// After baseline traffic from producer A, the producer moves to node1:
//   - node3 withdraws and stops serving
//   - node1 starts serving and announces the same prefix
//
// We require high delivery quality within 2s after the move. This is a
// realistic but aggressive SLO and is expected to fail with current timing.
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

// TestDvBranchSwitchMobilityStrict is a realistic >1-hop branch-switch test
// intended to be a failing target.
//
// Topology:
//
//           node3 (producer A)
//             |
// node0 --- node1
//   |         
// node2 --- node4 (producer B)
//
// Paths from consumer node0 to producers are both 2 hops:
//   A path: node0 -> node1 -> node3
//   B path: node0 -> node2 -> node4
//
// We move the producer from branch A to branch B and require high-quality
// recovery within 2s, which is intentionally strict and expected to fail.
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
