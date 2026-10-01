//go:build integration

package steward

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// TestIntegrationRunRecordedReportsTheGroup: a job is its own process group
// leader, so the pid it starts with is the group the ledger must name.
func TestIntegrationRunRecordedReportsTheGroup(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	util.SetProcessGroup(cmd)
	var got int
	if err := runRecorded(cmd, func(pgid int) error { got = pgid; return nil }); err != nil {
		t.Fatal(err)
	}
	if got == 0 || got != cmd.Process.Pid {
		t.Fatalf("recorded group %d, want the process's pid %d", got, cmd.Process.Pid)
	}
}

// TestIntegrationRunRecordedKillsAJobItCannotRecord: a group no ledger row
// names is one no later daemon can kill, so the job does not run on.
func TestIntegrationRunRecordedKillsAJobItCannotRecord(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "sleep", "60")
	util.SetProcessGroup(cmd)
	errNoRow := errors.New("disk full")
	start := time.Now()
	err := runRecorded(cmd, func(int) error { return errNoRow })
	if !errors.Is(err, errNoRow) {
		t.Fatalf("err = %v, want the recording error", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("the job was left running")
	}
	if cmd.ProcessState == nil {
		t.Fatal("the job was not reaped")
	}
}
