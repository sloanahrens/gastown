package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jobs"
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
	provision := func(string) (string, error) { provisioned = true; return "", nil }
	if err := enableSupervisor(townRoot, provision, func(string) error { return nil }); err == nil {
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

// TestDaemonScratchIsNotRegistered: the scratch rehearsal command was removed
// with the Docker-hosted daemon era, so no `gt daemon scratch` subcommand may
// be registered under daemonCmd.
func TestDaemonScratchIsNotRegistered(t *testing.T) {
	t.Parallel()
	for _, c := range daemonCmd.Commands() {
		if c.Name() == "scratch" {
			t.Fatalf("gt daemon scratch is registered; the command should have been deleted")
		}
	}
}

// TestDaemonJobListSourceMaps pins that the mapping copies every field the
// collector judges, including an unknown state and an unrecorded run.
func TestDaemonJobListSourceMaps(t *testing.T) {
	t.Parallel()
	lastRun := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	list := []jobs.Job{
		{Name: "dolt_backup", State: jobs.StateOverdue, Outcome: jobs.OutcomeFailed, LastRun: &lastRun, Cadence: 24 * time.Hour},
		{Name: "future_step", State: jobs.State("a_later_state")},
	}
	want := []daemon.JobReading{
		{Name: "dolt_backup", State: "overdue", Outcome: "failed", LastRun: lastRun, Cadence: 24 * time.Hour},
		{Name: "future_step", State: "a_later_state"},
	}
	if got := jobReadingsFromJobs(list); !reflect.DeepEqual(got, want) {
		t.Errorf("jobReadingsFromJobs() = %+v, want %+v", got, want)
	}
}

// TestDaemonJobListSourceIsWired pins that the daemon start code installs the
// jobs.List-backed source and that a failed read comes back as its error, never
// as an empty list the collector reads as clean.
func TestDaemonJobListSourceIsWired(t *testing.T) {
	t.Parallel()

	noHome := errors.New("no home directory")
	wireJobListSource(func() (string, error) { return "", noHome })
	if daemon.JobListSource == nil {
		t.Fatal("wireJobListSource left daemon.JobListSource nil, so the collector raises nothing")
	}
	if _, err := daemon.JobListSource(t.TempDir(), time.Now()); !errors.Is(err, noHome) {
		t.Errorf("source with no backup root = %v, want %v", err, noHome)
	}

	wireJobListSource(func() (string, error) { return t.TempDir(), nil })
	if _, err := daemon.JobListSource("", time.Now()); err == nil {
		t.Error("source on an empty town root = nil error, want jobs.List's refusal")
	}
}
