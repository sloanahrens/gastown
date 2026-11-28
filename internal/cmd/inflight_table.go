package cmd

import (
	"slices"

	"github.com/spf13/cobra"
)

// inFlightCommands names the commands a daemon roll can cut midway, by cobra
// CommandPath. "gt spec sling" is not listed: the daemon tracks it.
var inFlightCommands = []string{"gt sling"}

// listedInFlight reports whether cmd's CommandPath is a row of inFlightCommands.
func listedInFlight(cmd *cobra.Command) bool {
	return slices.Contains(inFlightCommands, cmd.CommandPath())
}
