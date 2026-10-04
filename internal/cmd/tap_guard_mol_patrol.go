package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var tapGuardMolPatrolCmd = &cobra.Command{
	Use:   "mol-patrol",
	Short: "Block mol patrol in agent contexts",
	Long: `Block mol patrol operations that could disrupt running agents.

'gt mol patrol' terminates stale molecules. Running it from within an
agent session risks killing sibling agents or even the caller's own
molecule.

This guard blocks:
  - gt mol patrol (when called from within a Gas Town agent)

Exit codes:
  0 - Operation allowed (not in Gas Town agent context)
  2 - Operation BLOCKED (in agent context)

Mol patrol should only be run by human operators from outside the Gas Town
agent tree.`,
	RunE: runTapGuardMolPatrol,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardMolPatrolCmd)
}

func runTapGuardMolPatrol(cmd *cobra.Command, args []string) error {
	return tapGuardMolPatrolIn(os.Getenv, os.Getwd, os.Stderr)
}

// tapGuardMolPatrolIn is runTapGuardMolPatrol reading the environment through
// getenv, the working directory through getwd and the block notice written to
// stderr. Every Gas Town agent context is blocked; only a human operator
// running from outside the agent tree may patrol.
func tapGuardMolPatrolIn(getenv func(string) string, getwd func() (string, error), stderr io.Writer) error {
	if !isGasTownAgentContextIn(getenv, getwd) {
		return nil
	}

	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(stderr, "║  ❌ MOL PATROL BLOCKED                                           ║")
	fmt.Fprintln(stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(stderr, "║  Running 'gt mol patrol' from an agent can kill sibling agents  ║")
	fmt.Fprintln(stderr, "║  or even your own molecule.                                     ║")
	fmt.Fprintln(stderr, "║                                                                  ║")
	fmt.Fprintln(stderr, "║  Only human operators should run mol patrol.                    ║")
	fmt.Fprintln(stderr, "║                                                                  ║")
	fmt.Fprintln(stderr, "║  If you need to check molecule status, use:                     ║")
	fmt.Fprintln(stderr, "║    gt mol status    (safe, read-only)                           ║")
	fmt.Fprintln(stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(stderr, "")
	return NewSilentExit(2)
}
