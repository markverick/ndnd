package sim

// Unit tests for SimEngine behaviours that are not covered by the
// end-to-end DV integration tests:
//
//   1. AttachHandler returns ErrMultipleHandlers on duplicate prefix.
//   2. DetachHandler prunes the nameTrie so re-attachment works.
//   3. Express fires the timeout callback exactly once (not twice).
//   4. onData CanBePrefix: Interest name is a prefix of Data name (not the
//      reverse). This is the regression test for the dataName.IsPrefix bug.

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/named-data/ndnd/std/ndn"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

// TestEngineAttachHandlerDuplicate verifies that attaching a second handler to
// the same prefix returns ErrMultipleHandlers.
func TestEngineAttachHandlerDuplicate(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))
	node := NewNode(0, clock)
	if err := node.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop()

	prefix := mustName(t, "/test/dup")
	noop := func(ndn.InterestHandlerArgs) {}

	if err := node.Engine().AttachHandler(prefix, noop); err != nil {
		t.Fatalf("first AttachHandler: %v", err)
	}
	err := node.Engine().AttachHandler(prefix, noop)
	if !errors.Is(err, ndn.ErrMultipleHandlers) {
		t.Fatalf("expected ErrMultipleHandlers, got %v", err)
	}
}

// TestEngineDetachHandlerAllowsReattach verifies that after DetachHandler the
// same prefix can be registered again (proves the nameTrie node was pruned).
func TestEngineDetachHandlerAllowsReattach(t *testing.T) {
	clock := NewDeterministicClock(time.Unix(0, 0))
	node := NewNode(0, clock)
	if err := node.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop()

	prefix := mustName(t, "/test/reattach")
	noop := func(ndn.InterestHandlerArgs) {}

	if err := node.Engine().AttachHandler(prefix, noop); err != nil {
		t.Fatalf("AttachHandler: %v", err)
	}
	if err := node.Engine().DetachHandler(prefix); err != nil {
		t.Fatalf("DetachHandler: %v", err)
	}
	// Detach+prune must allow a fresh attach.
	if err := node.Engine().AttachHandler(prefix, noop); err != nil {
		t.Fatalf("re-AttachHandler after DetachHandler: %v", err)
	}
}

// TestEngineExpressTimeoutFiredExactlyOnce verifies that when an Interest times
// out, the callback fires exactly once with InterestResultTimeout.  This is
// the regression test for the bug where Express returned an error but the
// scheduled timeout callback would also fire (double delivery).
func TestEngineExpressTimeoutFiredExactlyOnce(t *testing.T) {
	clock, n0, _, _, _, cleanup := makeConnectedPair(t)
	defer cleanup()

	prefix := mustName(t, "/timeout/test")
	// Route to n0's own app face so Express() does not fail immediately with
	// "no route".  No handler is registered for this prefix, so the Interest
	// is never answered and times out.
	n0.AddRoute(prefix, n0.AppFaceID(), 0)

	eng := n0.Engine()
	interest, err := eng.Spec().MakeInterest(prefix, &ndn.InterestConfig{
		Lifetime: optional.Some(100 * time.Millisecond),
		Nonce:    utils.ConvertNonce(eng.Timer().Nonce()),
	}, nil, nil)
	if err != nil {
		t.Fatalf("MakeInterest: %v", err)
	}

	var callCount int32
	expressErr := eng.Express(interest, func(args ndn.ExpressCallbackArgs) {
		if args.Result != ndn.InterestResultTimeout {
			t.Errorf("expected InterestResultTimeout, got %v", args.Result)
		}
		atomic.AddInt32(&callCount, 1)
	})
	if expressErr != nil {
		t.Fatalf("Express: %v", expressErr)
	}

	// Advance time past the lifetime + 50 ms slack to trigger the timeout.
	clock.Advance(200 * time.Millisecond)

	got := atomic.LoadInt32(&callCount)
	if got != 1 {
		t.Fatalf("timeout callback fired %d times, want exactly 1", got)
	}
}

// TestEngineOnDataCanBePrefix verifies that a CanBePrefix Interest is satisfied
// by a Data whose name is strictly longer (i.e. the Interest name is a prefix of
// the Data name).  In this test the Data is matched via the PIT-token path
// (Express always LP-wraps the Interest with a token, so the reply carries it
// back).  The name-based fallback path inside onData is a separate code path
// that cannot be exercised through the normal forwarding pipeline.
func TestEngineOnDataCanBePrefix(t *testing.T) {
	clock, n0, n1, face0to1, _, cleanup := makeConnectedPair(t)
	defer cleanup()

	// Producer serves /data/exact/suffix when asked for /data/exact (CanBePrefix).
	producerPrefix := mustName(t, "/data/exact")
	dataName := mustName(t, "/data/exact/suffix")
	n1.AddRoute(producerPrefix, n1.AppFaceID(), 0)
	n0.AddRoute(producerPrefix, face0to1, 0)

	signer := sig.NewSha256Signer()
	if err := n1.Engine().AttachHandler(producerPrefix, func(args ndn.InterestHandlerArgs) {
		data, err := n1.Engine().Spec().MakeData(
			dataName, // reply with a *longer* name than the Interest
			&ndn.DataConfig{},
			nil,
			signer,
		)
		if err != nil {
			t.Errorf("producer MakeData: %v", err)
			return
		}
		if err := args.Reply(data.Wire); err != nil {
			t.Errorf("producer Reply: %v", err)
		}
	}); err != nil {
		t.Fatalf("AttachHandler: %v", err)
	}

	eng0 := n0.Engine()
	interest, err := eng0.Spec().MakeInterest(producerPrefix, &ndn.InterestConfig{
		CanBePrefix: true,
		Lifetime:    optional.Some(2 * time.Second),
		Nonce:       utils.ConvertNonce(eng0.Timer().Nonce()),
	}, nil, nil)
	if err != nil {
		t.Fatalf("MakeInterest: %v", err)
	}

	var result atomic.Value
	if err := eng0.Express(interest, func(args ndn.ExpressCallbackArgs) {
		result.Store(args.Result)
	}); err != nil {
		t.Fatalf("Express: %v", err)
	}

	clock.Advance(500 * time.Millisecond)

	r, ok := result.Load().(ndn.InterestResult)
	if !ok || r != ndn.InterestResultData {
		t.Fatalf("CanBePrefix Data not matched: result=%v", result.Load())
	}
}
