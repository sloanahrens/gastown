//go:build integration && !windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These run startLocked's real spawn path: a child that exits during startup,
// and one killed when a foreign process holds the port, are process semantics
// no fake can show. verifyStartedLocked's decisions are unit-tested in
// dolt_start_verify_test.go.

// startVerifyManager returns a manager whose startLocked runs the real spawn
// path against a fake `dolt` on PATH. body is the fake's shell script. The
// health probe is stubbed to succeed, standing in for a foreign process that
// holds the port and answers it (gt-4cu7u). Tests using it are not parallel:
// they set PATH and portListenerPIDFn.
func startVerifyManager(t *testing.T, body string, listener func(m *DoltServerManager) int) *DoltServerManager {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := newTestManager(t)
	m.startFn = nil
	m.runningFn = nil
	m.config.DataDir = filepath.Join(m.townRoot, "dolt")
	m.config.LogFile = filepath.Join(m.townRoot, "daemon", "dolt-server.log")

	orig := portListenerPIDFn
	t.Cleanup(func() { portListenerPIDFn = orig })
	portListenerPIDFn = func(int) int { return listener(m) }
	return m
}

func startLockedForTest(m *DoltServerManager) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked()
}

func processGone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A dolt that cannot bind exits at once while the foreign holder keeps
// answering the health probe. The start must fail, not report healthy.
func TestIntegrationStartLocked_FailsWhenDoltExitsDuringStartup(t *testing.T) {
	m := startVerifyManager(t, "exit 1", func(*DoltServerManager) int { return 0 })

	err := startLockedForTest(m)
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("startLocked = %v, want an 'exited during startup' error", err)
	}
	if m.process != nil {
		t.Errorf("m.process = %v, want nil for a server that never came up", m.process)
	}
	if _, statErr := os.Stat(m.pidFile()); !os.IsNotExist(statErr) {
		t.Errorf("pid file still names the dead server (stat err = %v)", statErr)
	}
}

// The listener being the process we started is the success case, and so is a
// listener lsof cannot identify: the check fails only on proof.
func TestIntegrationStartLocked_SucceedsWhenWeOwnThePort(t *testing.T) {
	cases := map[string]func(m *DoltServerManager) int{
		"we hold the port": func(m *DoltServerManager) int { return m.process.Pid },
		"listener unknown": func(*DoltServerManager) int { return 0 },
	}
	for name, listener := range cases {
		t.Run(name, func(t *testing.T) {
			m := startVerifyManager(t, "exec sleep 30", listener)

			if err := startLockedForTest(m); err != nil {
				t.Fatalf("startLocked = %v, want nil", err)
			}
			if m.process == nil {
				t.Fatal("m.process = nil, want the started server tracked")
			}
			pid := m.process.Pid
			t.Cleanup(func() { _ = sendKillSignal(m.process) })
			if _, statErr := os.Stat(m.pidFile()); statErr != nil {
				t.Errorf("pid file missing for a healthy start: %v", statErr)
			}
			if !isProcessAlive(m.process) {
				t.Errorf("started server (PID %d) died", pid)
			}
		})
	}
}

// A dolt that is alive but does not own the port (someone else does) is a
// failed start, and the child we spawned must actually go away.
func TestIntegrationStartLocked_ForeignHolderKillsSpawnedChild(t *testing.T) {
	var childPID int
	m := startVerifyManager(t, "exec sleep 30", func(m *DoltServerManager) int {
		childPID = m.process.Pid
		return 1
	})
	err := startLockedForTest(m)
	if err == nil || !strings.Contains(err.Error(), "not the dolt sql-server we started") {
		t.Fatalf("startLocked = %v, want a foreign-holder error", err)
	}
	if m.process != nil {
		t.Errorf("m.process = %v, want nil", m.process)
	}
	if _, statErr := os.Stat(m.pidFile()); !os.IsNotExist(statErr) {
		t.Errorf("pid file still exists (stat err = %v)", statErr)
	}
	if childPID == 0 || !processGone(childPID) {
		t.Errorf("spawned dolt (PID %d) still running after the failed start", childPID)
	}
}
