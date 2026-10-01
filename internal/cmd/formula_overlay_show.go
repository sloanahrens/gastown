package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/workspace"
)

var formulaOverlayShowCmd = &cobra.Command{
	Use:   "show <formula>",
	Short: "Show the overlay for a formula",
	Long: `Display the overlay for a formula, read from
<townRoot>/formula-overlays/<formula>.toml.

Examples:
  gt formula overlay show mol-polecat-work`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaOverlayShow,
}

func init() {
	formulaOverlayCmd.AddCommand(formulaOverlayShowCmd)
}

func runFormulaOverlayShow(cmd *cobra.Command, args []string) error {
	formulaName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	path := formula.OverlayPath(townRoot, formulaName)

	// Load and validate overlay
	overlay, err := formula.LoadFormulaOverlay(formulaName, townRoot)
	if err != nil {
		return fmt.Errorf("loading overlay: %w", err)
	}

	if overlay == nil {
		fmt.Printf("No overlay found for formula %q\n", formulaName)
		fmt.Printf("  Checked: %s\n", path)
		fmt.Println("\nUse 'gt formula overlay edit' to create one.")
		return nil
	}

	fmt.Printf("# Overlay: %s\n", formulaName)
	fmt.Printf("# Source: %s\n", path)
	fmt.Printf("# Step overrides: %d\n", len(overlay.StepOverrides))
	fmt.Println()

	// Print the raw TOML file content
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted overlay directory
	if err != nil {
		return fmt.Errorf("reading overlay file: %w", err)
	}
	fmt.Print(string(data))
	if !strings.HasSuffix(string(data), "\n") {
		fmt.Println()
	}

	return nil
}
