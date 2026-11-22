package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecatimage"
	"github.com/steveyegge/gastown/internal/townreboot"
	"github.com/steveyegge/gastown/internal/workspace"
)

// townRebootCheckBound bounds each command a check runs, so a Docker or kubectl that hangs
// after a reboot is one failing row and not a command that never returns.
const townRebootCheckBound = 20 * time.Second

var townRebootCheckCmd = &cobra.Command{
	Use:   "reboot-check",
	Short: "Check a rebooted Mac against the operations doc, changing nothing",
	Long: `Print the operations doc's reboot checks in order, each as ok or FAIL with what was
seen and the repair to run. Nothing is changed. The command exits 0 whatever the rows say.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownRebootCheck(cmd.Context(), hostTownRebootCheckEnv(cmd.OutOrStdout()))
	},
}

func init() { townCmd.AddCommand(townRebootCheckCmd) }

// townRebootCheckEnv is what the command runs on: the town, the settings lookup, the check
// lists and the output, so a test supplies each.
type townRebootCheckEnv struct {
	town   func() (string, error)
	getenv func(string) string
	checks func(run townreboot.Runner, registryBase string) []townreboot.Check
	out    io.Writer
}

// hostTownRebootCheckEnv is the environment on the Mac.
func hostTownRebootCheckEnv(out io.Writer) townRebootCheckEnv {
	return townRebootCheckEnv{
		town:   workspace.FindFromCwdOrError,
		getenv: os.Getenv,
		checks: func(run townreboot.Runner, base string) []townreboot.Check { return hostRebootChecks(run, base) },
		out:    out,
	}
}

// hostRebootChecks is the doc's four lists in order, over one runner.
func hostRebootChecks(run townreboot.Runner, registryBase string) []townreboot.Check {
	checks := townreboot.HostChecks(run)
	checks = append(checks, townreboot.NetworkChecks(run)...)
	checks = append(checks, townreboot.ClusterChecks(run)...)
	return append(checks, townreboot.ImageGatewayChecks(run, registryBase)...)
}

// rebootExecRunner runs one command with the check bound and returns its standard output; the
// error is the exit status, never the output, which can hold a secret.
func rebootExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, townRebootCheckBound)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// runTownRebootCheck runs every check and prints the rows. It returns nil whatever they say.
func runTownRebootCheck(ctx context.Context, env townRebootCheckEnv) error {
	town, err := env.town()
	if err != nil {
		return err
	}
	cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
		if key == "POLECAT_IMAGE_TOWN_ROOT" {
			return town
		}
		return env.getenv(key)
	})
	if err != nil {
		return err
	}
	rows := townreboot.RunAll(ctx, env.checks(rebootExecRunner, config.ImageRegistryBase(cfg.Registry)))
	if _, err := fmt.Fprint(env.out, townreboot.Format(rows, false)); err != nil {
		return fmt.Errorf("printing the reboot checks: %w", err)
	}
	return nil
}
