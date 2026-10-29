package bdshim

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// exitError is a bd exit status, matched the way *exec.ExitError is. Only a
// real process makes an *exec.ExitError, and a unit test runs none, so a fake
// carries the code instead.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// TestExitResultTurnsHowBDEndedIntoARecording: a clean run and a non-zero exit
// are results the recording keeps; a run that timed out, was killed, or never
// started is an error and records no output.
func TestExitResultTurnsHowBDEndedIntoARecording(t *testing.T) {
	t.Parallel()
	out := []byte("partial\n")
	tests := []struct {
		name     string
		runErr   error
		timedOut bool
		wantExit int
		wantErr  string
	}{
		{"clean run", nil, false, 0, ""},
		{"non-zero exit", exitError(3), false, 3, ""},
		{"timed out", exitError(0), true, 0, "ran past"},
		{"killed by a signal", exitError(-1), false, 0, "killed by a signal"},
		{"never started", errors.New(`exec: "bd": executable file not found in $PATH`), false, 0, "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, exit, err := exitResult(out, tc.runErr, tc.timedOut, runLimit)
			if tc.wantErr == "" {
				if err != nil || exit != tc.wantExit || string(stdout) != string(out) {
					t.Errorf("exitResult = (%q, %d, %v), want bd's output and exit %d", stdout, exit, err, tc.wantExit)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("exitResult error = %v, want one naming %q", err, tc.wantErr)
			}
			if stdout != nil {
				t.Errorf("exitResult returned %q on failure, want no output recorded", stdout)
			}
		})
	}
}

// TestExecRunnerStopsOnceTheBudgetIsSpent: the walk's whole budget, not only
// each run, bounds a recording, so a bd that answers each help slowly cannot
// hold a doctor run command after command. The run is refused before the
// binary is exec'd, which the unfindable path proves.
func TestExecRunnerStopsOnceTheBudgetIsSpent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	run := execRunner("/nonexistent/bd", func() time.Time { return now })
	now = now.Add(recordBudget)
	if _, _, err := run("", []string{"version"}); err == nil || !strings.Contains(err.Error(), "took longer than") {
		t.Fatalf("run after the budget = %v, want the budget named", err)
	}
}
