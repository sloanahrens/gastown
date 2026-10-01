//go:build integration && !windows

package util

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestIntegrationKillProcessGroupIDKillsAGroupKnownOnlyByID: the id is all a
// later daemon has of a job the previous one started.
func TestIntegrationKillProcessGroupIDKillsAGroupKnownOnlyByID(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "60")
	SetDetachedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() // reaps it, as init would an orphan
	if err := KillProcessGroupID(pgid); err != nil {
		t.Fatalf("KillProcessGroupID: %v", err)
	}
	<-done
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Error("the group is still alive")
	}
	// A group already gone is the outcome asked for, not an error.
	if err := KillProcessGroupID(pgid); err != nil {
		t.Errorf("killing an empty group: %v", err)
	}
}
