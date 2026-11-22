package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/gtwrap"
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
seen and the repair to run. Nothing is changed. Text output exits 0 whatever the rows say.
--json prints the same rows as one JSON array and exits 1 when any row is not OK.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownRebootCheckCmd(cmd)
	},
}

func init() {
	townRebootCheckCmd.Flags().Bool("json", false, "print the rows as one JSON array")
	townCmd.AddCommand(townRebootCheckCmd)
}

// runTownRebootCheckCmd reads --json and runs the check on the Mac. A command without the flag
// is an error before any check runs, not a text run.
func runTownRebootCheckCmd(cmd *cobra.Command) error {
	asJSON, err := townRebootCheckJSONFlag(cmd)
	if err != nil {
		return err
	}
	env := hostTownRebootCheckEnv(cmd.OutOrStdout())
	env.asJSON = asJSON
	return runTownRebootCheck(cmd.Context(), env)
}

// townRebootCheckJSONFlag reads --json from the command.
func townRebootCheckJSONFlag(cmd *cobra.Command) (bool, error) {
	asJSON, err := cmd.Flags().GetBool("json")
	if err != nil {
		return false, fmt.Errorf("reading --json: %w", err)
	}
	return asJSON, nil
}

// townRebootCheckEnv is what the command runs on: the town, the settings lookup, the check
// lists, the output and the output form, so a test supplies each.
type townRebootCheckEnv struct {
	town   func() (string, error)
	getenv func(string) string
	checks func(run townreboot.Runner, registryBase string) []townreboot.Check
	out    io.Writer
	asJSON bool
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

// runTownRebootCheck runs every check and prints the rows. Text output returns nil whatever the
// rows say; JSON output returns exit 1 when any row is not OK, after printing the array. In a pod
// it refuses before the runner is built: the checks read the Mac's VMs and processes.
func runTownRebootCheck(ctx context.Context, env townRebootCheckEnv) error {
	if env.getenv(podEnv) != "" {
		return fmt.Errorf("gt town reboot-check reads the Mac's VMs and processes, and this is a pod (%s is set); run it on the Mac instead, where the host command set runs: %s", podEnv, gtwrap.GTHostCommandList())
	}
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
	if env.asJSON {
		return printRebootRowsJSON(env.out, rows)
	}
	if _, err := fmt.Fprint(env.out, townreboot.Format(rows, false)); err != nil {
		return fmt.Errorf("printing the reboot checks: %w", err)
	}
	return nil
}

// printRebootRowsJSON prints the rows as one JSON array, then returns exit 1 when any row is not
// OK. The array is printed first, so a script reads every row, its fix and the status together.
func printRebootRowsJSON(out io.Writer, rows []townreboot.Row) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return fmt.Errorf("printing the reboot checks: %w", err)
	}
	failed := 0
	for _, r := range rows {
		if !r.OK {
			failed++
		}
	}
	if failed > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("gt town reboot-check: %d of %d checks failed", failed, len(rows))}
	}
	return nil
}
