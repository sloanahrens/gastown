package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/townstatus"
	"github.com/steveyegge/gastown/internal/workspace"
)

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
	lines, v, _ := townstatus.HealthView(townRoot, time.Now())
	fmt.Fprintln(os.Stdout, lines[0])
	if code := v.ExitCode(); code != 0 {
		return NewSilentExit(code)
	}
	return nil
}
