package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEvaluatePatrolDueFiresAcrossRestarts is the shared regression test for
// gt-ima2 / gt-gxpwc: a patrol driven only by an in-process ticker resets its
// countdown on every daemon restart, so a host restarting the daemon faster
// than the ticker's interval starves the patrol outright — jsonl_git_backup
// and wisp_reaper both did this in practice (gt-gxpwc), the same class
// compactor_dog was fixed for (gt-ima2).
//
// evaluatePatrolDue is now the one function every ticker-driven patrol in
// this package calls to decide due-ness, so this test walks a restart
// timeline against it directly rather than duplicating the walk per patrol.
func TestEvaluatePatrolDueFiresAcrossRestarts(t *testing.T) {
	const interval = 15 * time.Minute
	// Restart cadence shorter than the interval — the exact shape from the
	// bug report (daemon restarts every ~13.5m against a 15m patrol).
	const restartGap = 13 * time.Minute

	townRoot := t.TempDir()
	start := time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC)

	var due []time.Time
	at := start
	for i := 0; i < 6; i++ {
		// Each iteration models a fresh daemon process: no in-memory state
		// carries over, only what is on disk.
		dec := evaluatePatrolDue(townRoot, "jsonl_git_backup", time.Time{}, at, interval)
		if dec.due {
			due = append(due, at)
			if err := savePatrolLastRun(townRoot, "jsonl_git_backup", at); err != nil {
				t.Fatalf("save last run: %v", err)
			}
		}
		at = at.Add(restartGap)
	}

	if len(due) == 0 {
		t.Fatal("patrol never fired across 6 restarts spanning 65 minutes at a 15m interval — this is the gt-ima2/gt-gxpwc starvation bug")
	}

	// First restart always fires (no record yet). After that, due-ness must
	// track wall-clock elapsed time since the last recorded run, not restart
	// count — so consecutive restarts inside one interval must NOT all fire.
	if !due[0].Equal(start) {
		t.Errorf("first check at %v, want %v (no last-run record yet)", due[0], start)
	}
	for i := 1; i < len(due); i++ {
		gap := due[i].Sub(due[i-1])
		if gap < interval {
			t.Errorf("fired again after only %v since the previous run (interval %v) — restarts must not shorten the effective interval", gap, interval)
		}
	}
}

// TestEvaluatePatrolDueRunsWhenLastRunStateUnreadable pins the fail-loud
// rule shared by every patrol built on evaluatePatrolDue: a corrupt last-run
// file must not be read as "not due" — a monitor that goes silent on broken
// state is the exact failure gt-ima2/gt-gxpwc describe.
func TestEvaluatePatrolDueRunsWhenLastRunStateUnreadable(t *testing.T) {
	townRoot := t.TempDir()
	path := patrolLastRunPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create daemon dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	dec := evaluatePatrolDue(townRoot, "wisp_reaper", time.Time{}, time.Now(), time.Hour)
	if !dec.due {
		t.Error("expected due=true when the last-run state is unreadable")
	}
	if dec.warn == "" {
		t.Error("expected a warn explaining the unreadable state")
	}
}

// TestEvaluatePatrolDueSkipsWhenNotDue covers the other half: inside the
// interval, the patrol must stay down rather than firing on every check.
func TestEvaluatePatrolDueSkipsWhenNotDue(t *testing.T) {
	townRoot := t.TempDir()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := savePatrolLastRun(townRoot, "checkpoint_dog", now.Add(-5*time.Minute)); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	dec := evaluatePatrolDue(townRoot, "checkpoint_dog", time.Time{}, now, 10*time.Minute)
	if dec.due {
		t.Error("expected due=false 5 minutes into a 10-minute interval")
	}
	if dec.note == "" {
		t.Error("expected a note explaining the skip")
	}
}

// TestEvaluatePatrolDueUsesInMemoryWhenNewer covers the same-process
// refinement: a completion this process recorded but failed to persist must
// not be repeated by the next check in the same process.
func TestEvaluatePatrolDueUsesInMemoryWhenNewer(t *testing.T) {
	townRoot := t.TempDir()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := savePatrolLastRun(townRoot, "main_branch_test", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed stale last run: %v", err)
	}

	// The disk record is 2h stale, but this process completed a cycle 1
	// minute ago that it could not persist — the in-memory time must win.
	inMemory := now.Add(-time.Minute)
	dec := evaluatePatrolDue(townRoot, "main_branch_test", inMemory, now, 30*time.Minute)
	if dec.due {
		t.Error("expected due=false: the in-memory completion is 1 minute old, well inside the 30m interval")
	}
}
