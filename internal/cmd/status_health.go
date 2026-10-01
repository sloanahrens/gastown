package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/workspace"
)

// townHealthView reads the health file the daemon writes every tick
// (gt-s3rec.2) and renders it at now: the one line, then one line per
// field. A file that is missing, unreadable or stale is UNKNOWN; a broken
// operational.health block falls back to the default stale age (the
// report's config field already says so).
func townHealthView(townRoot string, now time.Time) (lines []string, v townhealth.Verdict, rep *townhealth.Report) {
	_, stale, err := config.LoadOperationalConfig(townRoot).GetHealthSettings().Resolve()
	if err != nil {
		stale = townhealth.DefaultStaleAfter
	}
	r, err := townhealth.Read(townRoot)
	if err != nil {
		return []string{fmt.Sprintf("UNKNOWN no health report: %v", err)}, townhealth.VerdictUnknown, nil
	}
	return townhealth.Lines(r, now, stale), townhealth.Effective(r, now, stale), &r
}

// runStatusHealthLine is gt status --line: the one line, and the verdict
// as the exit code (0 green, 1 degraded, 2 red, 3 unknown).
func runStatusHealthLine(cmd *cobra.Command) error {
	// The exit code is the answer; cobra must not print it as an error.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		fmt.Printf("UNKNOWN not in a Gas Town workspace: %v\n", err)
		return NewSilentExit(townhealth.VerdictUnknown.ExitCode())
	}
	lines, v, _ := townHealthView(townRoot, time.Now())
	fmt.Fprintln(os.Stdout, lines[0])
	if code := v.ExitCode(); code != 0 {
		return NewSilentExit(code)
	}
	return nil
}
