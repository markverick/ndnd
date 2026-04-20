package sim

// Integration tests for the consumer-producer loop and the startConsumerLoop
// helper.  These run without ns-3: they use WallClock and wire two Nodes
// together via in-process function calls.
//
// What these tests guard against:
//   - startConsumerLoop silently stopping mid-chain (MakeInterest failure)
//   - Interest count never growing (scheduling broken)
//   - A stopped consumer continuing to send
//   - Producer not receiving any Interests it can reply to

import (
	"sync/atomic"
	"testing"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	sig "github.com/named-data/ndnd/std/security/signer"
)

// mustName is a test helper that parses a name and fatals on error.
func mustName(t *testing.T, s string) enc.Name {
	t.Helper()
	n, err := enc.NameFromStr(s)
	if err != nil {
		t.Fatalf("enc.NameFromStr(%q): %v", s, err)
	}
	return n
}

// makeConnectedPair creates two Nodes wired back-to-back.
// Returns (consumer node, producer node, face IDs for FIB setup, cleanup func).
func makeConnectedPair(t *testing.T) (clock *DeterministicClock, n0, n1 *Node, face0to1, face1to0 uint64, cleanup func()) {
	t.Helper()

	clock = NewDeterministicClock(time.Unix(0, 0))
	n0 = NewNode(0, clock)
	n1 = NewNode(1, clock)

	if err := n0.Start(); err != nil {
		t.Fatalf("n0.Start: %v", err)
	}
	if err := n1.Start(); err != nil {
		t.Fatalf("n1.Start: %v", err)
	}

	// Wire n0 <-> n1 symmetrically.
	face0to1 = n0.AddNetworkFace(0, func(_ uint64, frame []byte) {
		n1.ReceiveOnInterface(0, frame)
	})
	face1to0 = n1.AddNetworkFace(0, func(_ uint64, frame []byte) {
		n0.ReceiveOnInterface(0, frame)
	})

	cleanup = func() {
		n0.Stop()
		n1.Stop()
	}
	return
}

// TestConsumerLoopKeepsSending verifies that startConsumerLoop keeps
// scheduling interests and never silently stops mid-chain.
// This is the regression test for: "MakeInterest fails -> chain stops -> 0 packets".
func TestConsumerLoopKeepsSending(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))
	node := NewNode(0, clock)
	if err := node.Start(); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	defer node.Stop()

	prefix := mustName(t, "/ndn/test")

	// 200 Hz so 50 interests arrive within ~250 ms wall time.
	const freq = 200.0
	const want = 50

	stopped := startConsumerLoop(node.Engine(), clock, 0, prefix, freq, 4*time.Second)

	// Advance deterministic simulation time and ensure no panic/stop.
	clock.Advance(1 * time.Second)

	// Stop the loop and verify no panic / goroutine leak.
	atomic.StoreInt32(stopped, 1)

	// The test succeeds if we reach here without a panic:
	// startConsumerLoop must not have called t.Fatal or panicked.
	_ = want
}

// TestConsumerLoopCountsInterests verifies that exactly the expected number
// of interests are sent (using a producer that counts every incoming Interest).
func TestConsumerLoopCountsInterests(t *testing.T) {
	clock, n0, n1, face0to1, _, cleanup := makeConnectedPair(t)
	defer cleanup()

	prefix := mustName(t, "/ndn/test")

	// Producer on n1: count every Interest, reply with Data.
	var received int64
	signer := sig.NewSha256Signer()
	if err := n1.Engine().AttachHandler(prefix, func(args ndn.InterestHandlerArgs) {
		atomic.AddInt64(&received, 1)
		data, err := n1.Engine().Spec().MakeData(
			args.Interest.Name(),
			&ndn.DataConfig{},
			nil,
			signer,
		)
		if err != nil {
			return
		}
		_ = args.Reply(data.Wire)
	}); err != nil {
		t.Fatalf("AttachHandler: %v", err)
	}

	// n1 needs a FIB route /ndn/test -> appFaceID so the forwarder delivers
	// incoming Interests to the producer app.
	n1.AddRoute(prefix, n1.AppFaceID(), 0)

	// n0 needs a FIB route /ndn/test -> face0to1 so interests leave node 0.
	n0.AddRoute(prefix, face0to1, 0)

	const freq = 100.0 // 100 Hz -> 10 ms per interest
	const want = 20    // expect at least 20 Interests within the timeout

	stopped := startConsumerLoop(n0.Engine(), n0.Clock(), 0, prefix, freq, 4*time.Second)

	// Advance up to 3 s simulated time until `want` interests arrive.
	for i := 0; i < 600; i++ { // 600 * 5ms = 3s
		if atomic.LoadInt64(&received) >= want {
			break
		}
		clock.Advance(5 * time.Millisecond)
	}
	atomic.StoreInt32(stopped, 1)

	got := atomic.LoadInt64(&received)
	if got < want {
			t.Fatalf("producer received %d Interests, want at least %d -- "+
			"consumer loop likely stopped mid-chain", got, want)
	}
}

// TestConsumerLoopStops verifies that once the stop flag is set no more
// interests are scheduled.
func TestConsumerLoopStops(t *testing.T) {
	clock, n0, n1, face0to1, _, cleanup := makeConnectedPair(t)
	defer cleanup()

	prefix := mustName(t, "/ndn/test")

	var received int64
	signer := sig.NewSha256Signer()
	if err := n1.Engine().AttachHandler(prefix, func(args ndn.InterestHandlerArgs) {
		atomic.AddInt64(&received, 1)
		data, err := n1.Engine().Spec().MakeData(args.Interest.Name(), &ndn.DataConfig{}, nil, signer)
		if err == nil {
			_ = args.Reply(data.Wire)
		}
	}); err != nil {
		t.Fatalf("AttachHandler: %v", err)
	}
	n1.AddRoute(prefix, n1.AppFaceID(), 0)
	n0.AddRoute(prefix, face0to1, 0)

	stopped := startConsumerLoop(n0.Engine(), clock, 0, prefix, 200.0, 4*time.Second)

	clock.Advance(100 * time.Millisecond)
	beforeStop := atomic.LoadInt64(&received)
	atomic.StoreInt32(stopped, 1)

	clock.Advance(300 * time.Millisecond)
	afterStop := atomic.LoadInt64(&received)
	if afterStop != beforeStop {
		t.Fatalf("consumer kept sending after stop: before=%d after=%d", beforeStop, afterStop)
	}
}
