package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/hooks"
)

var hooksOverrideCmd = &cobra.Command{
	Use:   "override <target>",
	Short: "Edit overrides for a role or rig",
	Long: `Edit hook overrides for a specific role or rig+role combination.

Valid targets:
  Role-level:  crew, polecats (alias: polecat)
  Rig+role:    gastown/crew, beads/polecats, sky/polecats, etc.

Overrides are merged on top of the base config during sync.
Hooks with the same matcher replace the base hook entirely.

Override files are stored in ~/.gt/hooks-overrides/<target>.json.

Examples:
  gt hooks override crew              # Edit crew role overrides
  gt hooks override gastown/crew      # Edit gastown rig crew overrides
  gt hooks override crew --show       # Print current override config`,
	Args: cobra.ExactArgs(1),
	RunE: runHooksOverride,
}

var hooksOverrideShow bool

func init() {
	hooksCmd.AddCommand(hooksOverrideCmd)
	hooksOverrideCmd.Flags().BoolVar(&hooksOverrideShow, "show", false, "Print current override config to stdout")
}

func runHooksOverride(cmd *cobra.Command, args []string) error {
	return hooksOverrideWith(hooks.EnvHome(), openInEditor, args)
}

// hooksOverrideWith edits the override for args[0] in home, opening the chosen
// file with openEditor. Home and editor are parameters so a test can name a
// sandbox home and a recording opener (gt-arz8h).
func hooksOverrideWith(home hooks.Home, openEditor func(string) error, args []string) error {
	normalized, ok := hooks.NormalizeTarget(args[0])
	if !ok {
		return fmt.Errorf("invalid target %q; valid targets are roles (crew, polecats; alias: polecat) or rig/role (gastown/crew, beads/polecats, etc.)", args[0])
	}
	target := normalized

	cfg, err := home.LoadOverride(target)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("loading override config for %q: %w", target, err)
		}
		// File doesn't exist yet - create empty
		cfg = &hooks.HooksConfig{}
		if err := home.SaveOverride(target, cfg); err != nil {
			return fmt.Errorf("creating override config: %w", err)
		}
		fmt.Printf("Created empty override config for %s\n", target)
	}

	if hooksOverrideShow {
		data, err := hooks.MarshalConfig(cfg)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	// Open the file the read cascade loaded, not the primary path: with
	// GT_HOME set, saving the primary path would shadow the override in
	// effect (gt-arz8h).
	path := home.OverrideEditPath(target)
	if err := openEditor(path); err != nil {
		return err
	}

	// Validate after editing
	if _, err := home.LoadOverride(target); err != nil {
		return fmt.Errorf("warning: override config has errors after editing: %w", err)
	}

	fmt.Printf("Override config for %s updated. Run 'gt hooks sync' to propagate changes.\n", target)
	return nil
}
