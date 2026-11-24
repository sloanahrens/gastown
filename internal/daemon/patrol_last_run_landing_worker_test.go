package daemon

import (
	"context"
	"errors"
	"io/fs"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/landworker"
)

// assertNoLandingWorkerRun fails unless the town records no landing_worker
// completion. A missing file is a town that has recorded no run at all, which
// is what the negative cases expect (gt-zv3ndb).
func assertNoLandingWorkerRun(t *testing.T, d *Daemon) {
	t.Helper()
	runs, err := ReadPatrolLastRuns(d.config.TownRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("ReadPatrolLastRuns: %v", err)
	}
	if at, ok := runs["landing_worker"]; ok && !at.IsZero() {
		t.Fatalf("landing_worker recorded a run at %v; runs: %v", at, runs)
	}
}

// TestPatrolLastRunLandingWorkerRecordsAPass pins the landing_worker row's
// last-run record: a completed pass, even one that found nothing, is a run,
// while a pass that does not start — the patrol disabled — and a pass cut
// short by shutdown record none (gt-zv3ndb). A held landing is the negative
// rows of TestPatrolLastRunLandingWorkerHeldIsNotARun (gt-9jbyfx).
func TestPatrolLastRunLandingWorkerRecordsAPass(t *testing.T) {
	t.Parallel()

	t.Run("a completed pass is recorded", func(t *testing.T) {
		t.Parallel()
		d, _, loops := landingDrainDaemon(t)
		// The run time comes from the daemon's clock, so a fake one pins it.
		now := time.Date(2026, 11, 24, 18, 30, 0, 0, time.UTC)
		d.clock = clockwork.NewFakeClockAt(now)
		done := countPasses(d)
		// The fixture cancels the loop and joins it before the temp town is
		// removed.
		loops.start("gastown", time.Hour, func(context.Context) landworker.Report {
			return landworker.Report{}
		})
		// settlePasses wakes on the loop's pass-done seam, which the loop
		// reaches after recording the run.
		settlePasses(t, done, 1)

		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		at, ok := runs["landing_worker"]
		if !ok || at.IsZero() {
			t.Fatalf("landing_worker last run = %v (recorded %v), want a completed pass; runs: %v", at, ok, runs)
		}
		if !at.Equal(now) {
			t.Fatalf("landing_worker last run = %v, want the daemon clock's %v", at, now)
		}
	})

	t.Run("a disabled patrol is not recorded", func(t *testing.T) {
		t.Parallel()
		d, _, _ := landingDrainDaemon(t)
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			LandingWorker: &LandingWorkerConfig{Enabled: false},
		}}
		var passes atomic.Int32
		// The loop returns at its first check because the patrol is disabled,
		// so no pass runs and nothing is recorded.
		d.landingWorkerLoop("gastown", time.Hour, func(context.Context) landworker.Report {
			passes.Add(1)
			return landworker.Report{}
		})
		if got := passes.Load(); got != 0 {
			t.Fatalf("the pass ran %d time(s) with the patrol disabled, want 0", got)
		}
		assertNoLandingWorkerRun(t, d)
	})

	t.Run("a pass cut short by shutdown is not recorded", func(t *testing.T) {
		t.Parallel()
		d, _, _ := landingDrainDaemon(t)
		ctx, cancel := context.WithCancel(d.ctx)
		d.ctx = ctx
		// The pass returns as the daemon shuts down: it did not complete, so
		// the loop's next iteration sees the canceled context and stops.
		d.landingWorkerLoop("gastown", time.Hour, func(context.Context) landworker.Report {
			cancel()
			return landworker.Report{}
		})
		assertNoLandingWorkerRun(t, d)
	})
}

// TestPatrolLastRunLandingWorkerHeldIsNotARun pins the hold's negative half of
// the row: a landing hold stops the pass before it starts, so a held town
// records no landing_worker completion. Both ways a hold goes on are covered —
// a job's in-memory hold and the overseer's landing.hold file read by the
// heartbeat — so the record cannot claim a cadence the worker did not run at
// (gt-9jbyfx).
func TestPatrolLastRunLandingWorkerHeldIsNotARun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		hold func(t *testing.T, d *Daemon)
	}{
		{name: "a job's hold", hold: func(_ *testing.T, d *Daemon) {
			d.landingHolds.hold(landingHoldUpgradeRestart)
		}},
		{name: "the landing.hold file", hold: func(t *testing.T, d *Daemon) {
			writeLandingHoldFile(t, d, "overseer: drain the queue\n")
			d.checkLandingHoldFile()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _ := landingDrainDaemon(t)
			tc.hold(t, d)
			if !d.landingHolds.held() {
				t.Fatal("the landing hold is not on; the loop would be free to pass")
			}
			// A pass that took a slot under the hold is the failure the hold
			// stops. The fixture's rigs are operational and its patrol is
			// enabled, so without the hold nothing else would keep the loop off
			// a pass.
			d.landingPassWindowFn = func() {
				t.Error("the loop took a pass slot under a landing hold")
			}
			// Cancel before the loop starts, so its one iteration takes the
			// hold branch and returns from the wait instead of idling an hour.
			ctx, cancel := context.WithCancel(d.ctx)
			d.ctx = ctx
			cancel()
			var passes atomic.Int32
			d.landingWorkerLoop("gastown", time.Hour, func(context.Context) landworker.Report {
				passes.Add(1)
				return landworker.Report{}
			})
			if got := passes.Load(); got != 0 {
				t.Fatalf("the pass ran %d time(s) under a landing hold, want 0", got)
			}
			assertNoLandingWorkerRun(t, d)
		})
	}
}
