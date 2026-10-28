package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

// The layer directories the help prints, with the placeholder town root and rig
// name an operator reads, built by the config package so the help names what the
// reader resolves rather than a path the layout no longer uses (gt-oz73l).
var (
	helpCentralDir = config.CentralDirectiveDir("<townRoot>")
	helpRigDir     = config.RigDirectiveDir("<townRoot>", "<rig>")
)

// helpLegacyFallback is the help's legacy fallback paragraph. Each legacy
// directory is named with the layer directory it stands in for, since the reader
// falls back within the same layer and the central layer has no layer above it
// (gt-v1ln5).
var helpLegacyFallback = fmt.Sprintf(`Legacy fallback: a legacy file stands in for the new file of its own layer,
and is read only where that new file does not exist.
  <townRoot>/directives/<role>.md, the central layer's predecessor, stands
    in for %s/<role>.md.
  <townRoot>/<rig>/directives/<role>.md, the rig layer's predecessor, stands
    in for %s/<role>.md.
The reader logs a warning naming each legacy file it reads.`, helpCentralDir, helpRigDir)

// directiveLong is the `gt directive` help.
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

%s

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
	helpCentralDir,
	helpRigDir,
	helpLegacyFallback,
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
