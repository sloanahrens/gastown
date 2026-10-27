package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

// directiveLong is the `gt directive` help. The layer directories are built by
// the config package with a placeholder town root and rig name, so the help
// names what the reader resolves and 'gt directive list' prints rather than a
// path the layout no longer uses (gt-oz73l).
var directiveLong = fmt.Sprintf(`Manage operator-provided role directives.

Directives are markdown files that customize agent behavior per role.
They are injected at prime time and override formula defaults where
they conflict.

Subcommands:
  show    Display the active directive for a role
  edit    Open a directive in $EDITOR (creates if needed)
  list    List all directive files

File layout (the absolute paths 'gt directive list' prints):
  Central layer, every rig: %s/<role>.md
  Rig layer, that rig only: %s/<role>.md
  Every role:               _common.md in each layer in place of <role>.md

Legacy fallback: <townRoot>/directives/<role>.md and
<townRoot>/<rig>/directives/<role>.md. A legacy file is read only where the
layer above has no file of that name, and the reader logs a warning naming it.

Resolution: every file that exists is concatenated, broadest first, most
specific last — central _common, rig _common, central <role>, rig <role> — so
the most specific file gets the last word. A file named for no role is never
rendered at all; 'gt directive list' flags it as UNUSED and 'gt doctor' warns.
Put policy that is not role-specific in _common.md rather than copying it into
each role's file.

Examples:
  gt directive show polecat             # Show active polecat directive
  gt directive show witness --rig sky   # Show witness directive for sky rig
  gt directive edit crew                # Edit crew directive (rig-level)
  gt directive edit _common             # Edit policy that every role loads
  gt directive list                     # List all directive files`,
	config.CentralDirectiveDir("<townRoot>"),
	config.RigDirectiveDir("<townRoot>", "<rig>"),
)

var directiveCmd = &cobra.Command{
	Use:     "directive",
	Aliases: []string{"directives"},
	GroupID: GroupConfig,
	Short:   "Manage role directives",
	Long:    directiveLong,
	RunE:    requireSubcommand,
}

func init() {
	rootCmd.AddCommand(directiveCmd)
}
