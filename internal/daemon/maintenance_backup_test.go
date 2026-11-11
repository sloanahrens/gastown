package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/estop"
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
		"write gt,gc gt,remove,write hq,gc hq,remove"
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

func TestMaintenanceBackupRotationKeepsFourteen(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	for daysAgo := 15; daysAgo >= 1; daysAgo-- {
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
	if len(all) != 14 {
		t.Fatalf("%d backups after rotation, want 14", len(all))
	}
	if first, last := filepath.Base(all[0].Path), filepath.Base(all[len(all)-1].Path); first != "2026-09-18" || last != "2026-10-01" {
		t.Errorf("kept %s..%s, want 2026-09-18..2026-10-01", first, last)
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
	f.foreignPause = &doltpause.Marker{Actor: "operator", Reason: "manual restore", Until: gcTestNow.Add(time.Hour)}

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

// sync-url that reports success but writes nothing this daemon can see: the
// Dolt server's backup root is not the one the daemon reads.
func TestMaintenanceBackupRefusesAnUnsharedRoot(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	d.maint.backupExec = func(_ context.Context, _ *Daemon, db, _ string) error {
		f.backupCalls = append(f.backupCalls, db)
		return nil
	}

	res := d.maintenanceBackup([]string{"gt"})

	if res.outcome != gcOutcomeFailed || !strings.Contains(res.reason, f.backupRoot) || !strings.Contains(res.reason, "not shared with the Dolt server") {
		t.Errorf("outcome = %v (%s), want failed naming %s and the unshared root", res.outcome, res.reason, f.backupRoot)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "not shared with the Dolt server") {
		t.Errorf("escalations = %v, want one naming the unshared root", f.escalations)
	}
	if b, ok, err := doltbackup.Newest(f.backupRoot); err != nil || ok {
		t.Errorf("a night was kept for an unshared root: %+v (ok %v, err %v)", b, ok, err)
	}
	if doltbackup.Taken(f.backupRoot, gcTestNow) {
		t.Error("tonight is marked taken although its copy is not visible")
	}
	entries, err := os.ReadDir(f.backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed night left %d entr(ies) under the backup root, want none", len(entries))
	}
}

// backupVisible is a directory check in this daemon's own view of the root: the
// copy is visible when a directory sits at its path. A missing copy, or a plain
// file where the copy belongs, is refused and names the root.
func TestMaintenanceBackupVisibleNeedsADirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	partial := filepath.Join(root, "2026-10-01.partial")
	visible := filepath.Join(partial, "gt")
	if err := os.MkdirAll(visible, 0o755); err != nil {
		t.Fatal(err)
	}
	asFile := filepath.Join(partial, "hq")
	if err := os.WriteFile(asFile, []byte("not a copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(partial, "missing")

	if err := backupVisible(root, visible); err != nil {
		t.Errorf("visible copy refused: %v", err)
	}
	for _, dest := range []string{missing, asFile} {
		err := backupVisible(root, dest)
		if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "not shared with the Dolt server") {
			t.Errorf("backupVisible(%s) = %v, want a refusal naming the root %s", dest, err, root)
		}
	}
}

// The databases of an external server come from the server; a local server's
// come from its data dir. The backup root is the same either way.
func TestMaintenanceBackupDatabasesFollowsTheServer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		external        bool
		wantDBs         string
		wantServerCalls int
	}{
		{name: "local server lists the data dir", external: false, wantDBs: "hq", wantServerCalls: 0},
		{name: "external server lists through the server", external: true, wantDBs: "gt", wantServerCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := gcTestDaemon(t)
			f := withGCFakes(t, d)
			f.sizes = map[string]gcMeasure{"hq": {1 * mib, 0}}
			f.serverDBs = []string{"gt"}

			got, err := d.backupDatabases(tc.external)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != tc.wantDBs || f.serverCalls != tc.wantServerCalls {
				t.Errorf("databases = %v (server listed %d times), want %s (server listed %d times)",
					got, f.serverCalls, tc.wantDBs, tc.wantServerCalls)
			}
		})
	}
}

// The default lister is the offsite backup's (listServerJsonlDatabases): SHOW
// DATABASES through its export runner, with system databases and invalid names
// dropped.
func TestMaintenanceServerDatabasesUsesTheSharedLister(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.jsonlTargetFn = func() jsonlTarget {
		return jsonlTarget{useServer: true, host: "127.0.0.1", port: 3307, user: "root"}
	}
	var tables []string
	d.jsonlExportRunFn = func(_ context.Context, _ *exec.Cmd, table string, _ time.Duration) ([]byte, error) {
		tables = append(tables, table)
		return []byte(`{"rows":[{"Database":"gt"},{"Database":"information_schema"},{"Database":"../etc"},{"Database":"hq"}]}`), nil
	}

	got, err := d.maintenanceServerDatabases()

	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gt,hq" {
		t.Errorf("databases = %v, want gt,hq", got)
	}
	if strings.Join(tables, ",") != "SHOW DATABASES" {
		t.Errorf("lister ran %v, want one SHOW DATABASES", tables)
	}
}

