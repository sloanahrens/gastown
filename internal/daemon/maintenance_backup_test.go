package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltpause"
)

// The nightly backup runs through the same fakes as the gc (withGCFakes):
// the copy, the clock, the pause marker and the escalation sink are seams,
// and the backup root is a t.TempDir(). TestIntegrationNightlyBackupRestores
// runs the real CALL dolt_backup against a server.

func TestScheduledMaintenanceBacksUpBeforeGCUnderPause(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	// Each copy and each gc sits between its own marker write and remove,
	// and every copy is done before the first gc.
	want := "write nightly backup of gt,backup gt,remove," +
		"write nightly backup of hq,backup hq,remove," +
		"write hq,gc hq,remove,write gt,gc gt,remove"
	if got := strings.Join(f.events, ","); got != want {
		t.Errorf("events = %s\nwant     %s", got, want)
	}
	if f.marker != nil {
		t.Errorf("pause marker left behind: %+v", f.marker)
	}
	for _, m := range f.written[:2] {
		if m.Actor != maintenancePauseActor || !m.Until.Equal(gcTestNow.Add(maintenanceBackupTimeout+maintenancePauseSlack)) {
			t.Errorf("backup marker = %+v, want actor %s until now+backup timeout+slack", m, maintenancePauseActor)
		}
	}

	b, ok, err := doltbackup.Newest(f.backupRoot)
	if err != nil || !ok {
		t.Fatalf("no backup committed: %v", err)
	}
	if b.Path != doltbackup.Dir(f.backupRoot, gcTestNow) || !b.Has("gt") || !b.Has("hq") {
		t.Errorf("backup = %+v, want tonight's with gt and hq", b)
	}
	for _, db := range []string{"gt", "hq"} {
		if _, err := os.Stat(filepath.Join(b.Path, db)); err != nil {
			t.Errorf("backup has no %s directory: %v", db, err)
		}
	}
	if len(f.escalations) != 0 {
		t.Errorf("successful run escalated: %v", f.escalations)
	}
}

// A gc deferred by a busy town retries on the next tick; the backup it
// already took is not taken again.
func TestScheduledMaintenanceGCRetryDoesNotRepeatBackup(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	f.quietUntil = 0 // busy
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	if strings.Join(f.backupCalls, ",") != "hq" || len(f.gcCalls) != 0 {
		t.Fatalf("busy tick: backups %v, gc %v; want the backup and no gc", f.backupCalls, f.gcCalls)
	}

	f.quietUntil = -1
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 1 {
		t.Errorf("retry tick backed up again: %v", f.backupCalls)
	}
	if strings.Join(f.gcCalls, ",") != "hq" {
		t.Errorf("retry tick gc = %v, want hq", f.gcCalls)
	}
}

// A failed copy leaves no partial night, removes the marker, escalates once,
// skips the gc and does not retry in the window. The server is never stopped:
// the backup has no server-lifecycle call at all.
func TestScheduledMaintenanceBackupFailureSkipsGCAndUnpauses(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
	f.backupErr["hq"] = errors.New("dolt_backup sync-url: disk full")
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran after a failed backup: %v", f.gcCalls)
	}
	if strings.Join(f.backupCalls, ",") != "gt,hq" {
		t.Errorf("backup calls = %v, want gt,hq once (no retry in the window)", f.backupCalls)
	}
	if f.marker != nil {
		t.Errorf("pause marker left behind after a failed backup: %+v", f.marker)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "nightly backup of hq failed: dolt_backup sync-url: disk full") {
		t.Errorf("escalations = %v, want one naming hq", f.escalations)
	}
	entries, err := os.ReadDir(f.backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed night left %d entr(ies) under the backup root, want none", len(entries))
	}
	if !strings.Contains(logs.String(), "no gc tonight") {
		t.Errorf("log does not say the gc was skipped:\n%s", logs.String())
	}
}

