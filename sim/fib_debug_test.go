package sim

// Diagnostic test to trace FIB state during DV setup.
// This is temporary -- remove after fixing the partition test flakiness.

import (
	"fmt"
	"testing"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
)

// dumpFIB prints every FIB entry for a node.
func dumpFIB(t *testing.T, label string, node *Node) {
	t.Helper()
	fib := node.Forwarder.Thread().Fib()
	entries := fib.GetAllForwardingStrategies()
	t.Logf("=== FIB dump for %s (node %d, appFace=%d) ===", label, node.ID(), node.AppFaceID())
	for _, e := range entries {
		nhs := fib.FindNextHopsEnc(e.Name())
		nhStr := ""
		for _, nh := range nhs {
			nhStr += fmt.Sprintf(" face=%d/cost=%d", nh.Nexthop, nh.Cost)
		}
		strategy := fib.FindStrategyEnc(e.Name())
		t.Logf("  %s -> [%s] strategy=%s", e.Name(), nhStr, strategy)
	}
}

// TestDebugPartitionFIB reproduces the TestDvLinePartitionDisconnectsTrafficStrict
// setup and dumps FIB state to find the root cause of intermittent failures.
func TestDebugPartitionFIB(t *testing.T) {
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

	// Log face IDs for each node
	for i, n := range nodes {
		t.Logf("node%d: appFace=%d", i, n.AppFaceID())
	}

	wireLinear(clock, nodes)

	// Log interface face IDs
	for i, n := range nodes {
		for ifIdx, fID := range n.ifaceFaces {
			t.Logf("node%d: ifIndex=%d -> faceID=%d", i, ifIdx, fID)
		}
	}

	startDvOnNodes(t, nodes)

	// Dump FIB state AFTER all setup, BEFORE any clock advancement
	pfxPFS, _ := enc.NameFromStr("/minindn/32=DV/32=PFS")
	for i, n := range nodes {
		dumpFIB(t, fmt.Sprintf("node%d-after-setup", i), n)
		nhs := n.Forwarder.Thread().Fib().FindNextHopsEnc(pfxPFS)
		t.Logf("node%d PFS nexthops: %d", i, len(nhs))
		for _, nh := range nhs {
			t.Logf("  face=%d cost=%d", nh.Nexthop, nh.Cost)
		}
	}

	// Check data-fetch name lookup on each node BEFORE any clock advance
	// This is the exact prefix that PFS data fetch uses
	dataFetchName, _ := enc.NameFromStr("/minindn/32=DV/32=PFS/minindn/node3")
	for i, n := range nodes {
		nhs := n.Forwarder.Thread().Fib().FindNextHopsEnc(dataFetchName)
		t.Logf("node%d data-fetch nexthops for %s: %d", i, dataFetchName, len(nhs))
		for _, nh := range nhs {
			t.Logf("  face=%d cost=%d", nh.Nexthop, nh.Cost)
		}
	}

	// Also check the SVS sync prefix lookup
	pfxSVS, _ := enc.NameFromStr("/minindn/32=DV/32=PFS/32=svs")
	for i, n := range nodes {
		nhs := n.Forwarder.Thread().Fib().FindNextHopsEnc(pfxSVS)
		t.Logf("node%d SVS nexthops: %d", i, len(nhs))
		for _, nh := range nhs {
			t.Logf("  face=%d cost=%d", nh.Nexthop, nh.Cost)
		}
	}

	// Verify: on node3, a data fetch for its OWN data should reach the app face
	dataFetchLocal, _ := enc.NameFromStr("/minindn/32=DV/32=PFS/minindn/node3/t=1/32=SNAP/32=metadata")
	nhsLocal := nodes[3].Forwarder.Thread().Fib().FindNextHopsEnc(dataFetchLocal)
	t.Logf("node3 local-data-fetch nexthops: %d", len(nhsLocal))
	for _, nh := range nhsLocal {
		t.Logf("  face=%d cost=%d", nh.Nexthop, nh.Cost)
	}

	// Check KEY fetch name lookup
	keyFetch, _ := enc.NameFromStr("/minindn/node3/32=DV/KEY")
	for i, n := range nodes {
		nhs := n.Forwarder.Thread().Fib().FindNextHopsEnc(keyFetch)
		t.Logf("node%d KEY-fetch nexthops: %d", i, len(nhs))
		for _, nh := range nhs {
			t.Logf("  face=%d cost=%d", nh.Nexthop, nh.Cost)
		}
	}

	// Announce prefix and advance
	prefix := mustName(t, "/ndn/partition")
	_ = attachProducer(t, nodes[3], prefix)
	nodes[3].AnnouncePrefixToDv(prefix, 0)
	clock.Advance(60 * time.Second)

	// Dump FIB state AFTER convergence
	t.Logf("=== FIB state AFTER 60s convergence ===")
	for i, n := range nodes {
		dumpFIB(t, fmt.Sprintf("node%d-after-convergence", i), n)
	}

	// Check PFS data fetch nexthops AFTER convergence
	for i, n := range nodes {
		nhs := n.Forwarder.Thread().Fib().FindNextHopsEnc(pfxPFS)
		t.Logf("node%d PFS nexthops after convergence: %d", i, len(nhs))
		for _, nh := range nhs {
			t.Logf("  face=%d cost=%d", nh.Nexthop, nh.Cost)
		}
	}
}
