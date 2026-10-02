package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
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
  0 - Operation allowed (not in Gas Town agent context, or Mayor)
  2 - Operation BLOCKED (in agent context)

Mol patrol should only be run by the Mayor or by humans from outside
the Gas Town agent tree.`,
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
// stderr. Mayor may patrol — it coordinates agents — and every other Gas Town
// agent context is blocked.
func tapGuardMolPatrolIn(getenv func(string) string, getwd func() (string, error), stderr io.Writer) error {
	if !isGasTownAgentContextIn(getenv, getwd) {
		return nil
	}

	// Allow Mayor to run patrol (it coordinates agents).
	if config.ExtractSimpleRole(getenv(EnvGTRole)) == constants.RoleMayor {
		return nil
	}

	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(stderr, "║  ❌ MOL PATROL BLOCKED                                           ║")
	fmt.Fprintln(stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(stderr, "║  Running 'gt mol patrol' from an agent can kill sibling agents  ║")
	fmt.Fprintln(stderr, "║  or even your own molecule.                                     ║")
	fmt.Fprintln(stderr, "║                                                                  ║")
	fmt.Fprintln(stderr, "║  Only the Mayor or human operators should run mol patrol.       ║")
	fmt.Fprintln(stderr, "║                                                                  ║")
	fmt.Fprintln(stderr, "║  If you need to check molecule status, use:                     ║")
	fmt.Fprintln(stderr, "║    gt mol status    (safe, read-only)                           ║")
	fmt.Fprintln(stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(stderr, "")
	return NewSilentExit(2)
}