// A failed SHOW DATABASES is the list's error, never an empty list that would
// read as a backup of nothing.
func TestMaintenanceServerDatabasesReturnsTheListerError(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.jsonlTargetFn = func() jsonlTarget {
		return jsonlTarget{useServer: true, host: "127.0.0.1", port: 3307, user: "root"}
	}
	d.jsonlExportRunFn = func(context.Context, *exec.Cmd, string, time.Duration) ([]byte, error) {
		return nil, errors.New("dolt sql: connection refused")
	}

	got, err := d.maintenanceServerDatabases()

	if err == nil || !strings.Contains(err.Error(), "connection refused") || got != nil {
		t.Errorf("databases = %v, err = %v; want no list and the lister's error", got, err)
	}
}

// A town root that is a file cannot hold the Dolt client directory, so the
// list is refused rather than run from a directory that does not exist.
func TestMaintenanceServerDatabasesRefusesAnUnusableClientDir(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	townFile := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(townFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.config.TownRoot = townFile
	d.jsonlTargetFn = func() jsonlTarget {
		t.Error("the Dolt endpoint was read for a list that cannot run")
		return jsonlTarget{}
	}

	if got, err := d.maintenanceServerDatabases(); err == nil || got != nil {
		t.Errorf("databases = %v, err = %v; want the client directory error", got, err)
	}
}

// A scheduled tick with an external server skips the gc, takes the night's
// backup from the server's list, and consumes the window once that backup ends.
func TestScheduledMaintenanceExternalServerTakesBackupWithoutGC(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.serverDBs = []string{"gt", "hq"} // the local data dir holds nothing
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	if strings.Join(f.backupCalls, ",") != "gt,hq" {
		t.Errorf("backup calls = %v, want gt,hq from the server", f.backupCalls)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran against an external server: %v", f.gcCalls)
	}
	if !strings.Contains(logs.String(), "gc needs a local server") {
		t.Errorf("external skip not logged:\n%s", logs.String())
	}
	if !d.lastMaintenanceRun.IsZero() {
		t.Errorf("window consumed before the backup was read back: lastMaintenanceRun = %v", d.lastMaintenanceRun)
	}

	// The next tick folds the finished backup into the window and does not
	// run the night again.
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 2 || len(f.gcCalls) != 0 {
		t.Errorf("second tick: backup calls %v, gc calls %v; want the one backup and no gc", f.backupCalls, f.gcCalls)
	}
	if !d.lastMaintenanceRun.Equal(gcTestNow) {
		t.Errorf("lastMaintenanceRun = %v, want the window consumed at %v", d.lastMaintenanceRun, gcTestNow)
	}
}

// A night whose backup is deferred by another actor's pause keeps the window
// open: the next tick takes the backup once the pause lifts.
func TestScheduledMaintenanceExternalServerDeferredBackupKeepsWindowOpen(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.serverDBs = []string{"hq"}
	f.foreignPause = &doltpause.Marker{Actor: "operator", Reason: "manual restore", Until: gcTestNow.Add(time.Hour)}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	if len(f.backupCalls) != 0 || !d.lastMaintenanceRun.IsZero() {
		t.Fatalf("deferred tick: backup calls %v, lastMaintenanceRun %v; want no backup and the window open",
			f.backupCalls, d.lastMaintenanceRun)
	}

	f.foreignPause = nil
	d.runScheduledMaintenance()
	if strings.Join(f.backupCalls, ",") != "hq" || len(f.gcCalls) != 0 {
		t.Errorf("retry tick: backup calls %v, gc calls %v; want hq and no gc", f.backupCalls, f.gcCalls)
	}

	// The tick after folds the finished backup into the window and runs no
	// second night.
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 1 {
		t.Errorf("window tick after the backup ended repeated it: %v", f.backupCalls)
	}
	if !d.lastMaintenanceRun.Equal(gcTestNow) {
		t.Errorf("lastMaintenanceRun = %v, want the window consumed at %v", d.lastMaintenanceRun, gcTestNow)
	}
}

// An external server that cannot list its databases takes no backup and does
// not consume the window: the next tick asks again. The data dir is not read in
// its place.
func TestScheduledMaintenanceExternalServerListFailureKeepsWindowOpen(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {1 * mib, 0}}
	f.serverErr = errors.New("dolt is not answering")
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 || len(f.gcCalls) != 0 {
		t.Errorf("backup calls %v, gc calls %v; want neither when the server cannot be listed", f.backupCalls, f.gcCalls)
	}
	if !d.lastMaintenanceRun.IsZero() {
		t.Errorf("window consumed with no backup: lastMaintenanceRun = %v", d.lastMaintenanceRun)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "listing the databases the Dolt server serves: dolt is not answering") {
		t.Errorf("escalations = %v, want one naming the list failure", f.escalations)
	}
}

