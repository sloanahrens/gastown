package daemon

import (
	"io"
	"log"
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
