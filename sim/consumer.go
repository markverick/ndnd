package sim

// Consumer loop extracted from cgo_export.go so it can be tested without
// the CGo/ns-3 build environment.  cgo_export.go calls startConsumerLoop.

import (
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

// startConsumerLoop schedules periodic Interest sends for name at the given
// frequency.  Scheduling is driven by clock (Ns3Clock in simulation,
// WallClock in tests).
//
// Returns a pointer to the atomic stop flag.  Set it to 1 to stop the loop.
// The caller must store this pointer so NdndSimStopConsumer can reach it.
func startConsumerLoop(
	engine ndn.Engine,
	clock Clock,
	nodeID uint32,
	name enc.Name,
	freq float64,
	lifetime time.Duration,
) *int32 {
	if freq <= 0 {
		freq = 1.0
	}
	interval := time.Duration(float64(time.Second) / freq)

	var stopped int32
	var seqNo uint64

	var sendNext func()
	sendNext = func() {
		if atomic.LoadInt32(&stopped) != 0 {
			return
		}

		seq := seqNo
		seqNo++

		// Build name: prefix + seqNo as GenericNameComponent
		iName := make(enc.Name, len(name)+1)
		copy(iName, name)
		iName[len(name)] = enc.NewGenericComponent(strconv.FormatUint(seq, 10))

		cfg := &ndn.InterestConfig{
			Lifetime: optional.Some(lifetime),
			Nonce:    utils.ConvertNonce(engine.Timer().Nonce()),
		}
		interest, err := engine.Spec().MakeInterest(iName, cfg, nil, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"[ndndSIM] FATAL: node %d seq %d MakeInterest failed: %v — consumer stopped\n",
				nodeID, seq, err)
			return // stops the chain; logged so the caller can see it
		}
		if err := engine.Express(interest, func(ndn.ExpressCallbackArgs) {}); err != nil {
			fmt.Fprintf(os.Stderr,
				"[ndndSIM] node %d seq %d Express error: %v\n", nodeID, seq, err)
			// Express failure is not fatal — the Interest was just dropped.
			// Keep scheduling so the next attempt can succeed once the
			// forwarder/FIB is ready.
		}

		clock.Schedule(interval, sendNext)
	}

	clock.Schedule(interval, sendNext)
	return &stopped
}
