//go:build integration && !windows

package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// Both tests here drive one tick against a script that backgrounds a child
// holding the stdout pipe the daemon's buffer copy reads, and assert the
// single-flight flag goes back to false with the tick. While it is held every
// later tick logs "previous tick still running" and the upgrade wait reads the
// daemon as busy (gt-7uyfc).
//
// The two shapes of that pipe holder are different fixes, so each has its own
// test: a command that has already exited leaves exec nothing to cancel and
// only the WaitDelay bounds the wait (TestIntegrationRunCmd_
// SpecDispatchReleasesTheTickWithAGrandchildOnThePipe), and a command still
// running at the timeout is ended by the process-group kill, which reaches the
// child with it (TestIntegrationRunCmd_SpecDispatchKillsTheWholeGroup).
//
// Neither test is parallel: they write specDispatchTimeout, and parallel
// siblings would race it.

func TestIntegrationRunCmd_SpecDispatchReleasesTheTickWithAGrandchildOnThePipe(t *testing.T) {
	stubSpecDispatchTimeout(t, 2*time.Second)

	townRoot := t.TempDir()
	pidFile := filepath.Join(townRoot, "grandchild.pid")
	gtPath := filepath.Join(townRoot, "gt")
	// The shell exits at once; the sleep it backgrounded inherits its stdout,
	// which is enough to hold cmd.Run open. Nothing about this run is left to
	// kill, so the WaitDelay is the only bound on the wait.
	script := fmt.Sprintf("#!/bin/sh\nsleep 60 &\necho $! > %s\nexit 0\n", pidFile)
	if err := os.WriteFile(gtPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := specDispatchDaemon(townRoot, gtPath)
	t.Cleanup(func() { killRecordedPID(pidFile) })

	start := time.Now()
	if !d.triggerSpecDispatch() {
		t.Fatal("the tick did not start")
	}
	released := waitFor(specDispatchTimeout+daemonCmdWaitDelay+3*time.Second, func() bool {
		return !d.specDispatchRunning.Load()
	})
	elapsed := time.Since(start)
	if !released {
		t.Fatal("the tick never released the single-flight flag: the guard is held by a command whose grandchild owns the output pipe")
	}
	// The pid file is what says the command ran and the wait was real: a tick
	// that returned before starting it would satisfy the deadline's timeout
	// alone.
	grandchildPID(t, pidFile)
	if elapsed < specDispatchTimeout {
		t.Fatalf("the tick returned after %s, before its %s timeout: the command it bounds never ran", elapsed.Round(time.Millisecond), specDispatchTimeout)
	}
	if bound := specDispatchTimeout + daemonCmdWaitDelay; elapsed > bound+time.Second {
		t.Errorf("the tick returned after %s, past the %s bound of timeout plus WaitDelay", elapsed.Round(time.Millisecond), bound)
	}
}

func TestIntegrationRunCmd_SpecDispatchKillsTheWholeGroup(t *testing.T) {
	stubSpecDispatchTimeout(t, 2*time.Second)

	townRoot := t.TempDir()
	pidFile := filepath.Join(townRoot, "grandchild.pid")
	gtPath := filepath.Join(townRoot, "gt")
	// The shell waits on the child, so the command is still running at the
	// timeout: the kill reaches the process the daemon started and the
	// child that inherited its stdout pipe is in the same group.
	script := fmt.Sprintf("#!/bin/sh\nsleep 60 &\necho $! > %s\nwait\n", pidFile)
	if err := os.WriteFile(gtPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := specDispatchDaemon(townRoot, gtPath)
	t.Cleanup(func() { killRecordedPID(pidFile) })

	start := time.Now()
	if !d.triggerSpecDispatch() {
		t.Fatal("the tick did not start")
	}
	released := waitFor(specDispatchTimeout+daemonCmdWaitDelay+3*time.Second, func() bool {
		return !d.specDispatchRunning.Load()
	})
	elapsed := time.Since(start)
	if !released {
		t.Fatal("the tick never released the single-flight flag")
	}
	pid := grandchildPID(t, pidFile)
	// The group kill ends the wait at the timeout, and the WaitDelay backstop
	// is never reached: a tick that took it would mean the child survived the
	// kill. The helper's own grace bounds the kill, so that much past the
	// timeout is still the group kill and not the backstop.
	if limit := specDispatchTimeout + util.ProcessGroupKillGrace + time.Second; elapsed > limit {
		t.Errorf("the tick returned after %s, past the %s bound of its timeout plus the group kill's grace: the process-group kill did not end it", elapsed.Round(time.Millisecond), limit)
	}
	gone := waitFor(5*time.Second, func() bool { return syscall.Kill(pid, 0) != nil })
	if !gone {
		t.Errorf("backgrounded grandchild %d survived the tick's timeout: the process group was not killed", pid)
	}
}

// specDispatchDaemon is a daemon whose only job is to run one tick: the
// script at gtPath stands in for the gt binary, and the town is a temp dir so
// no real hold file or town is read.
func specDispatchDaemon(townRoot, gtPath string) *Daemon {
	return &Daemon{
		config:       &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: true}}},
		ctx:          context.Background(),
		logger:       log.New(io.Discard, "", 0),
		gtPath:       gtPath,
	}
}

// stubSpecDispatchTimeout shortens the tick's bound for a test that drives
// it, and restores it after.
func stubSpecDispatchTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := specDispatchTimeout
	specDispatchTimeout = d
	t.Cleanup(func() { specDispatchTimeout = prev })
}

// grandchildPID reads the pid the tick's script wrote.
func grandchildPID(t *testing.T, pidFile string) int {
	t.Helper()
	pid, err := readPID(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// killRecordedPID ends a grandchild the test is done with, tolerating a pid
// file the script never wrote (a failing test's cleanup).
func killRecordedPID(pidFile string) {
	if pid, err := readPID(pidFile); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// readPID reads a pid written by a shell's $!, refusing anything that is not
// one: kill(0, ...) is this test binary's own process group.
func readPID(pidFile string) (int, error) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("no grandchild pid was recorded: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("grandchild pid file holds %q, not a pid", raw)
	}
	return pid, nil
}

// waitFor polls cond until it holds or the budget runs out, and reports which.
func waitFor(budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
