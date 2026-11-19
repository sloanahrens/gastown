package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doctor"
)

var townDeployerAdminCheckCmd = &cobra.Command{
	Use:   "admin-check",
	Short: "Check the host kubeconfig for an embedded admin credential",
	Long: `Check the kubeconfig the operator's admin entry is written in for a context
that reaches the control cluster with an embedded credential. The sealed-admin
convention removes the admin entry, so the deployer's kubeconfig references its
key by path; this command reports each context and user that still carries one.

The kubeconfig is --kubeconfig when given, else the file $KUBECONFIG names,
else $HOME/.kube/config. Empty entries in $KUBECONFIG are skipped, and a
KUBECONFIG naming more than one file is refused rather than guessed at. The
command prints the
context and user names only, never a credential, and exits 1 on a finding and
on a kubeconfig it could not read.

The kubeconfig is the host's, so the command refuses inside a pod. It reads
only: it never writes the file or removes an entry.

Example:
  gt town deployer admin-check --kubeconfig ~/.kube/config`,
	Args: cobra.NoArgs,
	RunE: runTownDeployerAdminCheck,
}

func init() {
	townDeployerCmd.AddCommand(townDeployerAdminCheckCmd)
	townDeployerAdminCheckCmd.Flags().String("kubeconfig", "", "the kubeconfig to check (default $KUBECONFIG else $HOME/.kube/config)")
}

// townDeployerAdminCheckEnv is what `gt town deployer admin-check` reads from
// its session: the --kubeconfig flag, the environment the path falls back to,
// and where the result lines go.
type townDeployerAdminCheckEnv struct {
	kubeconfig string
	getenv     func(string) string
	out        io.Writer
}

func runTownDeployerAdminCheck(cmd *cobra.Command, _ []string) error {
	kubeconfig, err := cmd.Flags().GetString("kubeconfig")
	if err != nil {
		return fmt.Errorf("reading --kubeconfig: %w", err)
	}
	return runTownDeployerAdminCheckIn(townDeployerAdminCheckEnv{
		kubeconfig: kubeconfig,
		getenv:     os.Getenv,
		out:        cmd.OutOrStdout(),
	})
}

// runTownDeployerAdminCheckIn is what `gt town deployer admin-check` runs.
func runTownDeployerAdminCheckIn(env townDeployerAdminCheckEnv) error {
	if err := adminKubeconfigPodRefusal("gt town deployer admin-check", env.getenv); err != nil {
		return err
	}
	path, err := resolveAdminKubeconfigPath(env.kubeconfig, env.getenv)
	if err != nil {
		return err
	}

	result := doctor.NewAdminKubeconfigCheck(path).Run(&doctor.CheckContext{})
	// The message and the details name a context and a user, never a
	// credential value, so every line is safe to print.
	if _, err := fmt.Fprintln(env.out, strings.Join(append([]string{result.Message}, result.Details...), "\n")); err != nil {
		return fmt.Errorf("printing the admin-kubeconfig result: %w", err)
	}
	switch result.Status {
	case doctor.StatusOK:
		return nil
	case doctor.StatusSkipped:
		// A file that was not read proves nothing, so it must not exit 0.
		return fmt.Errorf("the admin-kubeconfig check did not run: %s", result.Message)
	default:
		return fmt.Errorf("the admin-kubeconfig check reported: %s", result.Message)
	}
}

// adminKubeconfigPodRefusal is the refusal both the command and the doctor
// check give inside a pod: the kubeconfig they read is the Mac's, so a pod would
// read a path that is not the operator's file. who names the caller in the text.
func adminKubeconfigPodRefusal(who string, getenv func(string) string) error {
	if getenv(podEnv) != "" {
		return fmt.Errorf("%s runs on the Mac, where the kubeconfig is, so it refuses inside a pod, where %s is set", who, podEnv)
	}
	return nil
}

// resolveAdminKubeconfigPath returns the one kubeconfig the admin-kubeconfig
// check reads: the flag when it is set, else the file $KUBECONFIG names, else
// $HOME/.kube/config. Empty entries of $KUBECONFIG are skipped, so an empty
// entry never becomes an empty path, and a list with no entry left reads as
// unset. The check asks about the one file the operator's admin entry lives in,
// so a list of two or more files names no file it can decide on.
func resolveAdminKubeconfigPath(flagValue string, getenv func(string) string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	var entries []string
	for _, entry := range filepath.SplitList(getenv("KUBECONFIG")) {
		// SplitList keeps an empty entry, and "" would reach the check as a
		// path naming no file.
		if strings.TrimSpace(entry) != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) > 1 {
		return "", fmt.Errorf("KUBECONFIG names %d files, so no single file is checked: pass --kubeconfig with the one file the admin entry lives in", len(entries))
	}
	if len(entries) == 1 {
		return entries[0], nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("--kubeconfig is not set, KUBECONFIG names no file and HOME is not set, so the kubeconfig cannot be found: pass --kubeconfig")
	}
	return filepath.Join(home, ".kube", "config"), nil
}
