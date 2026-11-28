package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// inFlightLabel returns the CommandPath and args of a listed in-flight command,
// and false for a command listedInFlight does not list.
func inFlightLabel(cmd *cobra.Command, args []string) (string, bool) {
	if cmd == nil || !listedInFlight(cmd) {
		return "", false
	}
	return strings.Join(append([]string{cmd.CommandPath()}, args...), " "), true
}
