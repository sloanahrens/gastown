package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

var directiveShowCmd = &cobra.Command{
	Use:   "show <role>",
	Short: "Show active directive for a role",
	Long: `Display the resolved directive content for a role with source annotation.

Shows which files contribute to the directive (town-level, rig-level, or both).

Examples:
  gt directive show polecat             # Show polecat directive
  gt directive show witness --rig sky   # Show witness directive for sky rig`,
	Args: cobra.ExactArgs(1),
	RunE: runDirectiveShow,
}

var directiveShowRig string

func init() {
	directiveCmd.AddCommand(directiveShowCmd)
	directiveShowCmd.Flags().StringVar(&directiveShowRig, "rig", "", "Rig name (default: auto-detect from cwd)")
}

func runDirectiveShow(cmd *cobra.Command, args []string) error {
	role := args[0]

	// Validate role
	if !isValidRole(role) {
		return fmt.Errorf("unknown role %q — valid names: %s", role, knownRolesHelp())
	}

	townRoot, rigName, err := resolveDirectiveContext(directiveShowRig)
	if err != nil {
		return err
	}

	sources := config.RoleDirectiveSources(role, townRoot, rigName)
	if len(sources) == 0 {
		fmt.Printf("No directive found for role %q\n", role)
		fmt.Printf("  Checked: %s\n", filepath.Join(townRoot, "directives", role+".md"))
		if rigName != "" {
			fmt.Printf("  Checked: %s\n", filepath.Join(townRoot, rigName, "directives", role+".md"))
		}
		fmt.Printf("  Also consulted: the %s.md files at both levels\n", config.SharedDirectiveName)
		fmt.Println("\nUse 'gt directive edit' to create one.")
		return nil
	}

	fmt.Printf("# Directive: %s\n", role)
	for _, path := range sources {
		fmt.Printf("# Source: %s\n", path)
	}
	fmt.Println()
	fmt.Println(config.LoadRoleDirective(role, townRoot, rigName))

	return nil
}

// resolveDirectiveContext finds the town root and rig name for directive commands.
func resolveDirectiveContext(explicitRig string) (townRoot, rigName string, err error) {
	townRoot, err = workspace.FindFromCwdOrError()
	if err != nil {
		return "", "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigName = explicitRig
	if rigName == "" {
		cwd, err := os.Getwd()
		if err == nil {
			rigName = detectRigFromPath(townRoot, cwd)
		}
	}

	return townRoot, rigName, nil
}

// isValidRole reports whether role can name a directive file: a known agent
// role, or the shared name that every role loads.
func isValidRole(role string) bool {
	return role == config.SharedDirectiveName || config.IsKnownRole(role)
}

// knownRolesHelp lists the names a directive file may take.
func knownRolesHelp() string {
	roles := make([]string, 0, len(config.AllRoles())+1)
	roles = append(roles, config.SharedDirectiveName)
	roles = append(roles, config.AllRoles()...)
	return strings.Join(roles, ", ")
}

// fileHasContent returns true if the file exists and has non-whitespace content.
func fileHasContent(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted config
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}