// An external server that serves no database fails the night and escalates; the
// window stays open and the data dir is not read in its place.
func TestScheduledMaintenanceExternalServerWithNoDatabasesFailsAndKeepsWindowOpen(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {1 * mib, 0}}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 || len(f.gcCalls) != 0 {
		t.Errorf("backup calls %v, gc calls %v; want neither for a server with no databases", f.backupCalls, f.gcCalls)
	}
	if !d.lastMaintenanceRun.IsZero() {
		t.Errorf("window consumed by a failed backup: lastMaintenanceRun = %v", d.lastMaintenanceRun)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "the Dolt server serves no databases") {
		t.Errorf("escalations = %v, want one naming the empty server", f.escalations)
	}
	if !strings.Contains(logs.String(), "nightly backup FAILED") {
		t.Errorf("failed backup not logged:\n%s", logs.String())
	}
}

// gcExternal is asked once per tick and that one answer serves every branch:
// an in-window tick takes its backup from the server, and an out-of-window
// tick's catch-ups read the same answer without asking again.
func TestScheduledMaintenanceProbesExternalServerOncePerTick(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.serverDBs = []string{"hq"}
	probes := 0
	d.maint.gcExternal = func(*Daemon) (bool, string) {
		probes++
		return true, "Dolt server is externally managed"
	}
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	if probes != 1 {
		t.Errorf("in-window tick probed gcExternal %d times, want 1", probes)
	}
	if strings.Join(f.backupCalls, ",") != "hq" {
		t.Errorf("in-window backup calls = %v, want hq", f.backupCalls)
	}

	probes = 0
	d.maint.now = func() time.Time { return gcTestNow.Add(5 * time.Hour) }
	d.runScheduledMaintenance()
	if probes != 1 {
		t.Errorf("out-of-window tick probed gcExternal %d times, want 1", probes)
	}
}

// A standby holds the window's gc, not its backup: an external server's night is
// taken from the server's list, and the window is consumed only once it ends.
func TestScheduledMaintenanceStandbyTakesExternalBackupWithoutGC(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.serverDBs = []string{"gt"}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()
	if err := estop.Activate(d.config.TownRoot, "test", "standby for the test"); err != nil {
		t.Fatal(err)
	}

	d.runScheduledMaintenance()

	if strings.Join(f.backupCalls, ",") != "gt" || len(f.gcCalls) != 0 {
		t.Errorf("backup calls %v, gc calls %v; want gt from the server and no gc", f.backupCalls, f.gcCalls)
	}
	if !d.lastMaintenanceRun.IsZero() {
		t.Errorf("window consumed before the backup was read back: lastMaintenanceRun = %v", d.lastMaintenanceRun)
	}

	d.runScheduledMaintenance()
	if strings.Join(f.backupCalls, ",") != "gt" || len(f.gcCalls) != 0 {
		t.Errorf("second tick: backup calls %v, gc calls %v; want the one backup and no gc", f.backupCalls, f.gcCalls)
	}
	if !d.lastMaintenanceRun.Equal(gcTestNow) {
		t.Errorf("lastMaintenanceRun = %v, want the window consumed at %v", d.lastMaintenanceRun, gcTestNow)
	}
}

// Tonight's night already on disk is not taken again against an external
// server, and the window is consumed without a second copy.
func TestScheduledMaintenanceExternalServerDoesNotRepeatTakenNight(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.serverDBs = []string{"hq"}
	if err := os.MkdirAll(doltbackup.PartialDir(f.backupRoot, gcTestNow), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := doltbackup.Commit(f.backupRoot, doltbackup.Manifest{Started: gcTestNow, Finished: gcTestNow, Databases: []string{"hq"}}); err != nil {
		t.Fatal(err)
	}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 || len(f.gcCalls) != 0 {
		t.Errorf("taken night repeated: backup calls %v, gc calls %v", f.backupCalls, f.gcCalls)
	}
	if !d.lastMaintenanceRun.Equal(gcTestNow) {
		t.Errorf("lastMaintenanceRun = %v, want the window consumed at %v", d.lastMaintenanceRun, gcTestNow)
	}
	if all, err := doltbackup.List(f.backupRoot); err != nil || len(all) != 1 {
		t.Errorf("backups = %d (err %v), want the one taken night", len(all), err)
	}
}

// The keep-fourteen prune runs over the external server's backup root as it
// does for a local one: tonight's night is taken and the oldest is removed.
func TestScheduledMaintenanceExternalServerKeepsNewestFourteen(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.serverDBs = []string{"hq"}
	for daysAgo := 14; daysAgo >= 1; daysAgo-- {
		started := gcTestNow.AddDate(0, 0, -daysAgo)
		if err := os.MkdirAll(doltbackup.PartialDir(f.backupRoot, started), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := doltbackup.Commit(f.backupRoot, doltbackup.Manifest{Started: started, Databases: []string{"hq"}}); err != nil {
			t.Fatal(err)
		}
	}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	all, err := doltbackup.List(f.backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != doltbackup.Keep {
		t.Fatalf("%d backups after rotation, want %d", len(all), doltbackup.Keep)
	}
	if last := filepath.Base(all[len(all)-1].Path); last != "2026-10-01" {
		t.Errorf("newest kept = %s, want tonight 2026-10-01", last)
	}
	if first := filepath.Base(all[0].Path); first != "2026-09-18" {
		t.Errorf("oldest kept = %s, want 2026-09-18 (the 14th night back is removed)", first)
	}
}
