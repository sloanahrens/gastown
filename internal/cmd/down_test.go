package cmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// A session that fails to stop is counted, and runDownWith clears allOK on that
// count, so gt down exits non-zero instead of printing "All services stopped".
// The unit tier starts no tmux (docs/testing.md) and runDownWith opens one
// before any session phase, so this drives the tally stopAllCrew and
// stopAllPolecats return their failure count from.
func TestRunDownSessionStopErrorExitsNonZero(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	stopped, failed := reportStopResults(&out, []downStopResult{
		{rigName: "gastown", name: "crew/sloan", err: errors.New("kill-session: no server running")},
		{rigName: "gastown", name: "nux"},
	})

	if stopped != 1 || failed != 1 {
		t.Errorf("stopped, failed = %d, %d; want 1, 1", stopped, failed)
	}
	for _, want := range []string{"crew/sloan: kill-session: no server running", "nux stopped"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
}

func TestIsProcessRunning_CurrentProcess(t *testing.T) {
	t.Parallel()
	if !isProcessRunning(os.Getpid()) {
		t.Error("current process should be detected as running")
	}
}

func TestIsProcessRunning_InvalidPID(t *testing.T) {
	t.Parallel()
	if isProcessRunning(99999999) {
		t.Error("invalid PID should not be detected as running")
	}
}

func TestIsProcessRunning_MaxPID(t *testing.T) {
	t.Parallel()
	if isProcessRunning(2147483647) {
		t.Error("max PID should not be running")
	}
}
