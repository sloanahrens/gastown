package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var directiveListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all directive files",
	Long: `List all directive files across town and rig levels.

Shows each directive file with its scope (town or rig) and role.

Examples:
  gt directive list`,
	RunE: runDirectiveList,
}

func init() {
	directiveCmd.AddCommand(directiveListCmd)
}

func runDirectiveList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Build a map of valid roles for quick lookup
	validRoles := make(map[string]bool)
	for _, r := range config.AllRoles() {
		validRoles[r] = true
	}

	type entry struct {
		scope      string // "town" or rig name
		role       string
		path       string
		isUnused   bool   // true if role doesn't match any known agent
	}

	var entries []entry

	// Town-level directives
	townDir := filepath.Join(townRoot, "directives")
	if files, err := os.ReadDir(townDir); err == nil {
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			role := strings.TrimSuffix(f.Name(), ".md")
			path := filepath.Join(townDir, f.Name())
			if fileHasContent(path) {
				entries = append(entries, entry{
					scope:    "town",
					role:     role,
					path:     path,
					isUnused: !validRoles[role],
				})
			}
		}
	}

	// Rig-level directives — scan each rig directory
	rigDirs, err := os.ReadDir(townRoot)
	if err == nil {
		for _, d := range rigDirs {
			if !d.IsDir() {
				continue
			}
			rigName := d.Name()
			// Skip non-rig directories
			rigConfigPath := filepath.Join(townRoot, rigName, "config.json")
			if _, err := os.Stat(rigConfigPath); err != nil {
				continue
			}

			rigDir := filepath.Join(townRoot, rigName, "directives")
			files, err := os.ReadDir(rigDir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
					continue
				}
				role := strings.TrimSuffix(f.Name(), ".md")
				path := filepath.Join(rigDir, f.Name())
				if fileHasContent(path) {
					entries = append(entries, entry{
						scope:    rigName,
						role:     role,
						path:     path,
						isUnused: !validRoles[role],
					})
				}
			}
		}
	}

	if len(entries) == 0 {
		fmt.Println("No directive files found.")
		fmt.Println("\nUse 'gt directive edit <role>' to create one.")
		return nil
	}

	fmt.Printf("Directive files (%d):\n\n", len(entries))
	fmt.Printf("  %-10s %-12s %s\n", "SCOPE", "ROLE", "PATH")
	fmt.Printf("  %-10s %-12s %s\n", "─────", "────", "────")
	for _, e := range entries {
		roleDisplay := e.role
		if e.isUnused {
			roleDisplay += " [UNUSED (no such role)]"
		}
		fmt.Printf("  %-10s %-12s %s\n", e.scope, roleDisplay, e.path)
	}

	// Report unused roles at the end
	// Use a map to deduplicate unused roles
	unusedRoleSet := make(map[string]bool)
	for _, e := range entries {
		if e.isUnused {
			unusedRoleSet[e.role] = true
		}
	}
	if len(unusedRoleSet) > 0 {
		fmt.Println()
		fmt.Println(style.Warning.Render("⚠️  Unused directive files detected:"))
		for role := range unusedRoleSet {
			fmt.Printf("    - %s (no agent role matches this name)\n", role)
		}
		fmt.Println()
		fmt.Println("  These files are never loaded by Gas Town.")
		fmt.Println("  Use 'gt directive show <role>' to verify, or rename/delete unused files.")
	}

	return nil
}
