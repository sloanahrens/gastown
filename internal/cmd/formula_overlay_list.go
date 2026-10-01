package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/workspace"
)

var formulaOverlayListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all overlay files",
	Long: `List the formula overlay files in <townRoot>/formula-overlays.

Examples:
  gt formula overlay list`,
	RunE: runFormulaOverlayList,
}

func init() {
	formulaOverlayCmd.AddCommand(formulaOverlayListCmd)
}

func runFormulaOverlayList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	dir := formula.OverlayDir(townRoot)
	var names []string
	if files, err := os.ReadDir(dir); err == nil {
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".toml") {
				continue
			}
			names = append(names, f.Name())
		}
	}

	if len(names) == 0 {
		fmt.Println("No overlay files found.")
		fmt.Println("\nUse 'gt formula overlay edit <formula>' to create one.")
		return nil
	}

	fmt.Printf("Formula overlay files (%d):\n\n", len(names))
	fmt.Printf("  %-30s %s\n", "FORMULA", "PATH")
	fmt.Printf("  %-30s %s\n", "───────", "────")
	for _, name := range names {
		fmt.Printf("  %-30s %s\n", strings.TrimSuffix(name, ".toml"), filepath.Join(dir, name))
	}

	return nil
}
