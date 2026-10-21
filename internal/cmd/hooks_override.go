package cmd

import (
	"context"
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

// hooksOverrideEnv is the home and editor one invocation of gt hooks override
// runs against.
type hooksOverrideEnv struct {
	home   hooks.Home
	editor func(string) error
}

// hooksOverrideEnvKey carries the home and editor a caller pinned for one
// invocation. Tests use withHooksOverrideEnv to run the command's own RunE
// against a sandbox home and a recording opener; production never sets it, so
// the environment supplies both (gt-arsyc).
type hooksOverrideEnvKey struct{}

// withHooksOverrideEnv pins home and editor as the pair runHooksOverride uses.
func withHooksOverrideEnv(ctx context.Context, home hooks.Home, editor func(string) error) context.Context {
	return context.WithValue(ctx, hooksOverrideEnvKey{}, hooksOverrideEnv{home: home, editor: editor})
}

func runHooksOverride(cmd *cobra.Command, args []string) error {
	home, editor := hooks.EnvHome(), openInEditor
	if env, ok := cmd.Context().Value(hooksOverrideEnvKey{}).(hooksOverrideEnv); ok {
		home, editor = env.home, env.editor
	}
	return hooksOverrideWith(home, editor, args)
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
	path, err := home.OverrideEditPath(target)
	if err != nil {
		return fmt.Errorf("locating override config for %q: %w", target, err)
	}
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
