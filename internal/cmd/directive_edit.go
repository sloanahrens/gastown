package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

var directiveEditCmd = &cobra.Command{
	Use:   "edit <role>",
	Short: "Edit directive for a role",
	Long: `Open the directive file for a role in $EDITOR.

Creates the directory and file if they do not exist. By default, edits the
rig-level directive (if a rig is detected) or the town-level directive.

Use --town to explicitly edit the town-level directive. The town-level directive
is the central layer in the gastown repo's checkout: an edit there is a change to
that checkout, and it lands through the pipeline rather than taking effect at once.

Examples:
  gt directive edit polecat             # Edit rig-level polecat directive
  gt directive edit crew --rig sky      # Edit sky rig crew directive
  gt directive edit witness --town      # Edit town-level witness directive`,
	Args: cobra.ExactArgs(1),
	RunE: runDirectiveEdit,
}

var (
	directiveEditRig  string
	directiveEditTown bool
)

func init() {
	directiveCmd.AddCommand(directiveEditCmd)
	directiveEditCmd.Flags().StringVar(&directiveEditRig, "rig", "", "Rig name (default: auto-detect from cwd)")
	directiveEditCmd.Flags().BoolVar(&directiveEditTown, "town", false, "Edit town-level directive instead of rig-level")
}

func runDirectiveEdit(cmd *cobra.Command, args []string) error {
	role := args[0]

	if !isValidRole(role) {
		return fmt.Errorf("unknown role %q — valid names: %s", role, knownRolesHelp())
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	cwd, _ := os.Getwd() // an unreadable cwd only turns off rig auto-detection

	return runDirectiveEditIn(cmd.OutOrStdout(), townRoot, cwd, directiveEditRig, role, directiveEditTown, openInEditor)
}

// runDirectiveEditIn is runDirectiveEdit in a known town. The rig is resolved
// (and an explicit one validated) before anything is created, so a bad --rig
// leaves the disk untouched. openEditor is the seam tests stub.
func runDirectiveEditIn(out io.Writer, townRoot, cwd, explicitRig, role string, townLevel bool, openEditor func(path string) error) error {
	rigName, err := resolveDirectiveRigIn(townRoot, explicitRig, cwd)
	if err != nil {
		return err
	}

	path := config.DirectiveEditPath(role, townRoot, rigName, townLevel)

	// Create directory and file if needed
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Create with a helpful header comment
		appliesTo := "role: " + role
		if role == config.SharedDirectiveName {
			appliesTo = "every role"
		}
		initial := fmt.Sprintf("<!-- Directive for %s -->\n<!-- This content is injected at prime time. -->\n\n", appliesTo)
		if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
			return fmt.Errorf("creating directive file: %w", err)
		}
		fmt.Fprintf(out, "Created new directive: %s\n", path)
	}

	if err := openEditor(path); err != nil {
		return err
	}

	fmt.Fprintf(out, "Directive updated: %s\n", path)
	fmt.Fprintln(out, "Changes take effect at next 'gt prime'.")
	return nil
}
