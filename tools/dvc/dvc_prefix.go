package dvc

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	"github.com/named-data/ndnd/std/types/optional"
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

		if cmd == "announce" || cmd == "withdraw" {
			switch key {
			case "face", "cost":
				// face/cost are intentionally not part of DV prefix announce/withdraw params
				continue
			}
		}

		if cmd == "announce" {
			switch key {
			case "expires":
				expires, err := strconv.ParseUint(val, 10, 64)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Invalid value for expires: %s\n", val)
					os.Exit(9)
					return
				}
				ctrlArgs.ExpirationPeriod = optional.Some(expires)
				continue
			}
		}

		if key == "expires" {
			fmt.Fprintf(os.Stderr, "%s does not accept expires\n", cmd)
			os.Exit(9)
			return
		}

		t.convPetArg(&ctrlArgs, key, val)
	}

	if cmd == "announce" {
		expiresMs, ok := ctrlArgs.ExpirationPeriod.Get()
		if !ok || expiresMs == 0 {
			fmt.Fprintln(os.Stderr, "prefix-announce requires expires=<milliseconds> and expires must be > 0")
			os.Exit(9)
			return
		}
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
