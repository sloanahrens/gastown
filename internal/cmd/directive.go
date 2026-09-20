package cmd

import (
	"github.com/spf13/cobra"
)

var directiveCmd = &cobra.Command{
	Use:     "directive",
	Aliases: []string{"directives"},
	GroupID: GroupConfig,
	Short:   "Manage role directives",
	Long: `Manage operator-provided role directives.

Directives are markdown files that customize agent behavior per role.
They are injected at prime time and override formula defaults where
they conflict.

Subcommands:
  show    Display the active directive for a role
  edit    Open a directive in $EDITOR (creates if needed)
  list    List all directive files

File layout:
  Town-level: <townRoot>/directives/<role>.md
  Rig-level:  <townRoot>/<rig>/directives/<role>.md
  Every role: <townRoot>/directives/_common.md and the same name in a rig

Resolution: every file that exists is concatenated, broadest first, most
specific last — town _common, rig _common, town <role>, rig <role> — so the
most specific file gets the last word. A file named for no role is never
rendered at all; 'gt directive list' flags it as UNUSED and 'gt doctor' warns.
Put policy that is not role-specific in _common.md rather than copying it into
each role's file.

Examples:
  gt directive show polecat             # Show active polecat directive
  gt directive show witness --rig sky   # Show witness directive for sky rig
  gt directive edit crew                # Edit crew directive (rig-level)
  gt directive edit _common             # Edit policy that every role loads
  gt directive list                     # List all directive files`,
	RunE: requireSubcommand,
}

func init() {
	rootCmd.AddCommand(directiveCmd)
}
