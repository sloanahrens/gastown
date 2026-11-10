package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/townstatus"
	"github.com/steveyegge/gastown/internal/workspace"
)

// runStatusHealthLine is gt status --line: the one line, and the verdict
// as the exit code (0 green, 1 degraded, 2 red, 3 unknown). With ready the
// exit code is 0 when the town is ready for traffic (see statusReadyAnswer).
func runStatusHealthLine(cmd *cobra.Command, ready bool) error {
	// The exit code is the answer; cobra must not print it as an error.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		fmt.Printf("UNKNOWN not in a Gas Town workspace: %v\n", err)
		return NewSilentExit(townhealth.VerdictUnknown.ExitCode())
	}
	return statusHealthLineExit(cmd, townRoot, ready, time.Now(), townstatus.HealthView)
}

// healthViewFunc reads the health view of townRoot at now; townstatus.HealthView
// is the one the command uses.
type healthViewFunc func(townRoot string, now time.Time) ([]string, townhealth.Verdict, *townhealth.Report)

// statusHealthLineExit prints the health line of townRoot's report, read by
// view, and returns the exit: the verdict's code, or with ready the readiness
// answer.
func statusHealthLineExit(cmd *cobra.Command, townRoot string, ready bool, now time.Time, view healthViewFunc) error {
	lines, v, rep := view(townRoot, now)
	fmt.Fprintln(cmd.OutOrStdout(), lines[0])
	if ready {
		ok, reason := statusReadyAnswer(townRoot, rep, now)
		if ok {
			return nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "not ready: %s\n", reason)
		// Not ready implies a non-green verdict, so v's code is non-zero.
		return NewSilentExit(v.ExitCode())
	}
	if code := v.ExitCode(); code != 0 {
		return NewSilentExit(code)
	}
	return nil
}

// statusReadyAnswer is the --ready answer for the report HealthView read: not
// ready when nothing was readable, otherwise townhealth.Ready against the
// stale-after the health line itself uses (townstatus.HealthStaleAfter).
func statusReadyAnswer(townRoot string, rep *townhealth.Report, now time.Time) (bool, string) {
	if rep == nil {
		return false, "no health report"
	}
	return townhealth.Ready(*rep, now, townstatus.HealthStaleAfter(townRoot))
}
