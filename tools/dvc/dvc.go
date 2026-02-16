package dvc

import (
	"fmt"
	"os"

	"github.com/named-data/ndnd/std/engine"
	"github.com/named-data/ndnd/std/ndn"
	"github.com/spf13/cobra"
)

// (AI GENERATED DESCRIPTION): Builds and returns the Cobra command set for querying router status, creating a new active neighbor link, and destroying an existing neighbor link.
func Cmds() []*cobra.Command {
	t := Tool{}

	return []*cobra.Command{{
		Use:   "status",
		Short: "Print general status of the router",
		Args:  cobra.NoArgs,
		Run:   t.RunDvStatus,
	}, {
		Use:   "prefix-announce [params]",
		Short: "Announce a prefix in the DV prefix egress state",
		Args:  cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t.ExecPrefixCmd(cmd, "announce", args, []string{"cost=0"})
		},
	}, {
		Use:   "prefix-withdraw [params]",
		Short: "Withdraw a prefix from the DV prefix egress state",
		Args:  cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t.ExecPrefixCmd(cmd, "withdraw", args, []string{})
		},
	}, {
		Use:   "pet-add-nexthop [params]",
		Short: "Add a nexthop to the PID PET",
		Args:  cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t.ExecPetCmd(cmd, "register", args, []string{"cost=0"})
		},
	}, {
		Use:   "pet-remove-nexthop [params]",
		Short: "Remove a nexthop from the PID PET",
		Args:  cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, args []string) {
			t.ExecPetCmd(cmd, "unregister", args, []string{})
		},
	}, {
		Use:   "link-create NEIGHBOR-URI",
		Short: "Create a new active neighbor link",
		Args:  cobra.ExactArgs(1),
		Run:   t.RunDvLinkCreate,
	}, {
		Use:   "link-destroy NEIGHBOR-URI",
		Short: "Destroy an active neighbor link",
		Args:  cobra.ExactArgs(1),
		Run:   t.RunDvLinkDestroy,
	}}
}

type Tool struct {
	engine ndn.Engine
}

// (AI GENERATED DESCRIPTION): Initializes the Tool’s engine with a default face and starts it, terminating the program if the engine fails to start.
func (t *Tool) Start() {
	t.engine = engine.NewBasicEngine(engine.NewDefaultFace())

	err := t.engine.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to start engine: %+v\n", err)
		os.Exit(1)
		return
	}
}

// (AI GENERATED DESCRIPTION): Stops the Tool’s engine, terminating its operation.
func (t *Tool) Stop() {
	t.engine.Stop()
}
