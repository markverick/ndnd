package dvc

import (
	"fmt"
	"os"
	"strings"

	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	"github.com/spf13/cobra"
)

// ExecPrefixCmd executes a prefix management command against the DV prefix egress state.
func (t *Tool) ExecPrefixCmd(_ *cobra.Command, cmd string, args []string, defaults []string) {
	t.Start()
	defer t.Stop()

	ctrlArgs := mgmt.ControlArgs{}

	for _, arg := range append(defaults, args...) {
		kv := strings.SplitN(arg, "=", 2)
		if len(kv) != 2 {
			fmt.Fprintf(os.Stderr, "Invalid argument: %s (should be key=value)\n", arg)
			os.Exit(9)
			return
		}

		key, val := t.preprocessPetArg(kv[0], kv[1])
		t.convPetArg(&ctrlArgs, key, val)
	}

	res, err := t.execDvMgmtCmd("prefix", cmd, &ctrlArgs)
	if res == nil {
		fmt.Fprintf(os.Stderr, "Error executing command: %+v\n", err)
		os.Exit(1)
		return
	}

	t.printCtrlResponse(res)
	if err != nil {
		os.Exit(1)
	}
}
