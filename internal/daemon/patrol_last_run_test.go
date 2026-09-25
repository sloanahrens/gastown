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

// TestShortPatrolCheckTick pins the min(5m, interval/4) relationship and its
// floor/ceiling clamps.
func TestShortPatrolCheckTick(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{15 * time.Minute, 3*time.Minute + 45*time.Second}, // jsonl_git_backup, dolt_backup
		{1 * time.Hour, 5 * time.Minute},                   // wisp_reaper: interval/4 (15m) clamped to the 5m ceiling
		{10 * time.Minute, 2*time.Minute + 30*time.Second}, // checkpoint_dog, patrol_watchdog
		{60 * time.Minute, 5 * time.Minute},                // main_branch_test default
		{30 * time.Minute, 5 * time.Minute},                // mayor_dispatch: interval/4 (7.5m) clamped to 5m
		{2 * time.Minute, time.Minute},                     // below the 1m floor: interval/4 (30s) clamped up
		{0, time.Minute},                                   // non-positive: floor
	}
	for _, tt := range tests {
		if got := shortPatrolCheckTick(tt.interval); got != tt.want {
			t.Errorf("shortPatrolCheckTick(%v) = %v, want %v", tt.interval, got, tt.want)
		}
	}
}

// TestShortPatrolCheckTickAvoidsSteadyStateHalfRate is the regression test for
// crew review point 2 on gt-gxpwc: a ticker fired at exactly the run interval,
// combined with last-run recorded at cycle END, halves a patrol's effective
// rate once a cycle takes non-negligible wall-clock time — the review's own
// simulation was a 15m interval with a 2m cycle producing 4 runs in 8 ticks
// instead of 8. This walks that identical timeline at shortPatrolCheckTick's
// cadence instead of the bare interval and asserts the patrol keeps pace.
func TestShortPatrolCheckTickAvoidsSteadyStateHalfRate(t *testing.T) {
	const interval = 15 * time.Minute
	const cycleDuration = 2 * time.Minute
	const walked = 2 * time.Hour
	tick := shortPatrolCheckTick(interval)

	townRoot := t.TempDir()
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	var runStarts []time.Time
	for at := start; at.Sub(start) < walked; at = at.Add(tick) {
		dec := evaluatePatrolDue(townRoot, "jsonl_git_backup", time.Time{}, at, interval)
		if !dec.due {
			continue
		}
		runStarts = append(runStarts, at)
		// last-run is recorded at cycle END, matching every converted
		// patrol's defer-at-cycle-end semantics.
		if err := savePatrolLastRun(townRoot, "jsonl_git_backup", at.Add(cycleDuration)); err != nil {
			t.Fatalf("save last run: %v", err)
		}
	}

	wantRuns := int(walked / interval)
	if len(runStarts) < wantRuns-1 {
		t.Fatalf("only %d runs across %v at a %v interval (check tick %v), want ~%d — this is the steady-state half-rate bug",
			len(runStarts), walked, interval, tick, wantRuns)
	}

	// No gap between consecutive runs may reach double the interval — the
	// unmistakable shape of a skipped cycle (the old bug: a ticker fired at
	// the full interval let exactly every other check see "not due").
	for i := 1; i < len(runStarts); i++ {
		if gap := runStarts[i].Sub(runStarts[i-1]); gap >= 2*interval {
			t.Errorf("gap of %v between run %d and %d is a full interval late — a cycle was skipped", gap, i-1, i)
		}
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
