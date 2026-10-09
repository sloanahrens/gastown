package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/daemon"
)

// TestRunDaemonEnableSupervisor_RefusesWhenDaemonLockHeld verifies that
// 'gt daemon enable-supervisor' exits non-zero, writes nothing, and points to
// 'gt daemon stop' when a daemon (manual or otherwise) already holds
// daemon.lock. RunAtLoad + KeepAlive.Crashed would otherwise cause launchd to
// immediately spawn a second daemon that loses the flock and gets respawned
// forever alongside the manually-started one.
func TestRunDaemonEnableSupervisor_RefusesWhenDaemonLockHeld(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("MkdirAll mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("WriteFile town.json: %v", err)
	}

	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("MkdirAll daemon: %v", err)
	}
	lockPath := filepath.Join(daemonDir, "daemon.lock")

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if !locked {
		t.Fatal("expected to acquire daemon.lock")
	}
	defer func() { _ = lock.Unlock() }()

	provisioned := false
	provision := func(string, time.Duration) (string, error) { provisioned = true; return "", nil }
	if err := enableSupervisor(townRoot, provision); err == nil {
		t.Fatal("enableSupervisor() error = nil, want error while daemon.lock is held")
	} else if !strings.Contains(err.Error(), "stop the running daemon first: gt daemon stop") {
		t.Errorf("enableSupervisor() error = %q, want it to mention 'gt daemon stop'", err.Error())
	}
	if provisioned {
		t.Error("supervisor file was provisioned despite daemon.lock being held")
	}
}

func TestReadDaemonStartupFailure(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	logData := "" +
		"2026/03/28 22:00:00 Daemon startup failed (PID 111): stale error\n" +
		"2026/03/28 22:00:01 Daemon startup failed (PID 222): incompatible beads workspace / gt binary combination\n"
	if err := os.WriteFile(filepath.Join(daemonDir, "daemon.log"), []byte(logData), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := readDaemonStartupFailure(townRoot, 222)
	want := "incompatible beads workspace / gt binary combination"
	if got != want {
		t.Fatalf("readDaemonStartupFailure() = %q, want %q", got, want)
	}
}

func TestReadDaemonStartupFailure_MissingPIDReturnsEmpty(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "daemon.log"), []byte("2026/03/28 22:00:00 Daemon startup failed (PID 111): stale error\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := readDaemonStartupFailure(townRoot, 222); got != "" {
		t.Fatalf("readDaemonStartupFailure() = %q, want empty string", got)
	}
}

func TestDaemonRunExitMapsUpgradeTo75(t *testing.T) {
	t.Parallel()
	var code = -1
	exit := func(c int) { code = c }

	if err := daemonRunExit(fmt.Errorf("wrapped: %w", daemon.ErrRestartForUpgrade), exit); err != nil {
		t.Fatalf("daemonRunExit(upgrade) = %v, want nil", err)
	}
	if code != 75 {
		t.Fatalf("exit code = %d, want 75", code)
	}

	code = -1
	other := errors.New("boom")
	if err := daemonRunExit(other, exit); err != other {
		t.Fatalf("daemonRunExit(other) = %v, want passthrough", err)
	}
	if err := daemonRunExit(nil, exit); err != nil || code != -1 {
		t.Fatalf("daemonRunExit(nil) = %v, code %d; want nil and no exit", err, code)
	}
}

func TestDaemonRunExitMapsUnrequestedStopTo75(t *testing.T) {
	t.Parallel()
	var code = -1
	exit := func(c int) { code = c }
	if err := daemonRunExit(fmt.Errorf("wrapped: %w", daemon.ErrUnrequestedStop), exit); err != nil {
		t.Fatalf("daemonRunExit(unrequested stop) = %v, want nil", err)
	}
	if code != 75 {
		t.Fatalf("exit code = %d, want 75 so launchd relaunches the daemon", code)
	}
}
