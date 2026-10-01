package cmd

import (
	"github.com/spf13/cobra"
)

var formulaOverlayCmd = &cobra.Command{
	Use:   "overlay",
	Short: "Manage formula overlays",
	Long: `Manage formula overlays — per-formula step overrides.

Overlays are TOML files that customize formula steps via replace, append,
or skip modes. They are applied at prime time when formula steps are displayed.

Subcommands:
  show    Display the overlay for a formula
  edit    Open an overlay in $EDITOR (creates if needed)
  list    List all overlay files

There is one overlay directory: <townRoot>/formula-overlays/<formula>.toml.
A rig-level <townRoot>/<rig>/formula-overlays directory is not read.

Examples:
  gt formula overlay show mol-polecat-work
  gt formula overlay edit mol-polecat-work
  gt formula overlay list`,
	RunE: requireSubcommand,
}

func init() {
	formulaCmd.AddCommand(formulaOverlayCmd)
}
