package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// passDaemon is a daemon with overrun_guard on and nothing else to read, so a
// pass finds no seats and ends at once.
func passDaemon(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true}}},
	}
}

// TestOverrunPassStartMarker: a pass reads as running from its start until its
// completion record clears the mark, so a health read can tell a pass in flight
// from one that is merely overdue (gt-46l6ct).
func TestOverrunPassStartMarker(t *testing.T) {
	t.Parallel()

	t.Run("a held pass reads its start, and nothing after it ends", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		if got := d.overrunGuardRun.RunningSince(); !got.IsZero() {
			t.Fatalf("RunningSince before any pass = %v, want zero", got)
		}

		before := time.Now()
		var during time.Time
		d.runOverrunGuardPass(func() { during = d.overrunGuardRun.RunningSince() })
		after := time.Now()

		if during.IsZero() {
			t.Fatal("RunningSince inside a started pass = zero, want the pass start")
		}
		if during.Before(before) || during.After(after) {
			t.Errorf("RunningSince inside the pass = %v, want a time between %v and %v", during, before, after)
		}
		if got := d.overrunGuardRun.RunningSince(); !got.IsZero() {
			t.Errorf("RunningSince after the pass ended = %v, want zero", got)
		}
	})

	t.Run("the guard's goroutine marks its pass", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		d.triggerOverrunGuard()
		waitFlagClear(t, &d.overrunGuardRunning)
		if d.overrunGuardRun.last().IsZero() {
			t.Fatal("triggering the guard recorded no completion, so no pass ran")
		}
		if got := d.overrunGuardRun.RunningSince(); !got.IsZero() {
			t.Errorf("RunningSince after the guard's pass = %v, want zero", got)
		}
	})
}

// TestOverrunPassStartMarkerFile: a pass persists its start to
// patrol_last_run.json while it runs, and its completion removes the entry. A
// start older than the daemon's own start reads as not running, so a pass a
// dead daemon left behind is not shown as in flight (gt-v9ynaw).
func TestOverrunPassStartMarkerFile(t *testing.T) {
	t.Parallel()

	t.Run("a started pass writes running_since and its end removes it", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		root := d.config.TownRoot
		before := time.Now()
		var during time.Time
		var duringErr error
		d.runOverrunGuardPass(func() {
			during, duringErr = ReadPatrolRunningSince(root, "overrun_guard", before)
		})

		if duringErr != nil {
			t.Fatalf("ReadPatrolRunningSince inside the pass: %v", duringErr)
		}
		if during.IsZero() {
			t.Fatal("running_since inside a started pass = zero, want the pass start in the file")
		}
		runs, err := ReadPatrolLastRuns(root)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns after the pass: %v", err)
		}
		if runs["overrun_guard"].IsZero() {
			t.Fatal("the pass recorded no completion, so its end never cleared the marker")
		}
		got, err := ReadPatrolRunningSince(root, "overrun_guard", before)
		if err != nil {
			t.Fatalf("ReadPatrolRunningSince after the pass: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("running_since after the pass = %v, want the entry removed", got)
		}
		data, err := os.ReadFile(filepath.Join(root, "daemon", "patrol_last_run.json"))
		if err != nil {
			t.Fatalf("read patrol_last_run.json after the pass: %v", err)
		}
		if strings.Contains(string(data), "running_since") {
			t.Errorf("patrol_last_run.json after the pass still carries running_since:\n%s", data)
		}
	})

	t.Run("a start older than the daemon's start reads as not running", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		old := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		if err := savePatrolRunningSince(root, "overrun_guard", old); err != nil {
			t.Fatalf("savePatrolRunningSince: %v", err)
		}

		got, err := ReadPatrolRunningSince(root, "overrun_guard", old.Add(time.Hour))
		if err != nil {
			t.Fatalf("ReadPatrolRunningSince with a later daemon start: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("running_since %v from before the daemon started = %v, want not running", old, got)
		}

		got, err = ReadPatrolRunningSince(root, "overrun_guard", old.Add(-time.Hour))
		if err != nil {
			t.Fatalf("ReadPatrolRunningSince with an earlier daemon start: %v", err)
		}
		if !got.Equal(old) {
			t.Errorf("running_since from after the daemon started = %v, want %v", got, old)
		}
	})

	t.Run("an unknown daemon start is an error, not a read", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := savePatrolRunningSince(root, "overrun_guard", time.Now()); err != nil {
			t.Fatalf("savePatrolRunningSince: %v", err)
		}
		if _, err := ReadPatrolRunningSince(root, "overrun_guard", time.Time{}); err == nil {
			t.Error("ReadPatrolRunningSince with no daemon start = nil error, want an error")
		}
	})

	t.Run("a corrupt file is reported, not read as idle", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		path := filepath.Join(root, "daemon", "patrol_last_run.json")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
			t.Fatalf("write corrupt file: %v", err)
		}
		_, err := ReadPatrolRunningSince(root, "overrun_guard", time.Now())
		if !errors.Is(err, ErrPatrolLastRunCorrupt) {
			t.Errorf("ReadPatrolRunningSince on a corrupt file: err = %v, want ErrPatrolLastRunCorrupt", err)
		}
	})
}
