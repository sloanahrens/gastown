package daemon

import (
	"errors"
	"io/fs"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
)

// diskMaintRecordedRun reads the town's last-run record and returns the disk
// maintenance entry, and whether it has one. A missing record has none, as
// internal/jobs reads it.
func diskMaintRecordedRun(t *testing.T, d *Daemon) (time.Time, bool) {
	t.Helper()
	runs, err := ReadPatrolLastRuns(d.config.TownRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("ReadPatrolLastRuns: %v", err)
	}
	at, ok := runs["disk_maintenance"]
	return at, ok && !at.IsZero()
}

// TestDiskMaintenanceLastRunRecordsTheDailyPass: a completed daily pass leaves
// a disk_maintenance last run at the pass's conclusion, so gt jobs reads the
// row as run rather than never_run.
func TestDiskMaintenanceLastRunRecordsTheDailyPass(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	now := diskMaintNow // 04:45, in the window
	d.diskMaint.now = func() time.Time { return now }
	f.target.during = func() { now = now.Add(3 * time.Minute) } // the plan takes three minutes

	d.runDiskMaintenance()

	at, ok := diskMaintRecordedRun(t, d)
	if !ok {
		t.Fatal("the daily pass left no disk_maintenance last run: gt jobs reads it as never_run")
	}
	if want := diskMaintNow.Add(3 * time.Minute); !at.Equal(want) {
		t.Errorf("last run = %v, want %v: the run is recorded at the pass's conclusion", at, want)
	}
}

// TestDiskMaintenanceLastRunSkipsATickThatOnlyTrims: a tick outside the window
// whose cache trim is due runs no daily pass, so it records no last run.
func TestDiskMaintenanceLastRunSkipsATickThatOnlyTrims(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	now := diskMaintNow.Add(8 * time.Hour) // 12:45, outside the 04:30 window
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance()

	if *trims != 1 {
		t.Fatalf("trimmed %d times, want 1: the fixture must run the trim to be about a trim-only tick", *trims)
	}
	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a tick that only trimmed the cache", at)
	}
}

// TestDiskMaintenanceLastRunSkipsADeferredPass: a pass whose apply deferred
// leaves the window open for a retry, so it records no last run: the retry is
// the daily pass.
func TestDiskMaintenanceLastRunSkipsADeferredPass(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("", agentconfig.DiskMaintenanceModeApply)
	f.target.plan = diskmaint.Plan{Actions: []diskmaint.Action{{Kind: "delete-path", Ref: "ci-gocache/x"}}}
	f.target.result = diskmaint.Result{Deferred: "polecat-image lock is held"}

	d.runDiskMaintenance()

	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a pass that deferred: %q", at, logs.String())
	}
}
