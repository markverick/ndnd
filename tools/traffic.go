package tools

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/engine"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	"github.com/named-data/ndnd/std/object"
	"github.com/named-data/ndnd/std/object/storage"
	"github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
	"github.com/spf13/cobra"
)

// CmdTraffic returns the parent "traffic" command with producer/consumer subcommands.
func CmdTraffic() *cobra.Command {
	cmd := &cobra.Command{
		GroupID: "tools",
		Use:     "traffic",
		Short:   "NDN traffic generator (producer and consumer)",
	}
	cmd.AddCommand(cmdTrafficProducer())
	cmd.AddCommand(cmdTrafficConsumer())
	return cmd
}

// --- Producer ---

type trafficProducer struct {
	prefix    enc.Name
	payload   int
	freshness int
	expose    bool
	app       ndn.Engine
	sgn       ndn.Signer
}

func cmdTrafficProducer() *cobra.Command {
	tp := &trafficProducer{}
	cmd := &cobra.Command{
		Use:   "producer PREFIX",
		Short: "Start an NDN traffic producer",
		Args:  cobra.ExactArgs(1),
		Run:   tp.run,
	}
	cmd.Flags().IntVar(&tp.payload, "payload", 1024, "content payload size in bytes")
	cmd.Flags().IntVar(&tp.freshness, "freshness", 2000, "FreshnessPeriod in milliseconds")
	cmd.Flags().BoolVar(&tp.expose, "expose", true, "advertise prefix with client origin (for DV)")
	return cmd
}

func (tp *trafficProducer) run(_ *cobra.Command, args []string) {
	prefix, err := enc.NameFromStr(args[0])
	if err != nil {
		log.Fatal(tp, "Invalid prefix", "name", args[0])
		return
	}
	tp.prefix = prefix
	tp.sgn = signer.NewSha256Signer()

	tp.app = engine.NewBasicEngine(engine.NewDefaultFace())
	if err := tp.app.Start(); err != nil {
		log.Fatal(tp, "Unable to start engine", "err", err)
		return
	}
	defer tp.app.Stop()

	if err := tp.app.AttachHandler(prefix, tp.onInterest); err != nil {
		log.Fatal(tp, "Unable to register handler", "err", err)
		return
	}

	cli := object.NewClient(tp.app, storage.NewMemoryStore(), nil)
	if err := cli.Start(); err != nil {
		log.Fatal(tp, "Unable to start object client", "err", err)
		return
	}
	defer cli.Stop()

	cli.AnnouncePrefix(ndn.Announcement{
		Name:   prefix,
		Expose: tp.expose,
	})
	defer cli.WithdrawPrefix(prefix, nil)

	fmt.Printf("traffic producer started: prefix=%s payload=%d freshness=%dms\n",
		prefix, tp.payload, tp.freshness)

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGTERM)
	<-sigchan
}

func (tp *trafficProducer) onInterest(args ndn.InterestHandlerArgs) {
	content := make([]byte, tp.payload)
	freshness := time.Duration(tp.freshness) * time.Millisecond
	data, err := tp.app.Spec().MakeData(
		args.Interest.Name(),
		&ndn.DataConfig{
			ContentType: optional.Some(ndn.ContentTypeBlob),
			Freshness:   optional.Some(freshness),
		},
		enc.Wire{content},
		tp.sgn,
	)
	if err != nil {
		return
	}
	args.Reply(data.Wire)
}

func (tp *trafficProducer) String() string { return "traffic-producer" }

// --- Consumer ---

type trafficConsumer struct {
	app      ndn.Engine
	prefix   enc.Name
	interval int // milliseconds between Interests
	lifetime int // Interest lifetime in milliseconds
	seqNo    uint64
}

func cmdTrafficConsumer() *cobra.Command {
	tc := &trafficConsumer{}
	cmd := &cobra.Command{
		Use:   "consumer PREFIX",
		Short: "Start an NDN traffic consumer",
		Args:  cobra.ExactArgs(1),
		Run:   tc.run,
	}
	cmd.Flags().IntVar(&tc.interval, "interval", 100, "inter-Interest interval in milliseconds (100 = 10 Hz)")
	cmd.Flags().IntVar(&tc.lifetime, "lifetime", 4000, "Interest lifetime in milliseconds")
	return cmd
}

func (tc *trafficConsumer) run(_ *cobra.Command, args []string) {
	prefix, err := enc.NameFromStr(args[0])
	if err != nil {
		log.Fatal(tc, "Invalid prefix", "name", args[0])
		return
	}
	tc.prefix = prefix
	tc.seqNo = 0

	tc.app = engine.NewBasicEngine(engine.NewDefaultFace())
	if err := tc.app.Start(); err != nil {
		log.Fatal(tc, "Unable to start engine", "err", err)
		return
	}
	defer tc.app.Stop()

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGTERM)

	// Send first Interest immediately, then at each interval tick.
	tc.send()
	ticker := time.NewTicker(time.Duration(tc.interval) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tc.seqNo++
			tc.send()
		case <-sigchan:
			return
		}
	}
}

func (tc *trafficConsumer) send() {
	seq := tc.seqNo
	name := tc.prefix.Append(enc.NewGenericComponent(strconv.FormatUint(seq, 10)))

	cfg := &ndn.InterestConfig{
		Lifetime: optional.Some(time.Duration(tc.lifetime) * time.Millisecond),
		Nonce:    utils.ConvertNonce(tc.app.Timer().Nonce()),
	}
	interest, err := tc.app.Spec().MakeInterest(name, cfg, nil, nil)
	if err != nil {
		return
	}
	tc.app.Express(interest, func(args ndn.ExpressCallbackArgs) {
		switch args.Result {
		case ndn.InterestResultData:
			fmt.Printf("received: %s\n", args.Data.Name())
		case ndn.InterestResultTimeout:
			fmt.Fprintf(os.Stderr, "timeout: %s\n", name)
		case ndn.InterestResultNack:
			fmt.Fprintf(os.Stderr, "nack: %s reason=%d\n", name, args.NackReason)
		}
	})
}

func (tc *trafficConsumer) String() string { return "traffic-consumer" }
