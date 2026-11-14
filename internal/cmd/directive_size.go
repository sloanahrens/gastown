package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

// directiveSizeDefaultRole is the layer the directive size budget is set for,
// so 'gt directive size' with no argument answers the question the budget asks.
const directiveSizeDefaultRole = "polecat"

// directiveSizeTarget is the budget's 12 KB target
// (docs/design/rework-program.md). The committed ceiling moves down toward it
// each time a rule is cut or two are merged.
const directiveSizeTarget = 12 * 1024

var directiveSizeCmd = &cobra.Command{
	Use:   "size [role]",
	Short: "Print the rendered size of a directive in bytes",
	Long: `Print the bytes the injected directive for a role costs, in total and per source.

Each source is one file that renders for the role, and its figure is the bytes
the seat prompt carries for its heading and text, so the sections add up to the
total. The role defaults to polecat, the layer the size budget is set for.

Examples:
  gt directive size                    # Polecat directive, town plus rig layer
  gt directive size crew --rig sky     # Crew directive for the sky rig`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDirectiveSize,
}

var directiveSizeRig string

func init() {
	directiveCmd.AddCommand(directiveSizeCmd)
	directiveSizeCmd.Flags().StringVar(&directiveSizeRig, "rig", "", "Rig name (default: auto-detect from cwd)")
}

func runDirectiveSize(cmd *cobra.Command, args []string) error {
	role := directiveSizeDefaultRole
	if len(args) == 1 {
		role = args[0]
	}
	townRoot, rigName, err := resolveDirectiveContext(directiveSizeRig)
	if err != nil {
		return err
	}
	return runDirectiveSizeIn(cmd.OutOrStdout(), role, townRoot, rigName)
}

// runDirectiveSizeIn prints the directive size for a role in a known town. The
// role is refused before any file is read: a name that is no directive file
// stem renders only the shared file, and that would report a healthy size for a
// directive that does not exist.
func runDirectiveSizeIn(w io.Writer, role, townRoot, rigName string) error {
	if !isValidRole(role) {
		return fmt.Errorf("unknown role %q — valid names: %s", role, knownRolesHelp())
	}
	size, err := readDirectiveSize(role, townRoot, rigName)
	if err != nil {
		return err
	}
	renderDirectiveSize(w, size)
	return nil
}

// directiveSizeSection is one directive file that renders for a role and the
// bytes its heading and text add to the injected prompt.
type directiveSizeSection struct {
	Scope string
	Name  string
	Bytes int
}

// directiveSize is the injected size of a role's directive: the bytes of the
// whole section string, and its sources in resolution order counted one by one.
type directiveSize struct {
	Role     string
	Bytes    int
	Sections []directiveSizeSection
}

// readDirectiveSize measures a role's directive through renderDirectiveSection,
// the function that writes the seat prompt, so headings and the Changelog strip
// are counted. The total is measured on the whole render rather than summed, so
// a separator added between sources cannot leave it short. A source that exists
// and cannot be read is an error, as it is for the prompt: a size that silently
// drops a policy is smaller than the one installed.
func readDirectiveSize(role, townRoot, rigName string) (directiveSize, error) {
	directives, err := config.ReadRoleDirectives(role, townRoot, rigName)
	if err != nil {
		return directiveSize{}, err
	}
	size := directiveSize{
		Role:     role,
		Bytes:    len(renderDirectiveSection(directives)),
		Sections: make([]directiveSizeSection, 0, len(directives)),
	}
	for _, d := range directives {
		size.Sections = append(size.Sections, directiveSizeSection{
			Scope: d.Scope,
			Name:  d.Name,
			Bytes: len(renderDirectiveSection([]config.RoleDirective{d})),
		})
	}
	return size, nil
}

// renderDirectiveSize prints the total, then one line per source. A role with
// no directive files says so: a bare zero reads as a failed measurement.
func renderDirectiveSize(w io.Writer, size directiveSize) {
	fmt.Fprintf(w, "%s directive: %d bytes (target %d)\n", size.Role, size.Bytes, directiveSizeTarget)
	if len(size.Sections) == 0 {
		fmt.Fprintln(w, "  no directive files")
		return
	}
	for _, section := range size.Sections {
		fmt.Fprintf(w, "  %-20s %6d bytes\n", section.Scope+" "+section.Name, section.Bytes)
	}
}
