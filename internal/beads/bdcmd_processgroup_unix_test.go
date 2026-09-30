//go:build !windows

package beads

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// TestBdCmd_ContextEndKillsTheWholeGroupWithinItsGrace pins the two bounds
// BdCmd's commands inherit from util.SetProcessGroup's Cancel since gt-6t43: a
// group that will not take SIGTERM holds the call for ProcessGroupKillGrace
// past the deadline before the escalation ends it, and the escalation reaches
// a child of the group rather than the shell BdCmd started.
// TestBdCmd_RunTimesOut could not tell either from an immediate SIGKILL: its
// stub dies the moment it is signalled, whether or not the grace was there.
//
// The test ends the context itself, once the child's pid is on stdout, the way
// TestSetProcessGroup_CancelTakesTheGrandchild does: a deadline short enough
// to keep this quick is one a loaded host can spend before the stub has run a
// line, which would leave the group empty and the kill untested.
//
// Not parallel: it writes util.ProcessGroupKillGrace and PATH.
func TestBdCmd_ContextEndKillsTheWholeGroupWithinItsGrace(t *testing.T) {
	const grace = 400 * time.Millisecond
	stubProcessGroupKillGrace(t, grace)

	binDir := t.TempDir()
	// trap '' TERM is inherited across the exec, so nothing in the group
	// answers the polite signal and only the escalation can end it.
	stub := "#!/usr/bin/env sh\ntrap '' TERM\nsleep 60 & echo $!\nwait\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := NewBdCmd("list").buildContextCommand(ctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bd stub: %v", err)
	}
	grandchild := readStdoutPID(t, stdout)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	start := time.Now()
	cancel()
	if err := <-done; err == nil {
		t.Error("cmd.Wait() = nil, want the error of a command killed when its context ended")
	}
	elapsed := time.Since(start)

	if elapsed < grace {
		t.Errorf("the call returned %s after its context ended, before the %s grace BdCmd inherits: the group was killed without being signalled first",
			elapsed.Round(time.Millisecond), grace)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the call took %s to return: the context ending did not bound it", elapsed.Round(time.Millisecond))
	}
	waitForPidGone(t, grandchild)
}

// stubProcessGroupKillGrace shrinks the SIGTERM grace so a test can drive the
// escalation without waiting out the default.
func stubProcessGroupKillGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	prev := util.ProcessGroupKillGrace
	util.ProcessGroupKillGrace = grace
	t.Cleanup(func() { util.ProcessGroupKillGrace = prev })
}

// readStdoutPID reads the pid the stub printed as its first line of stdout. The
// read blocks until the stub has written it, so the caller knows the group is
// populated before it ends the context.
func readStdoutPID(t *testing.T, stdout io.Reader) int {
	t.Helper()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the stub's backgrounded child pid: %q, %v", line, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("child pid %q: %v", line, err)
	}
	return pid
}

// waitForPidGone polls until pid is gone. A SIGKILLed process stays visible to
// kill(pid, 0) as a zombie until it is reaped, so "gone" is a state to wait for
// rather than to sample once.
func waitForPidGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("child %d survived the context ending: the process group was not killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
