package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/update"
)

// updateStatusSHALength is how much of a commit id the status report shows.
const updateStatusSHALength = 8

// updateStatusSeams are the status command's dependencies: the town lookup and
// the clock the heartbeat age is measured against. A test supplies fakes, so
// reading a record needs neither a live town nor the wall clock.
type updateStatusSeams struct {
	FindTownRoot func() (string, error)
	Now          func() time.Time
}

// newUpdateStatusCmd builds `gt update status` over seams.
func newUpdateStatusCmd(seams updateStatusSeams) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Print what the last or current gt update run recorded",
		Long: `Print what the last or current gt update run recorded.

The report is read from the run's state file and leaves it untouched: it takes
no lock, writes nothing and runs nothing. A running run shows its heartbeat age,
so a stale record is visible as one. With --json the recorded state is printed
whole.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateStatus(cmd, seams, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "Print the recorded state as indented JSON")
	return c
}

// runUpdateStatus prints the recorded run. A town with no record is not an
// error; a record that cannot be read is, and is never reported as no run.
func runUpdateStatus(cmd *cobra.Command, seams updateStatusSeams, asJSON bool) error {
	townRoot, err := seams.FindTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	st, err := (&update.Store{Dir: update.Dir(townRoot)}).Read()
	if errors.Is(err, update.ErrNoState) {
		fmt.Fprintln(cmd.OutOrStdout(), "no gt update has run")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the update state: %w", err)
	}
	if asJSON {
		return writeUpdateJSON(cmd.OutOrStdout(), st)
	}
	writeUpdateStatusText(cmd.OutOrStdout(), st, seams.Now())
	return nil
}

// writeUpdateStatusText is the one-line-per-fact report: the run and its
// outcome, when it started with a live heartbeat age or when it ended, the
// build it set out to install, the hold it owns, and every step.
func writeUpdateStatusText(w io.Writer, st update.State, now time.Time) {
	fmt.Fprintf(w, "run %s (%s): %s\n", st.RunID, st.Source, st.Outcome)
	if st.Outcome == update.OutcomeRunning {
		fmt.Fprintf(w, "started %s, last heartbeat %s ago\n",
			formatUpdateStatusTime(st.StartedAt), formatUpdateStatusAge(now.Sub(st.HeartbeatAt)))
	} else {
		fmt.Fprintf(w, "ended %s\n", formatUpdateStatusTime(st.EndedAt))
	}
	fmt.Fprintf(w, "target %s (installed %s)\n",
		shortUpdateStatusSHA(st.Target.SHA), shortUpdateStatusSHA(st.Target.InstalledSHA))
	if st.Hold.Owned {
		fmt.Fprintf(w, "hold: %s\n", st.Hold.Note)
	}
	for _, step := range st.Steps {
		fmt.Fprintf(w, "  %s: %s", step.Name, step.Result)
		if step.Detail != "" {
			fmt.Fprintf(w, " - %s", step.Detail)
		}
		fmt.Fprintln(w)
	}
}

// formatUpdateStatusTime is the report's timestamp form.
func formatUpdateStatusTime(t time.Time) string {
	return t.Format(time.RFC3339)
}

// formatUpdateStatusAge is a heartbeat age at whole-second resolution; a
// heartbeat from the future (a clock that stepped back) reads as just now
// rather than as a negative age.
func formatUpdateStatusAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second).String()
}

// shortUpdateStatusSHA shortens a commit id for the report. State is untrusted
// input, so the cut is by rune: a byte slice could split one and put invalid
// UTF-8 in the output.
func shortUpdateStatusSHA(sha string) string {
	runes := []rune(sha)
	if len(runes) <= updateStatusSHALength {
		return sha
	}
	return string(runes[:updateStatusSHALength])
}
