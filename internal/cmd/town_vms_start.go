package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/gtwrap"
	"github.com/steveyegge/gastown/internal/vmstart"
)

// townVMsStartBound bounds each command the start runs. A Colima start that
// hangs ends the step with an error the report records, instead of holding the
// launchd job forever (gt-czoguq).
const townVMsStartBound = 10 * time.Minute

var townVmsCmd = &cobra.Command{
	Use:   "vms",
	Short: "Bring the Mac's Colima VMs up after a reboot",
	RunE:  requireSubcommand,
}

func init() {
	townVmsCmd.AddCommand(newTownVMsStartCmd(hostTownVMsStartEnv()))
	townCmd.AddCommand(townVmsCmd)
}

// townVMsStartEnv is what the start runs through: the environment it reads the
// pod variable from, the user's home, the runner Start runs each command with,
// and the clock and wait Start uses. A test supplies fakes for all of them, so
// no process starts and no real time passes.
type townVMsStartEnv struct {
	getenv func(string) string
	home   func() (string, error)
	runner vmstart.Runner
	sleep  func(time.Duration)
	now    func() time.Time
}

// hostTownVMsStartEnv is the environment on the Mac.
func hostTownVMsStartEnv() townVMsStartEnv {
	return townVMsStartEnv{
		getenv: os.Getenv,
		home:   os.UserHomeDir,
		runner: execRunner,
		sleep:  time.Sleep,
		now:    time.Now,
	}
}

// newTownVMsStartCmd builds `start` around env, so a test drives it with a fake
// runner and no process is started.
func newTownVMsStartCmd(env townVMsStartEnv) *cobra.Command {
	var home string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start both Colima VMs, start the sandbox containers and repair DNS and the registry",
		Long: `Run the reboot recovery internal/vmstart.Start describes: bring the sandbox VM
up, then the control VM, start the sandbox VM's kind nodes and registry, and
repair the DNS of both VMs and the Mac's registry forward.

Each step the run performed is printed as one line: the VM profile, the command
and "ok" or its error. A failed step is recorded and the steps that can still
run are still tried; the command exits 1 when any step failed.

The command runs on the Mac, where colima, docker and kubectl exist. Inside a
pod it refuses: KUBERNETES_SERVICE_HOST is set there and colima is not.

The registry repair reads the control VM's Lima state under the launchd job
user's home directory; --home names a different one.

Examples:
  gt town vms start
  gt town vms start --home /Users/operator`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runTownVMsStart(c, env, home)
		},
	}
	cmd.Flags().StringVar(&home, "home", "",
		"the home directory whose Lima state the registry repair reads (default: the user's home)")
	return cmd
}

// runTownVMsStart refuses in a pod, resolves home, runs vmstart.Start and
// prints its report; the Start error is returned after the report, so the
// caller exits 1 with the lines that show which step failed.
func runTownVMsStart(cmd *cobra.Command, env townVMsStartEnv, flagHome string) error {
	if env.getenv(podEnv) != "" {
		return fmt.Errorf("gt town vms start runs on the Mac, where colima exists, and this is a pod (%s is set); run it on the Mac instead, where the host command set runs: %s", podEnv, gtwrap.GTHostCommandList())
	}
	home, err := resolveTownVMsStartHome(env, flagHome)
	if err != nil {
		return err
	}
	report, startErr := vmstart.Start(cmd.Context(), env.runner, env.sleep, env.now, home)
	_, writeErr := io.WriteString(cmd.OutOrStdout(), report.String())
	if writeErr != nil {
		writeErr = fmt.Errorf("writing the start report: %w", writeErr)
	}
	return errors.Join(startErr, writeErr)
}

// resolveTownVMsStartHome returns the home directory the registry repair reads:
// the flag when set, the user's home otherwise. A relative path is refused, so
// the repair never reads a tree relative to the launchd job's working directory.
func resolveTownVMsStartHome(env townVMsStartEnv, flagHome string) (string, error) {
	home := flagHome
	if home == "" {
		var err error
		home, err = env.home()
		if err != nil {
			return "", fmt.Errorf("--home is not set and the user's home directory cannot be found, so the registry repair has no Lima state to read: %w", err)
		}
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("--home %q is not an absolute path", home)
	}
	return home, nil
}

// execRunner is the vmstart.Runner on the Mac: it runs one command with a bound
// on the call and returns its standard output. The error is the command's exit
// status, as *exec.ExitError, and never its output, which can hold a secret.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, townVMsStartBound)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}
