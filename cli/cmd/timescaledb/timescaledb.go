package timescaledb

import (
	"fmt"

	"github.com/spf13/cobra"
	root "github.com/timescale/tobs/cli/cmd"
)

// Cmd represents the timescaledb command
var Cmd = &cobra.Command{
	Use:   "timescaledb",
	Short: "Subcommand for TimescaleDB operations",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		err := root.RootCmd.PersistentPreRunE(cmd, args)
		if err != nil {
			return fmt.Errorf("could not read global flag: %w", err)
		}

		return nil
	},
}

func init() {
	root.RootCmd.AddCommand(Cmd)
}
