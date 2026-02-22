package tools

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/engine"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	"github.com/named-data/ndnd/std/object"
	"github.com/named-data/ndnd/std/object/storage"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/security/keychain"
	"github.com/named-data/ndnd/std/security/trust_schema"
	"github.com/spf13/cobra"
)

type PutChunks struct {
	expose bool
}

const (
	envClientKeyChainUri  = "NDND_CLIENT_KEYCHAIN"
	envClientTrustSchema  = "NDND_CLIENT_TRUST_SCHEMA"
	envClientTrustAnchors = "NDND_CLIENT_TRUST_ANCHORS"
)

// (AI GENERATED DESCRIPTION): Creates a Cobra command that publishes data chunks read from standard input under a specified name prefix, optionally registering the prefix with the client origin.
func CmdPutChunks() *cobra.Command {
	pc := PutChunks{}

	cmd := &cobra.Command{
		GroupID: "tools",
		Use:     "put PREFIX",
		Short:   "Publish data under a name prefix",
		Long: `Publish data under a name prefix.
This tool expects data from the standard input.`,
		Args:    cobra.ExactArgs(1),
		Example: `  ndnd put /my/example/data < data.bin`,
		Run:     pc.run,
	}

	cmd.Flags().BoolVar(&pc.expose, "expose", false, "Expose prefix through routing")
	return cmd
}

// (AI GENERATED DESCRIPTION): Returns the literal string `"put"` to identify the `PutChunks` operation (implementing the fmt.Stringer interface).
func (pc *PutChunks) String() string {
	return "put"
}

func (pc *PutChunks) loadTrustConfig(store ndn.Store) *sec.TrustConfig {
	keyChainUri := strings.TrimSpace(os.Getenv(envClientKeyChainUri))
	schemaPath := strings.TrimSpace(os.Getenv(envClientTrustSchema))
	anchorsRaw := strings.TrimSpace(os.Getenv(envClientTrustAnchors))

	if keyChainUri == "" && schemaPath == "" && anchorsRaw == "" {
		return nil
	}
	if keyChainUri == "" || schemaPath == "" || anchorsRaw == "" {
		log.Fatal(pc, "Incomplete client trust configuration in environment",
			"required", []string{envClientKeyChainUri, envClientTrustSchema, envClientTrustAnchors})
		return nil
	}

	kc, err := keychain.NewKeyChain(keyChainUri, store)
	if err != nil {
		log.Fatal(pc, "Unable to open client keychain", "uri", keyChainUri, "err", err)
		return nil
	}

	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		log.Fatal(pc, "Unable to read client trust schema", "path", schemaPath, "err", err)
		return nil
	}
	schema, err := trust_schema.NewLvsSchema(schemaBytes)
	if err != nil {
		log.Fatal(pc, "Unable to parse client trust schema", "path", schemaPath, "err", err)
		return nil
	}

	anchors := make([]enc.Name, 0)
	for _, anchor := range strings.Split(anchorsRaw, ",") {
		anchor = strings.TrimSpace(anchor)
		if anchor == "" {
			continue
		}
		name, err := enc.NameFromStr(anchor)
		if err != nil {
			log.Fatal(pc, "Invalid client trust anchor", "anchor", anchor, "err", err)
			return nil
		}
		anchors = append(anchors, name)
	}
	if len(anchors) == 0 {
		log.Fatal(pc, "No valid trust anchors found", "env", envClientTrustAnchors)
		return nil
	}

	trust, err := sec.NewTrustConfig(kc, schema, anchors)
	if err != nil {
		log.Fatal(pc, "Unable to create client trust configuration", "err", err)
		return nil
	}
	trust.UseDataNameFwHint = true
	return trust
}

// (AI GENERATED DESCRIPTION): Ingests data from standard input, produces a named Data object in the NDN engine, announces its prefix, and blocks until a termination signal is received.
func (pc *PutChunks) run(_ *cobra.Command, args []string) {
	name, err := enc.NameFromStr(args[0])
	if err != nil {
		log.Fatal(pc, "Invalid object name", "name", args[0])
		return
	}

	// start face and engine
	app := engine.NewBasicEngine(engine.NewDefaultFace())
	err = app.Start()
	if err != nil {
		log.Fatal(pc, "Unable to start engine", "err", err)
		return
	}
	defer app.Stop()

	// start object client
	store := storage.NewMemoryStore()
	trust := pc.loadTrustConfig(store)
	cli := object.NewClient(app, store, trust)
	err = cli.Start()
	if err != nil {
		log.Fatal(pc, "Unable to start object client", "err", err)
		return
	}
	defer cli.Stop()

	// read from stdin till eof
	var content enc.Wire
	for {
		buf := make([]byte, 8192)
		n, err := io.ReadFull(os.Stdin, buf)
		if n > 0 {
			content = append(content, buf[:n])
		}
		if err != nil {
			break
		}
	}

	// produce object
	vname, err := cli.Produce(ndn.ProduceArgs{
		Name:    name.WithVersion(enc.VersionUnixMicro),
		Content: content,
	})
	if err != nil {
		log.Fatal(pc, "Unable to produce object", "err", err)
		return
	}

	content = nil // gc
	log.Info(pc, "Object produced", "name", vname)

	// announce the prefix
	cli.AnnouncePrefix(ndn.Announcement{
		Name:   name,
		Expose: pc.expose,
	})
	defer cli.WithdrawPrefix(name, nil)

	// wait forever
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGTERM)
	receivedSig := <-sigchan
	log.Info(nil, "Received signal - exiting", "signal", receivedSig)
}
