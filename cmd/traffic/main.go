package main

import (
	"os"

	"github.com/named-data/ndnd/tools"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "ndnd-traffic",
		Short: "NDN traffic generator",
	}
	root.AddCommand(tools.CmdTraffic())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
