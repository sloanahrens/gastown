package daemon

import (
	"errors"
	"io/fs"
	"strings"
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

	if !strings.Contains(logs.String(), "apply deferred") {
		t.Fatalf("the pass did not defer, so this row proves nothing: %q", logs.String())
	}
	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a pass that deferred: %q", at, logs.String())
	}
}

// TestDiskMaintenanceLastRunSkipsAPassRefusedWhole: a daily pass that refused
// whole (here its targets could not be built) concluded without running, so the
// window settles and gt jobs reads no last run: a refusal is not a run (om
// finding f75b5ca1f0e7). The next tick does not retry it.
func TestDiskMaintenanceLastRunSkipsAPassRefusedWhole(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.targetsErr = errors.New("the registry could not be read")

	d.runDiskMaintenance()

	if !strings.Contains(logs.String(), "the targets could not be built") {
		t.Fatalf("the pass was not refused, so this row proves nothing: %q", logs.String())
	}
	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a pass that refused whole: %q", at, logs.String())
	}
	if d.diskMaintenanceSettledAt().IsZero() {
		t.Error("the refused pass did not settle its window: it would retry every tick")
	}
}

// diskMaintNotRecorded is the log line a daily pass writes when no target
// completed its work, and so records no last run.
const diskMaintNotRecorded = "daily pass not recorded as a run"

// TestDiskMaintenanceLastRunSkipsAPassWhoseEveryTargetRefused: a target whose
// plan refused (it could not check its inputs) did not run its work; a pass whose
// only target refused records no last run, says so, and still settles its window
// (gt-27erxa, om finding 12f69a57440c).
func TestDiskMaintenanceLastRunSkipsAPassWhoseEveryTargetRefused(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.target.plan = diskmaint.Plan{Refused: "the registry listing was unreadable"}

	d.runDiskMaintenance()

	if !strings.Contains(logs.String(), "refused: the registry listing was unreadable") {
		t.Fatalf("the target did not refuse, so this row proves nothing: %q", logs.String())
	}
	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a pass whose every target refused: %q", at, logs.String())
	}
	if !strings.Contains(logs.String(), diskMaintNotRecorded) {
		t.Errorf("the log does not say the pass was not recorded as a run: %q", logs.String())
	}
	if d.diskMaintenanceSettledAt().IsZero() {
		t.Error("the pass did not settle its window: it would retry every tick")
	}
}

// TestDiskMaintenanceLastRunRecordsAPassWhereOneTargetRan: some targets refuse and
// one runs its work: the pass ran, so the last run is recorded.
func TestDiskMaintenanceLastRunRecordsAPassWhereOneTargetRan(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.target.plan = diskmaint.Plan{Refused: "the registry listing was unreadable"}
	f.extra = map[string]*diskMaintFakeTarget{"engine": {}}

	d.runDiskMaintenance()

	if !strings.Contains(logs.String(), "refused: the registry listing was unreadable") || !strings.Contains(logs.String(), "engine dry_run") {
		t.Fatalf("one target must refuse and the other run, or this row proves nothing: %q", logs.String())
	}
	if _, ok := diskMaintRecordedRun(t, d); !ok {
		t.Errorf("a pass in which one target ran left no last run: %q", logs.String())
	}
	if strings.Contains(logs.String(), diskMaintNotRecorded) {
		t.Errorf("the log says the pass was not recorded though a target ran: %q", logs.String())
	}
}

// TestDiskMaintenanceLastRunSkipsAPassWhosePlanCannotBeBuilt: a target whose plan
// cannot be built did not run, so the pass records no last run and says so.
func TestDiskMaintenanceLastRunSkipsAPassWhosePlanCannotBeBuilt(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.target.planErr = errors.New("the ledger could not be read")

	d.runDiskMaintenance()

	if !strings.Contains(logs.String(), "the plan could not be built: the ledger could not be read") {
		t.Fatalf("the plan did not fail, so this row proves nothing: %q", logs.String())
	}
	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a pass whose plan could not be built: %q", at, logs.String())
	}
	if !strings.Contains(logs.String(), diskMaintNotRecorded) {
		t.Errorf("the log does not say the pass was not recorded as a run: %q", logs.String())
	}
}

// TestDiskMaintenanceLastRunSkipsAPassWhoseApplyFailed: a target that ran and
// failed is a run that failed, not a good run: a last-run row holds a time and no
// outcome and is read for freshness, so recording it would clear the overdue alarm
// on a job that keeps failing. The pass records nothing and says so.
func TestDiskMaintenanceLastRunSkipsAPassWhoseApplyFailed(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("", agentconfig.DiskMaintenanceModeApply)
	f.target.plan = diskmaint.Plan{Actions: []diskmaint.Action{{Kind: "delete-path", Ref: "ci-gocache/x"}}}
	f.target.applyErr = errors.New("the registry refused the delete")

	d.runDiskMaintenance()

	if !strings.Contains(logs.String(), "apply failed: the registry refused the delete") {
		t.Fatalf("the apply did not fail, so this row proves nothing: %q", logs.String())
	}
	if at, ok := diskMaintRecordedRun(t, d); ok {
		t.Errorf("recorded a daily run at %v from a pass whose only apply failed: %q", at, logs.String())
	}
	if !strings.Contains(logs.String(), diskMaintNotRecorded) {
		t.Errorf("the log does not say the pass was not recorded as a run: %q", logs.String())
	}
}
