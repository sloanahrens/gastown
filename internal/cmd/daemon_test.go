package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/templates"
)

// TestRunDaemonEnableSupervisor_RefusesWhenDaemonLockHeld verifies that
// 'gt daemon enable-supervisor' exits non-zero, writes nothing, and points to
// 'gt daemon stop' when a daemon (manual or otherwise) already holds
// daemon.lock. RunAtLoad + KeepAlive.Crashed would otherwise cause launchd to
// immediately spawn a second daemon that loses the flock and gets respawned
// forever alongside the manually-started one.
func TestRunDaemonEnableSupervisor_RefusesWhenDaemonLockHeld(t *testing.T) {
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

	// Isolate HOME so a refusal-path bug can't touch the real machine's
	// LaunchAgents/systemd directories.
	t.Setenv("HOME", t.TempDir())
	t.Chdir(townRoot)

	if err := runDaemonEnableSupervisor(nil, nil); err == nil {
		t.Fatal("runDaemonEnableSupervisor() error = nil, want error while daemon.lock is held")
	} else if !strings.Contains(err.Error(), "stop the running daemon first: gt daemon stop") {
		t.Errorf("runDaemonEnableSupervisor() error = %q, want it to mention 'gt daemon stop'", err.Error())
	}

	// Nothing should have been written.
	if plistPath, perr := templates.LaunchdPlistPath(); perr == nil {
		if _, statErr := os.Stat(plistPath); statErr == nil {
			t.Error("plist file was written despite daemon.lock being held")
		}
	}
}

func TestReadDaemonStartupFailure(t *testing.T) {
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
