package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/jobs"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/workspace"
)

// jobsExitUsage is the exit status for an argument or flag the command does
// not have.
const jobsExitUsage = 2

// jobsDeps is what `gt jobs` reads from the host: the town whose list it
// prints, the backup root the daemon writes the Dolt backup into, and the
// clock that judges ages and due times. A test supplies all three.
type jobsDeps struct {
	findTownRoot func() (string, error)
	backupRoot   func() (string, error)
	now          func() time.Time
}

// realJobsDeps is jobsDeps on this host.
func realJobsDeps() jobsDeps {
	return jobsDeps{
		findTownRoot: workspace.FindFromCwdOrError,
		backupRoot:   doltbackup.DefaultRoot,
		now:          time.Now,
	}
}

// newJobsCmd builds `gt jobs`; a test builds a fresh one so its flag state is
// its own.
func newJobsCmd(d jobsDeps) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:     "jobs",
		GroupID: GroupDiag,
		Short:   "Print the town's maintenance job list",
		Long: `Print every maintenance job the daemon runs: one row per daemon step, the
nightly Dolt backup, the Dolt gc and each node's disk reading, with its last
run, its cadence and its state.

The list is read-only. gt jobs --json is what the dashboard reads, so that
array is the dashboard's source. A row whose inputs cannot be read prints
unknown, never ok.

Exit status: 0 when every row is readable and no run failed, 1 when a row is
unknown or its last run failed, 2 for a bad argument.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return &ExitCodeError{Code: jobsExitUsage, Err: fmt.Errorf("jobs takes no arguments, got %q", strings.Join(args, " "))}
			}
			return nil
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return jobsCommand(cmd.OutOrStdout(), d, asJSON)
		},
	}
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &ExitCodeError{Code: jobsExitUsage, Err: err}
	})
	c.Flags().BoolVar(&asJSON, "json", false, "Print the list as a JSON array")
	return c
}

var jobsCmd = newJobsCmd(realJobsDeps())

func init() {
	rootCmd.AddCommand(jobsCmd)
}

// jobsCommand prints the town's job list and maps it onto the exit status. The
// backup root is resolved the way the daemon resolves it, because the row that
// reads the nightly backup is only as real as the root it is given; a root the
// host cannot name is passed on empty, so that row reads unknown rather than
// never run.
func jobsCommand(w io.Writer, d jobsDeps, asJSON bool) error {
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	backupRoot, backupErr := d.backupRoot()
	now := d.now()
	list, err := jobs.List(townRoot, jobs.Options{BackupRoot: backupRoot}, now)
	if err != nil {
		return fmt.Errorf("listing the maintenance jobs: %w", err)
	}
	if asJSON {
		err = writeJobsJSON(w, list)
	} else {
		err = writeJobsText(w, list, now)
	}
	if err != nil {
		return err
	}
	return jobsStatus(list, backupErr)
}

// writeJobsText prints one row per job.
func writeJobsText(w io.Writer, list []jobs.Job, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLAST RUN\tCADENCE\tSTATE\tOUTCOME")
	for _, j := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			j.Name, jobsAgeCell(j.LastRun, now), jobsCadenceCell(j.Cadence), j.State, jobsOutcomeCell(j.Outcome))
	}
	return tw.Flush()
}

// writeJobsJSON prints the list as the JSON array the dashboard parses.
func writeJobsJSON(w io.Writer, list []jobs.Job) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(list)
}

// jobsAgeCell is how long ago a job last ran, "-" when no run is recorded.
func jobsAgeCell(lastRun *time.Time, now time.Time) string {
	if lastRun == nil {
		return "-"
	}
	return townhealth.Short(now.Sub(*lastRun)) + " ago"
}

// jobsCadenceCell is a job's cadence, "-" when it has none.
func jobsCadenceCell(cadence time.Duration) string {
	if cadence <= 0 {
		return "-"
	}
	return townhealth.Short(cadence)
}

// jobsOutcomeCell is a job's last outcome, "-" when no run recorded one.
func jobsOutcomeCell(outcome jobs.Outcome) string {
	if outcome == "" {
		return "-"
	}
	return string(outcome)
}

// jobsStatus is the exit status a printed list leaves: non-zero when a row is
// unknown or its last run failed, so a broken input never reads as a clean
// list. A backup root the host could not resolve is named, since that is what
// left the backup row unknown.
func jobsStatus(list []jobs.Job, backupErr error) error {
	if backupErr != nil {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("resolving the Dolt backup root: %w", backupErr)}
	}
	var unknown, failed int
	for _, j := range list {
		switch {
		case j.State == jobs.StateUnknown:
			unknown++
		case j.Outcome == jobs.OutcomeFailed:
			failed++
		}
	}
	if unknown == 0 && failed == 0 {
		return nil
	}
	return &ExitCodeError{Code: 1, Err: fmt.Errorf("%d job(s) unknown, %d failed", unknown, failed)}
}
