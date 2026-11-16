package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/workspace"
)

// PreflightResult is one pre-roll check's outcome: the check's name and the
// error it failed with, nil when it passed.
type PreflightResult struct {
	Name string
	Err  error
}

// The names the preflight reports for its checks.
const (
	preflightNameTownConfig  = "town-config"
	preflightNameBDHandshake = "bd-handshake"
)

// preflightChecks are the pre-roll checks, as fields so a unit test replaces
// them and runs neither a town config load nor bd.
type preflightChecks struct {
	// townConfig reports every town config file that exists but does not
	// load (daemon.CheckTownConfig).
	townConfig func(townRoot string) error
	// bdHandshake is the startup handshake for the town's store
	// (bdTownHandshake).
	bdHandshake func(ctx context.Context, townRoot string) error
}

// townPreflightChecks are the checks runPreflightChecks runs in production.
var townPreflightChecks = preflightChecks{
	townConfig:  daemon.CheckTownConfig,
	bdHandshake: bdTownHandshake,
}

// runPreflightChecks runs every pre-roll check against townRoot and returns
// one result per check, in run order: a failing check never stops the ones
// after it, so an operator sees every problem in one pass.
func runPreflightChecks(ctx context.Context, townRoot string) []PreflightResult {
	return runPreflightChecksWith(ctx, townRoot, townPreflightChecks)
}

// runPreflightChecksWith is runPreflightChecks with the checks passed in, so a
// test drives every result with no town on disk and no bd. The rows below are
// the order the checks run in, and a check with no function wired is reported
// as failed rather than passing silently.
func runPreflightChecksWith(ctx context.Context, townRoot string, checks preflightChecks) []PreflightResult {
	ordered := []struct {
		name  string
		wired bool
		run   func() error
	}{
		{preflightNameTownConfig, checks.townConfig != nil, func() error { return checks.townConfig(townRoot) }},
		{preflightNameBDHandshake, checks.bdHandshake != nil, func() error { return checks.bdHandshake(ctx, townRoot) }},
	}
	results := make([]PreflightResult, 0, len(ordered))
	for _, check := range ordered {
		var err error
		if check.wired {
			err = check.run()
		} else {
			err = fmt.Errorf("the preflight has no %s check wired", check.name)
		}
		results = append(results, PreflightResult{Name: check.name, Err: err})
	}
	return results
}

// newDaemonPreflightCmd builds `gt daemon preflight`. The checks and the
// town-root lookup are parameters so a test runs neither a town config load
// nor bd.
func newDaemonPreflightCmd(checks preflightChecks, findTownRoot func() (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Run the daemon's pre-roll checks and print one line per result",
		Long: `Run the pre-roll checks against the town and print one line per result:
"ok  <name>" for a check that passed and "FAIL <name>: <error>" for one that
failed. Every check runs, so one failure never hides another.

Exit status: 0 when every check passed, 1 when any check failed. A check the
command has no function for is reported as failed, never as ok.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			townRoot, err := findTownRoot()
			if err != nil {
				return err
			}
			err = printPreflightResults(cmd.OutOrStdout(), runPreflightChecksWith(cmd.Context(), townRoot, checks))
			if _, silent := IsSilentExit(err); silent {
				// The exit status is the answer; cobra must not print it as an
				// error over the per-check lines.
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
			}
			return err
		},
	}
}

var daemonPreflightCmd = newDaemonPreflightCmd(townPreflightChecks, workspace.FindFromCwdOrError)

func init() {
	daemonCmd.AddCommand(daemonPreflightCmd)
}

// printPreflightResults prints one line per result, "ok  <name>" or
// "FAIL <name>: <error>", and returns a silent exit 1 when any result failed.
// A failed write returns that error instead, so a check whose line was lost is
// never reported as a clean run.
func printPreflightResults(w io.Writer, results []PreflightResult) error {
	failed := 0
	for _, result := range results {
		var err error
		if result.Err != nil {
			failed++
			_, err = fmt.Fprintf(w, "FAIL %s: %v\n", result.Name, result.Err)
		} else {
			_, err = fmt.Fprintf(w, "ok  %s\n", result.Name)
		}
		if err != nil {
			return fmt.Errorf("printing the %s result: %w", result.Name, err)
		}
	}
	if failed > 0 {
		return NewSilentExit(1)
	}
	return nil
}
