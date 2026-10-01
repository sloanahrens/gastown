package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/workspace"
)

var formulaOverlayEditCmd = &cobra.Command{
	Use:   "edit <formula>",
	Short: "Edit overlay for a formula",
	Long: `Open the overlay file for a formula in $EDITOR.

Creates <townRoot>/formula-overlays/<formula>.toml if it does not exist.

Examples:
  gt formula overlay edit mol-polecat-work`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaOverlayEdit,
}

func init() {
	formulaOverlayCmd.AddCommand(formulaOverlayEditCmd)
}

func runFormulaOverlayEdit(cmd *cobra.Command, args []string) error {
	formulaName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	path := formula.OverlayPath(townRoot, formulaName)

	// Create directory and file if needed
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		initial := `# Formula overlay for ` + formulaName + `
# Modes: replace, append, skip
#
# [[step-overrides]]
# step_id = "step-name"
# mode = "append"
# description = """
# Additional instructions for this step.
# """
`
		if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
			return fmt.Errorf("creating overlay file: %w", err)
		}
		fmt.Printf("Created new overlay: %s\n", path)
	}

	// Open in editor
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	editorCmd := exec.Command(editor, path)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("running editor: %w", err)
	}

	// Validate after editing
	if _, err := formula.LoadFormulaOverlay(formulaName, townRoot); err != nil {
		return fmt.Errorf("warning: overlay has errors after editing: %w", err)
	}

	fmt.Printf("Overlay updated: %s\n", path)
	fmt.Println("Changes take effect at next 'gt prime'.")
	return nil
}
