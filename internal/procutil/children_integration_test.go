//go:build integration && !windows

package procutil

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A defunct child is reaped by its own pid and a live one is left to its owner:
// the dashboard restarts in place with whatever children the replaced image
// left, and the polls of the new image own theirs through os/exec.
func TestIntegrationReapZombieChildren_ReapsTheDefunctChildAndLeavesALiveOne(t *testing.T) {
	live := exec.Command("sh", "-c", "read x; exit 0")
	stdin, err := live.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := live.Start(); err != nil {
		t.Fatalf("start live child: %v", err)
	}
	liveDone := make(chan error, 1)
	go func() { liveDone <- live.Wait() }() // its owner, as every poll has one
	t.Cleanup(func() { _ = stdin.Close() })

	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Fatalf("start short-lived child: %v", err)
	}
	deadPid := dead.Process.Pid // never waited on: it exits and stays defunct

	deadline := time.Now().Add(eventually)
	for !strings.HasPrefix(psChildren(t)[deadPid], "Z") {
		if time.Now().After(deadline) {
			t.Fatalf("child %d never became defunct", deadPid)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := ReapZombieChildren(); got != 1 {
		t.Fatalf("ReapZombieChildren = %d, want 1 (the defunct child, not the live one)", got)
	}
	if inProcessTable(t, deadPid) {
		t.Errorf("child %d is still in the process table after it was reaped", deadPid)
	}
	if !inProcessTable(t, live.Process.Pid) {
		t.Errorf("live child %d left the process table: it was touched", live.Process.Pid)
	}
	if got := ReapZombieChildren(); got != 0 {
		t.Errorf("a second ReapZombieChildren = %d, want 0", got)
	}

	_ = stdin.Close() // let the live child finish; its owner's Wait must still succeed
	if err := <-liveDone; err != nil {
		t.Errorf("live child's Wait = %v, want nil: its status was taken", err)
	}
}
