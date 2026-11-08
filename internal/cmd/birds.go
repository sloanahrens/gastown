package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/bird"
	"github.com/steveyegge/gastown/internal/birdreports"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	birdsCmd.GroupID = GroupAgents
	rootCmd.AddCommand(birdsCmd)
	birdsCmd.AddCommand(birdsDueCmd)
}

var birdsCmd = &cobra.Command{
	Use:   "birds",
	Short: "Check the birds' daily reports",
	Long: `Check the birds' daily reports.

A bird publishes its daily report with gt report publish. The internal/reportdir
package comment states where a report is read from, and the older location it
falls back to; this command asks that package rather than restating the paths.
gt birds due prints the late ones.`,
	RunE: requireSubcommand,
}

var birdsDueCmd = &cobra.Command{
	Use:   "due",
	Short: "Print the late daily reports and the request to send each reporter",
	Long: `Print every reporter whose daily report is missing or more than 26 hours old,
then the request to send that reporter, with <NAME> replaced by its name and
<RUNTIME_DIR> by the runtime directory this command reads.

The reporters are the nine birds and the overseer; each report is read where
internal/reportdir resolves it, so no location is restated here. The request
text is docs/birds/daily-request.md. The command sends nothing: the caller sends
each request with SendMessage. It exits 1 when a report is due and 0 when all
are fresh, so a prompt can branch on it.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBirdsDueCommand(cmd, args, workspace.FindFromCwdOrError, time.Now)
	},
}

// runBirdsDueCommand is gt birds due's body. The town lookup and the clock are
// parameters so a test drives the failure paths without a town on this machine.
func runBirdsDueCommand(cmd *cobra.Command, args []string, findTownRoot func() (string, error), now func() time.Time) error {
	if len(args) != 0 {
		return birdFailure(&bird.UsageError{Arg: "gt birds due", Wants: "no arguments"})
	}
	townRoot, err := findTownRoot()
	if err != nil {
		return err
	}
	err = runBirdsDue(cmd.OutOrStdout(), filepath.Join(townRoot, ".runtime"), now(), birdreports.DefaultReporters())
	if _, ok := IsSilentExit(err); ok {
		// The exit status is the answer; cobra must not print it as an error.
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
	}
	return err
}

// runBirdsDue prints one line per due reporter, then the request for it. It
// returns a silent exit 1 when any report is due, so the caller's prompt can
// branch on the status without parsing the text. A failed write returns that
// error instead, so a truncated list is never reported as a clean run.
func runBirdsDue(w io.Writer, runtimeDir string, now time.Time, reporters []string) error {
	reports := birdreports.ReadAll(runtimeDir, reporters, now, birdreports.MaxAge)
	due := 0
	for _, report := range reports {
		if !report.Due() {
			continue
		}
		if due > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		due++
		if _, err := fmt.Fprintln(w, birdDueLine(report)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		request := birdreports.Request(report.Reporter, runtimeDir)
		if !strings.HasSuffix(request, "\n") {
			request += "\n"
		}
		if _, err := fmt.Fprint(w, request); err != nil {
			return err
		}
	}
	if due == 0 {
		if _, err := fmt.Fprintf(w, "all %d daily reports are fresh\n", len(reports)); err != nil {
			return err
		}
		return nil
	}
	return NewSilentExit(1)
}

// birdDueLine is the line a due reporter gets before its request: its name, its
// state, and its age when there is one to state.
func birdDueLine(report birdreports.Report) string {
	switch report.Status {
	case birdreports.Stale:
		return fmt.Sprintf("%s: stale, %s old", report.Reporter, birdAge(report.Age))
	default:
		return fmt.Sprintf("%s: %s (%s)", report.Reporter, report.Status, report.Note)
	}
}

// birdAge spells a stale report's age as whole hours and minutes.
func birdAge(age time.Duration) string {
	return fmt.Sprintf("%dh%02dm", int(age.Hours()), int(age.Minutes())%60)
}
