package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
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
		for _, p := range config.DirectivePaths(role, townRoot, rigName) {
			if filepath.Base(p) == role+".md" {
				fmt.Printf("  Checked: %s\n", p)
			}
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

	cwd, _ := os.Getwd() // an unreadable cwd only turns off rig auto-detection
	rigName, err = resolveDirectiveRigIn(townRoot, explicitRig, cwd)
	if err != nil {
		return "", "", err
	}
	return townRoot, rigName, nil
}

// resolveDirectiveRigIn is the rig half of resolveDirectiveContext in a known
// town: an explicit rig must pass validateDirectiveRig, otherwise the rig is
// the one cwd sits in, if any.
func resolveDirectiveRigIn(townRoot, explicitRig, cwd string) (string, error) {
	if explicitRig != "" {
		if err := validateDirectiveRig(townRoot, explicitRig); err != nil {
			return "", err
		}
		return explicitRig, nil
	}
	if cwd == "" {
		return "", nil
	}
	return detectRigFromPath(townRoot, cwd), nil
}

// validateDirectiveRig rejects an explicit --rig that cannot name a directory
// the loaders read. The name is joined into a path, so a separator or ".."
// would write outside the town, and a name missing from the registry (a typo)
// would write a file no agent ever loads.
func validateDirectiveRig(townRoot, rigName string) error {
	if strings.ContainsAny(rigName, `/\`) || strings.Contains(rigName, "..") {
		return fmt.Errorf("invalid rig name %q: must be a registered rig name, not a path", rigName)
	}

	rigsConfig, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading rigs config: %w", err)
	}
	if _, ok := rigsConfig.Rigs[rigName]; !ok {
		return fmt.Errorf("rig %q is not a registered rig of this town; run 'gt rig list' to see the rigs", rigName)
	}
	return nil
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
