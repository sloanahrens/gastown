package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/power"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	powerDryRun     bool
	powerTimeout    int
	powerWithDolt   bool
	powerYes        bool
	powerForceCloud bool
)

var powerCmd = &cobra.Command{
	Use:     "power",
	GroupID: GroupServices,
	Short:   "Manage the town power states: status, full, standby, off",
	Long: `Manage the town's power states.

  full     Dolt, the Forgejo stack, the daemon and the watchers up; no ESTOP,
           no hold. Dispatch runs.
  standby  the same parts up, the hold set, the polecats drained, then the
           ESTOP set: no dispatch, no scheduled slings, no restarts.
  off      standby's drain, then the daemon, the watchers and the Forgejo
           stack stopped. Dolt stays up unless --dolt.

status is read-only and prints every part plus the derived state.

Before changing a state read docs/power-runbook.md: it covers the drain, the
hooks, the exit codes (0 ok, 2 usage, 3 drain timed out, 4 a part did not come
up) and the reboot-to-standby job.`,
	RunE: requireSubcommand,
}

var powerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report every part and the derived power state",
	Long:  "Report every part of the town and the state they add up to. This command is read-only.",
	Args:  cobra.NoArgs,
	RunE:  runPowerCommand(power.Status, true),
}

var powerFullCmd = &cobra.Command{
	Use:   "full",
	Short: "Bring every part up and clear the ESTOP and the hold",
	Long: `Bring Docker, the Forgejo stack, Dolt, the watchers and the daemon up,
then clear the ESTOP and the hold. Nothing dispatches until the hold is
cleared, and the hold is left in place if a part does not come up.

With --dry-run it prints the commands and waits without running them.`,
	Args: cobra.NoArgs,
	RunE: runPowerCommand(power.Full, true),
}

var powerStandbyCmd = &cobra.Command{
	Use:   "standby",
	Short: "Drain the town, then hold dispatch and restarts",
	Long: `Set the hold first, bring every part up, wait for the landing queues, the
polecats and any Dolt gc to finish, then set the ESTOP. The hold precedes the
ESTOP because the ESTOP also refuses the restart a draining polecat may need.

A drain that times out names what still runs and exits 3; nothing is killed.
With --dry-run it prints every command and the probes without changing
anything.`,
	Args: cobra.NoArgs,
	RunE: runPowerCommand(power.Standby, true),
}

var powerOffCmd = &cobra.Command{
	Use:   "off",
	Short: "Drain, then stop the daemon, the watchers and Forgejo",
	Long: `Run standby's drain, then stop the daemon, the watchers and the Forgejo
stack. Dolt stays up unless --dolt. The Forgejo stack stays stopped across a
reboot; 'gt power full' starts it again.

off needs --yes. While a cloud release-bot LaunchDaemon is installed it also
needs --force-cloud: the bot and patrol poll Forgejo, so the stack being
stopped pauses deploys and makes those jobs log read errors. With --dry-run it
prints every command and changes nothing.`,
	Args: cobra.NoArgs,
	RunE: runPowerCommand(power.Off, true),
}

var powerBootHoldCmd = &cobra.Command{
	Use:   "boot-hold",
	Short: "Write the hold so a reboot lands in standby",
	Long:  "Write the hold and record a boot standby. This is the login job internal/templates/launchd/com.gastown.power-boot.plist runs.",
	Args:  cobra.NoArgs,
	RunE:  runPowerCommand(power.BootHold, false),
}

func init() {
	// A rejected flag is a usage error like any other (exit 2), the code the
	// operator script reported.
	powerCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &ExitCodeError{Code: power.ExitUsage, Err: err}
	})
	powerFullCmd.Flags().BoolVar(&powerDryRun, "dry-run", false, "Print what would run and change nothing")
	powerStandbyCmd.Flags().BoolVar(&powerDryRun, "dry-run", false, "Print what would run and change nothing")
	powerStandbyCmd.Flags().IntVar(&powerTimeout, "timeout", 900, "Drain timeout in seconds")
	powerOffCmd.Flags().BoolVar(&powerDryRun, "dry-run", false, "Print what would run and change nothing")
	powerOffCmd.Flags().IntVar(&powerTimeout, "timeout", 900, "Drain timeout in seconds")
	powerOffCmd.Flags().BoolVar(&powerWithDolt, "dolt", false, "Also stop the Dolt server")
	powerOffCmd.Flags().BoolVar(&powerYes, "yes", false, "Confirm stopping the daemon, the watchers and Forgejo")
	powerOffCmd.Flags().BoolVar(&powerForceCloud, "force-cloud", false, "Stop Forgejo even while a cloud release bot is installed")
	powerBootHoldCmd.Flags().BoolVar(&powerDryRun, "dry-run", false, "Print what would run and change nothing")

	powerCmd.AddCommand(powerStatusCmd)
	powerCmd.AddCommand(powerFullCmd)
	powerCmd.AddCommand(powerStandbyCmd)
	powerCmd.AddCommand(powerOffCmd)
	powerCmd.AddCommand(powerBootHoldCmd)
	rootCmd.AddCommand(powerCmd)
}

// runPowerCommand resolves the host into power.Options and maps the state's
// coded exit onto the process exit. needsForgejo is false for a state that
// never touches the Forgejo stack, so a settings file that cannot be read does
// not stop it.
func runPowerCommand(fn func(context.Context, power.Options) error, needsForgejo bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		o, err := powerOptions(needsForgejo)
		if err != nil {
			return powerExit(err)
		}
		return powerExit(fn(cmd.Context(), o))
	}
}

func powerExit(err error) error {
	if err == nil {
		return nil
	}
	var pe *power.ExitError
	if errors.As(err, &pe) {
		return &ExitCodeError{Code: pe.Code, Err: pe.Err}
	}
	return err
}

func powerOptions(needsForgejo bool) (power.Options, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return power.Options{}, &power.ExitError{Code: power.ExitUsage,
			Err: fmt.Errorf("not in a Gas Town workspace: %w", err)}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return power.Options{}, &power.ExitError{Code: power.ExitUsage,
			Err: fmt.Errorf("finding the home directory: %w", err)}
	}
	forgejoDir := ""
	if needsForgejo {
		forgejoDir, err = config.ForgejoDir(townRoot)
		if err != nil {
			return power.Options{}, &power.ExitError{Code: power.ExitUsage,
				Err: fmt.Errorf("reading forgejo.dir from settings/config.json: %w", err)}
		}
	}
	if powerTimeout < 0 {
		return power.Options{}, &power.ExitError{Code: power.ExitUsage,
			Err: fmt.Errorf("--timeout needs seconds")}
	}
	gtCommand, err := os.Executable()
	if err != nil {
		gtCommand = cli.Name()
	}
	o := power.Options{
		TownRoot:   townRoot,
		HomeDir:    home,
		ForgejoDir: forgejoDir,
		GTCommand:  gtCommand,
		DryRun:     powerDryRun,
		Timeout:    time.Duration(powerTimeout) * time.Second,
		WithDolt:   powerWithDolt,
		Yes:        powerYes,
		ForceCloud: powerForceCloud,
	}
	power.RealSeams(&o)
	return o, nil
}
