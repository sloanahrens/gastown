package daemon

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landworker"
)

// waitLandingLoops blocks a fixture's teardown until the landing loops started
// against d have stopped, so a completed pass is not still writing
// patrol_last_run.json as t.TempDir deletes the tree. The wait is bounded: a
// loop that outlives its canceled context is an error, not a hang (gt-zv3ndb).
func waitLandingLoops(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-d.landingLoopsIdle():
	case <-ctx.Done():
		t.Error("the landing loops did not stop after their daemon's context was canceled")
	}
}

// TestPatrolLastRunLandingWorkerRecordsAPass pins the landing_worker row's
// last-run record: a completed pass, even one that found nothing, is a run,
// while a loop that never starts a pass records none (gt-zv3ndb).
func TestPatrolLastRunLandingWorkerRecordsAPass(t *testing.T) {
	t.Parallel()

	t.Run("a completed pass is recorded", func(t *testing.T) {
		t.Parallel()
		d, _ := landingDrainDaemon(t)
		done := countPasses(d)
		pass := func(context.Context) landworker.Report { return landworker.Report{} }
		// The fixture's cleanup cancels the loop and joins it before the temp
		// town is removed.
		go d.landingWorkerLoop("gastown", time.Hour, pass)
		settlePasses(t, done, 1)

		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		at, ok := runs["landing_worker"]
		if !ok || at.IsZero() {
			t.Fatalf("landing_worker last run = %v (recorded %v), want a completed pass; runs: %v", at, ok, runs)
		}
	})

	t.Run("a pass that does not start is not recorded", func(t *testing.T) {
		t.Parallel()
		d, _ := landingDrainDaemon(t)
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			LandingWorker: &LandingWorkerConfig{Enabled: false},
		}}
		pass := func(context.Context) landworker.Report { return landworker.Report{} }

		// The loop returns at its first check because the patrol is disabled,
		// so no pass runs and nothing is recorded.
		d.landingWorkerLoop("gastown", time.Hour, pass)

		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		if at, ok := runs["landing_worker"]; ok && !at.IsZero() {
			t.Fatalf("a disabled patrol recorded a run at %v; runs: %v", at, runs)
		}
	})
}