func TestMaintenanceBackupRotationKeepsSeven(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	for daysAgo := 9; daysAgo >= 1; daysAgo-- {
		started := gcTestNow.AddDate(0, 0, -daysAgo)
		if err := os.MkdirAll(doltbackup.PartialDir(f.backupRoot, started), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := doltbackup.Commit(f.backupRoot, doltbackup.Manifest{Started: started, Databases: []string{"hq"}}); err != nil {
			t.Fatal(err)
		}
	}

	if res := d.maintenanceBackup([]string{"hq"}); res.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v (%s), want completed", res.outcome, res.reason)
	}

	all, err := doltbackup.List(f.backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != doltbackup.Keep {
		t.Fatalf("%d backups after rotation, want %d", len(all), doltbackup.Keep)
	}
	if first, last := filepath.Base(all[0].Path), filepath.Base(all[len(all)-1].Path); first != "2026-09-25" || last != "2026-10-01" {
		t.Errorf("kept %s..%s, want 2026-09-25..2026-10-01", first, last)
	}
}

func TestMaintenanceBackupTakenOncePerNight(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)

	d.maintenanceBackup([]string{"hq"})
	if res := d.maintenanceBackup([]string{"hq"}); res.outcome != gcOutcomeCompleted {
		t.Errorf("second call outcome = %v, want completed", res.outcome)
	}
	if len(f.backupCalls) != 1 {
		t.Errorf("backup calls = %v, want one per night", f.backupCalls)
	}

	// The next night takes a new one.
	d.maint.now = func() time.Time { return gcTestNow.Add(24 * time.Hour) }
	d.maintenanceBackup([]string{"hq"})
	if len(f.backupCalls) != 2 {
		t.Errorf("next night's backup not taken: %v", f.backupCalls)
	}
}

func TestMaintenanceBackupRestartsAStalePartial(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	stale := filepath.Join(doltbackup.PartialDir(f.backupRoot, gcTestNow), "gone-db")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}

	if res := d.maintenanceBackup([]string{"hq"}); res.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v (%s)", res.outcome, res.reason)
	}
	if _, err := os.Stat(filepath.Join(doltbackup.Dir(f.backupRoot, gcTestNow), "gone-db")); !os.IsNotExist(err) {
		t.Errorf("tonight's backup kept a crashed attempt's directory: %v", err)
	}
}

func TestMaintenanceBackupDefersToAnotherActorsPause(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.foreignPause = &doltpause.Marker{Actor: "sloan", Reason: "manual restore", Until: gcTestNow.Add(time.Hour)}

	res := d.maintenanceBackup([]string{"hq"})
	if res.outcome != gcOutcomeDeferred || !strings.Contains(res.reason, "manual restore") {
		t.Errorf("outcome = %v (%s), want deferred naming the pause", res.outcome, res.reason)
	}
	if len(f.backupCalls) != 0 || len(f.written) != 0 {
		t.Errorf("backup ran or overwrote another actor's marker: calls %v, written %v", f.backupCalls, f.written)
	}
}

func TestMaintenanceBackupDefersWhileDaemonDoltTaskHoldsLock(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	release, ok := d.tryDoltTask("wisp_reaper")
	if !ok {
		t.Fatal("could not take the read side")
	}
	defer release()

	if res := d.maintenanceBackup([]string{"hq"}); res.outcome != gcOutcomeDeferred {
		t.Errorf("outcome = %v, want deferred", res.outcome)
	}
	if len(f.backupCalls) != 0 {
		t.Errorf("backup ran while a daemon Dolt task held the lock: %v", f.backupCalls)
	}
}

func TestMaintenanceBackupRefusesWhenPauseCannotBeWritten(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.writeErr = errors.New("read-only daemon dir")

	if res := d.maintenanceBackup([]string{"hq"}); res.outcome != gcOutcomeFailed {
		t.Errorf("outcome = %v, want failed", res.outcome)
	}
	if len(f.backupCalls) != 0 {
		t.Errorf("backup ran without a pause marker: %v", f.backupCalls)
	}
}

func TestDoltBackupSyncURLRefusesBadInputBeforeConnecting(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	for _, db := range []string{"", "../hq", "hq?allowAllFiles=true", ".dolt", "a b"} {
		if err := d.doltBackupSyncURL(context.Background(), db, "/abs"); err == nil || !strings.Contains(err.Error(), "invalid database name") {
			t.Errorf("doltBackupSyncURL(%q) = %v, want invalid-name error", db, err)
		}
	}
	if err := d.doltBackupSyncURL(context.Background(), "hq", "relative/dir"); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("relative destination = %v, want refusal", err)
	}
}
