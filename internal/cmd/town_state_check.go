package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/townstate"
	"github.com/steveyegge/gastown/internal/workspace"
)

var townStateCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Run the day-one checks after the volume move, or record the git speed baseline before it",
	Long: `Run the day-one checks for the town-state volume move and print a table: check,
measured, threshold, result. The checks are git speed in each rig the landing
worker reads (against the baseline), the daemon's health probe and gt status
--line, the daemon log from the cut-over, and the first landing since the
cut-over. The thresholds are named in internal/townstate.

The command exits 1 when any check did not pass. A check that could not run
reports "not run: <reason>" and counts as a failure. --json prints the same rows.

Before the move, record the baseline the git speed is judged against:
  gt town state check --record-baseline

A baseline that already exists is not replaced unless --force is given, since a
baseline taken after the move would hide a regression. The checks only read the
town, except --record-baseline, which writes one file under daemon/.

Examples:
  gt town state check --record-baseline
  gt town state check
  gt town state check --json`,
	Args: cobra.NoArgs,
	RunE: runTownStateCheck,
}

func init() {
	townStateCmd.AddCommand(townStateCheckCmd)
	townStateCheckCmd.Flags().Bool("record-baseline", false, "time the git commands in each rig now and write the baseline, instead of checking")
	townStateCheckCmd.Flags().Bool("force", false, "with --record-baseline, replace an existing baseline")
	townStateCheckCmd.Flags().Bool("json", false, "print the rows as JSON")
}

// townStateCheckEnv is what the check command reads from its session: the flags,
// the town root, the runner and clock the checks run through, and the writer
// the table or the baseline line goes to.
type townStateCheckEnv struct {
	record, force, asJSON bool
	findTown              func() (string, error)
	run                   func(ctx context.Context, dir, name string, args ...string) (string, int, error)
	now                   func() time.Time
	out                   io.Writer
}

func runTownStateCheck(cmd *cobra.Command, _ []string) error {
	record, _ := cmd.Flags().GetBool("record-baseline")
	force, _ := cmd.Flags().GetBool("force")
	asJSON, _ := cmd.Flags().GetBool("json")
	return runTownStateCheckIn(cmd.Context(), townStateCheckEnv{
		record:   record,
		force:    force,
		asJSON:   asJSON,
		findTown: workspace.FindFromCwdOrError,
		run:      runDayOneCommand,
		now:      time.Now,
		out:      cmd.OutOrStdout(),
	})
}

// runTownStateCheckIn is what `gt town state check` runs: the baseline record,
// or the day-one checks printed as a table or JSON.
func runTownStateCheckIn(ctx context.Context, env townStateCheckEnv) error {
	if env.force && !env.record {
		return errors.New("--force applies only with --record-baseline")
	}
	town, err := env.findTown()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	dayEnv := townstate.Env{Town: town, GT: "gt", Run: env.run, Now: env.now}
	if env.record {
		path, err := townstate.RecordGitBaseline(ctx, dayEnv, env.force)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.out, "recorded the git speed baseline at %s\n", path)
		return nil
	}
	return reportDayOne(env.out, townstate.DayOneChecks(ctx, dayEnv), env.asJSON)
}

// runDayOneCommand runs one command in dir and returns its standard output and
// exit status. A command that cannot start is an error. A non-zero exit is not:
// the checks read the exit status themselves.
func runDayOneCommand(ctx context.Context, dir, name string, args ...string) (string, int, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	out, err := c.Output()
	return dayOneCommandResult(out, err)
}

// dayOneCommandResult reads a finished command's output and error. An exit
// status is a result the checks read; a command that did not start is an error.
func dayOneCommandResult(out []byte, err error) (string, int, error) {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0, nil
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode(), nil
	default:
		return "", 0, err
	}
}

// reportDayOne prints the rows as a table, or as JSON, and returns an exit
// status of 1 when any row did not pass.
func reportDayOne(w io.Writer, rows []townstate.Row, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fmt.Errorf("encoding the check rows: %w", err)
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tMEASURED\tTHRESHOLD\tRESULT")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Check, r.Measured, r.Threshold, r.Result)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("printing the check table: %w", err)
		}
	}
	if n := townstate.DayOneFailures(rows); n > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("town state check: %d of %d checks did not pass", n, len(rows))}
	}
	return nil
}
