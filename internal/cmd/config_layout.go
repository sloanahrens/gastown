package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// gt config migrate and gt config validate (gt-y3pgh.7): the two-file
// layout (internal/config/layout.go).

var configMigrateDryRun bool

var configMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Move the town's config onto two files: mayor/town.json and settings/config.json",
	Long: `Move the town's config from five files onto two, once.

mayor/town.json becomes the machine-owned file: town identity, the Dolt
endpoint, and the rig registry (prefix, parked) and overseer identity that
lived in mayor/rigs.json and mayor/overseer.json. gt verbs write it.

settings/config.json becomes the operator-owned file: agents, thresholds,
secret references, and the daemon patrols and escalation routing that lived
in mayor/daemon.json and settings/escalation.json. Edit it by hand, check it
with 'gt config validate', and restart the daemon to apply it.

Each old file moves verbatim into a section of its new home. The new files
are written and verified by a strict load before the old ones are removed,
all under the files' locks; an interrupted run is finished by running it
again. It also writes the operational.health defaults (the town health
thresholds) into settings/config.json, so every threshold lives in the
operator config; a health block the operator already wrote is kept. It then
records each rig's database name, from the dolt_database
of the rig's bd .beads/metadata.json, in the rig's registry entry;
metadata.json stays, because bd reads it. On a town that is already on two
files and whose registry has every name it refuses.

Examples:
  gt config migrate --dry-run   # show what would move; write nothing
  gt config migrate`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return configMigrate(cwdConfigCmdEnv(), configMigrateDryRun)
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Check that every town config file loads",
	Long: `Load every town config file the way the daemon does and report each one
that does not, one line per file naming the file and the error. It also
reports which layout the town is on (see 'gt config migrate').

Exits non-zero when a file does not load. 'gt daemon start' refuses the same
files, so run this after editing settings/config.json and before restarting.

Examples:
  gt config validate`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return configValidate(cwdConfigCmdEnv())
	},
}

func init() {
	configMigrateCmd.Flags().BoolVar(&configMigrateDryRun, "dry-run", false, "Print what would move; write nothing")
	configCmd.AddCommand(configMigrateCmd)
	configCmd.AddCommand(configValidateCmd)
}

func configMigrate(e configCmdEnv, dryRun bool) error {
	townRoot, err := e.findTown()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	if err := townconfig.Check(townRoot); err != nil {
		return fmt.Errorf("fix the config before migrating it:\n%w", err)
	}
	var steps []config.LayoutStep
	if dryRun {
		steps, err = config.PlanLayoutMigration(townRoot)
	} else {
		steps, err = config.MigrateLayout(townRoot)
	}
	moved := !errors.Is(err, config.ErrAlreadyMigrated)
	if err != nil && moved {
		return err
	}
	// The registry absorbs each rig's database name after the layout move,
	// so a leftover rigs.json still equals its section when the move
	// compares them (gt-y3pgh.11).
	absorbed, absorbErr := townconfig.AbsorbRigDatabases(townRoot, dryRun)
	if !moved && len(absorbed) == 0 && absorbErr == nil {
		return fmt.Errorf("%w; nothing to do", err)
	}
	verb := "Migrated"
	if dryRun {
		verb = "Would migrate"
	}
	if moved {
		fmt.Fprintf(e.out, "%s the town config to two files (%s, %s):\n", verb, config.MachineConfigFile, config.OperatorConfigFile)
		for _, s := range steps {
			fmt.Fprintf(e.out, "  %s\n", s)
		}
	}
	if absorbErr != nil {
		return fmt.Errorf("recording the rigs' database names in the registry: %w", absorbErr)
	}
	if len(absorbed) > 0 {
		verb = "Recorded"
		if dryRun {
			verb = "Would record"
		}
		fmt.Fprintf(e.out, "%s each rig's database in the registry (%s; bd keeps reading metadata.json):\n", verb, config.MachineConfigFile)
		for _, s := range absorbed {
			fmt.Fprintf(e.out, "  %s: dolt_database %q from %s\n", s.Rig, s.Metadata, s.MetadataFile)
		}
	}
	if dryRun {
		return nil
	}
	if err := townconfig.Check(townRoot); err != nil {
		return fmt.Errorf("the migrated config does not load:\n%w", err)
	}
	fmt.Fprintf(e.out, "\nVerify with 'gt config validate' and 'gt doctor' (config-layout, rig-database).\n")
	return nil
}

func configValidate(e configCmdEnv) error {
	townRoot, err := e.findTown()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	if err := townconfig.Check(townRoot); err != nil {
		return err
	}
	r, err := config.DetectLayout(townRoot)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "Town config loads (%s layout)\n", r.Layout)
	switch r.Layout {
	case config.LayoutFiveFile:
		fmt.Fprintf(e.out, "Move it to two files with: gt config migrate --dry-run\n")
	case config.LayoutPartial:
		fmt.Fprintf(e.out, "Old files still on disk: %v; finish with: gt config migrate\n", r.Left)
	}
	return nil
}
