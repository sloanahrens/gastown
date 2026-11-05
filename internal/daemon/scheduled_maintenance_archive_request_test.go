package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// archiveRequestConfig is a patrol config whose history archive writes to dir.
// Its window is gcModeConfig's 03:00, which archiveRequestDaemon's clock is
// outside: an archive request runs at any hour.
func archiveRequestDaemon(t *testing.T, now time.Time, databases ...string) (*Daemon, *gcFakes, *commitFake) {
	t.Helper()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	d.patrolConfig = archiveConfig(t.TempDir())
	d.maint.now = func() time.Time { return now }
	f.sizes = map[string]gcMeasure{}
	for _, db := range databases {
		f.sizes[db] = gcMeasure{631 * mib, 500 * mib}
	}
	return d, f, withArchiveFakes(d, f, nil, nil)
}

// writeArchiveRequestFile puts an archive request for runID in d's update
// directory.
func writeArchiveRequestFile(t *testing.T, d *Daemon, runID string, at time.Time) string {
	t.Helper()
	dir := update.Dir(d.config.TownRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(update.RequestFile{RunID: runID, At: at})
	if err != nil {
		t.Fatal(err)
	}
	path := archiveRequestPath(dir)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readArchiveAnswer decodes the answer the daemon left for an archive request.
func readArchiveAnswer(t *testing.T, d *Daemon) update.MaintenanceResult {
	t.Helper()
	data, err := os.ReadFile(archiveResultPath(update.Dir(d.config.TownRoot)))
	if err != nil {
		t.Fatalf("read archive result: %v", err)
	}
	var res update.MaintenanceResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("parse archive result: %v", err)
	}
	return res
}

// archiveRequestDir is the configured history archive directory.
func archiveRequestDir(d *Daemon) string {
	return d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveDir
}

// mustNotExist fails when path exists.
func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists (%s): %v", what, path, err)
	}
}

// archiveFiles lists the configured archive directory's entries; a directory
// never created is empty.
func archiveFiles(t *testing.T, d *Daemon) []string {
	t.Helper()
	entries, err := os.ReadDir(archiveRequestDir(d))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the archive directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// An archive request runs the history archive pass for every database and
// answers completed, without the backup or the gc.
func TestArchiveRequestRunsArchiveOnlyAndAnswers(t *testing.T) {
	t.Parallel()
	d, f, cf := archiveRequestDaemon(t, outsideRequestNow, "gt", "hq")
	cf.commits = map[string][]archiveCommit{
		"gt": {{Hash: "aaa111", Committer: "operator", Date: outsideRequestNow.Add(-time.Hour), Message: "gt: one"}},
		"hq": {{Hash: "bbb222", Committer: "operator", Date: outsideRequestNow.Add(-2 * time.Hour), Message: "hq: one"}},
	}
	requestFile := writeArchiveRequestFile(t, d, "run-archive", outsideRequestNow)

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v: an archive request runs no gc", f.gcCalls)
	}
	if len(f.backupCalls) != 0 {
		t.Errorf("backup calls = %v: an archive request runs no backup", f.backupCalls)
	}
	res := readArchiveAnswer(t, d)
	if res.RunID != "run-archive" || res.Outcome != "completed" {
		t.Fatalf("result = %+v, want run-archive completed", res)
	}
	lines := strings.Split(res.Detail, "\n")
	if len(lines) != 2 {
		t.Fatalf("detail = %q, want one line per database", res.Detail)
	}
	for _, want := range []string{"gt: archived", "hq: archived"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail = %q, want it to hold %q", res.Detail, want)
		}
	}
	for _, db := range []string{"gt", "hq"} {
		path := filepath.Join(archiveRequestDir(d), archiveFileName("dolt-log", db, outsideRequestNow))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not archived: %v", db, err)
		}
	}
	mustNotExist(t, requestFile, "the answered archive request")
	mustNotExist(t, requestPath(update.Dir(d.config.TownRoot)), "a maintenance request")
	mustNotExist(t, resultPath(update.Dir(d.config.TownRoot)), "a maintenance result")
}

// A busy town leaves the request for the next tick; a standby answers deferred
// at once, naming the hold, and runs nothing.
func TestArchiveRequestLeavesBusyAndAnswersStandby(t *testing.T) {
	t.Parallel()

	t.Run("busy leaves the request", func(t *testing.T) {
		t.Parallel()
		d, f, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		f.quietUntil = 0 // the first quiet probe finds the town busy
		requestFile := writeArchiveRequestFile(t, d, "run-busy", outsideRequestNow)

		d.runScheduledMaintenance()

		if files := archiveFiles(t, d); len(files) != 0 {
			t.Errorf("archived %v while the town was busy", files)
		}
		if _, err := os.Stat(requestFile); err != nil {
			t.Errorf("the request is gone after a busy pass: %v", err)
		}
		mustNotExist(t, archiveResultPath(update.Dir(d.config.TownRoot)), "an archive result for a busy pass")
		if len(f.gcCalls)+len(f.backupCalls) != 0 {
			t.Errorf("gc/backup calls = %v/%v: an archive request runs neither", f.gcCalls, f.backupCalls)
		}
	})

	for _, c := range standbyConditions {
		c := c
		t.Run(c.name+" answers deferred", func(t *testing.T) {
			t.Parallel()
			d, f, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
			c.set(t, d, outsideRequestNow)
			requestFile := writeArchiveRequestFile(t, d, "run-standby", outsideRequestNow)

			d.runScheduledMaintenance()

			if files := archiveFiles(t, d); len(files) != 0 {
				t.Errorf("archived %v under %s", files, c.name)
			}
			res := readArchiveAnswer(t, d)
			if res.RunID != "run-standby" || res.Outcome != "deferred" {
				t.Errorf("result = %+v, want run-standby deferred", res)
			}
			wantDetail := "ESTOP"
			if c.name == "waned Ring" {
				wantDetail = "Ring waned"
			}
			if !strings.Contains(res.Detail, wantDetail) {
				t.Errorf("detail = %q, want it to name %q", res.Detail, wantDetail)
			}
			mustNotExist(t, requestFile, "the answered archive request")
			if len(f.gcCalls)+len(f.backupCalls) != 0 {
				t.Errorf("gc/backup calls = %v/%v under %s", f.gcCalls, f.backupCalls, c.name)
			}
		})
	}
}

// One database whose export fails is named in a failed result; the others are
// archived, and the request is removed only after the result is written.
func TestArchiveRequestFailureNamesTheDatabase(t *testing.T) {
	t.Parallel()
	d, _, cf := archiveRequestDaemon(t, outsideRequestNow, "gt", "hq")
	cf.commits = map[string][]archiveCommit{
		"gt": {{Hash: "aaa111", Committer: "operator", Date: outsideRequestNow.Add(-time.Hour), Message: "gt: one"}},
		"hq": {{Hash: "bbb222", Committer: "operator", Date: outsideRequestNow.Add(-2 * time.Hour), Message: "hq: one"}},
	}
	cf.errs = map[string]error{"gt": errors.New("dolt_log read: connection reset")}
	requestFile := writeArchiveRequestFile(t, d, "run-fail", outsideRequestNow)

	d.runScheduledMaintenance()

	res := readArchiveAnswer(t, d)
	if res.RunID != "run-fail" || res.Outcome != "failed" {
		t.Fatalf("result = %+v, want run-fail failed", res)
	}
	if !strings.Contains(res.Detail, "gt: not archived") {
		t.Errorf("detail = %q, want it to name gt as not archived", res.Detail)
	}
	if !strings.Contains(res.Detail, "hq: archived") {
		t.Errorf("detail = %q, want hq still archived", res.Detail)
	}
	path := filepath.Join(archiveRequestDir(d), archiveFileName("dolt-log", "hq", outsideRequestNow))
	if _, err := os.Stat(path); err != nil {
		t.Errorf("hq was not archived: %v", err)
	}
	mustNotExist(t, requestFile, "the answered archive request")
	if _, err := os.Stat(archiveResultPath(update.Dir(d.config.TownRoot))); err != nil {
		t.Errorf("the result was not written: %v", err)
	}
}

// An archive request touches neither the window's last-run time nor the
// deferral streak, and leaves the maintenance request alone.
func TestArchiveRequestDoesNotConsumeTheWindow(t *testing.T) {
	t.Parallel()
	d, f, cf := archiveRequestDaemon(t, outsideRequestNow, "hq")
	cf.commits = map[string][]archiveCommit{
		"hq": {{Hash: "bbb222", Committer: "operator", Date: outsideRequestNow.Add(-time.Hour), Message: "hq: one"}},
	}
	lastRun := outsideRequestNow.Add(-48 * time.Hour)
	d.lastMaintenanceRun = lastRun
	if _, err := updateMaintenanceGCState(d.config.TownRoot, func(st *maintenanceGCState) {
		st.PendingDeferral = &gcDeferral{WindowEnd: outsideRequestNow.Add(-time.Hour), Reason: "town busy"}
		st.ConsecutiveDeferredWindows = 3
	}); err != nil {
		t.Fatal(err)
	}
	maintenanceFile := writeRequestFile(t, d, "run-maintenance", outsideRequestNow)
	writeArchiveRequestFile(t, d, "run-archive", outsideRequestNow)

	d.runScheduledMaintenance()

	if !d.lastMaintenanceRun.Equal(lastRun) {
		t.Errorf("lastMaintenanceRun = %v, want %v untouched", d.lastMaintenanceRun, lastRun)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.ConsecutiveDeferredWindows != 3 {
		t.Errorf("deferral streak = %d, want 3 untouched", st.ConsecutiveDeferredWindows)
	}
	if st.PendingDeferral == nil || st.PendingDeferral.Reason != "town busy" {
		t.Errorf("pending deferral = %+v, want it untouched", st.PendingDeferral)
	}
	if _, err := os.Stat(maintenanceFile); err != nil {
		t.Errorf("the maintenance request was touched: %v", err)
	}
	mustNotExist(t, resultPath(update.Dir(d.config.TownRoot)), "a maintenance result")
	if res := readArchiveAnswer(t, d); res.Outcome != "completed" {
		t.Errorf("archive result = %+v, want completed", res)
	}
	if len(f.gcCalls)+len(f.backupCalls) != 0 {
		t.Errorf("gc/backup calls = %v/%v: an archive request runs neither", f.gcCalls, f.backupCalls)
	}
}

// The archive request and result names differ from the maintenance pair's, and
// a plain maintenance request still runs the full cycle.
func TestArchiveRequestUsesItsOwnFiles(t *testing.T) {
	t.Parallel()
	if update.ArchiveRequestFileName == update.RequestFileName {
		t.Errorf("ArchiveRequestFileName = %q, the maintenance request's name", update.ArchiveRequestFileName)
	}
	if update.ArchiveResultFileName == update.ResultFileName {
		t.Errorf("ArchiveResultFileName = %q, the maintenance result's name", update.ArchiveResultFileName)
	}
	if update.ArchiveResultFileName == update.ArchiveRequestFileName {
		t.Errorf("the archive request and result share the name %q", update.ArchiveResultFileName)
	}

	t.Run("a maintenance request still runs the full cycle", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow)
		path := writeRequestFile(t, d, "run-maintenance", gcTestNow)

		requestTickAndCycle(t, d)

		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Errorf("gc calls = %q, want hq", got)
		}
		if got := strings.Join(f.backupCalls, ","); got != "hq" {
			t.Errorf("backup calls = %q, want hq", got)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-maintenance" || res.Outcome != "completed" {
			t.Errorf("result = %+v, want run-maintenance completed", res)
		}
		mustNotExist(t, path, "the answered maintenance request")
		dir := update.Dir(d.config.TownRoot)
		mustNotExist(t, archiveRequestPath(dir), "an archive request")
		mustNotExist(t, archiveResultPath(dir), "an archive result")
	})
}

// A request the tick cannot answer is dropped and the tick carries on; an
// invalid run id is answered failed without echoing it; an unreadable request
// is left for the next tick.
func TestArchiveRequestUnanswerable(t *testing.T) {
	t.Parallel()

	t.Run("malformed is dropped and the tick continues", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow) // inside the window: the scheduled cycle runs
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := archiveRequestPath(dir)
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		requestTickAndCycle(t, d)

		mustNotExist(t, path, "the malformed archive request")
		mustNotExist(t, archiveResultPath(dir), "an archive result for a malformed request")
		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Errorf("gc calls = %q, want the scheduled cycle after the drop", got)
		}
	})

	t.Run("an invalid run id is answered failed", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		path := writeArchiveRequestFile(t, d, "run/1", outsideRequestNow)

		d.runScheduledMaintenance()

		res := readArchiveAnswer(t, d)
		if res.Outcome != "failed" || !strings.Contains(res.Detail, "invalid run id") {
			t.Errorf("result = %+v, want failed naming the invalid run id", res)
		}
		if res.RunID != "" {
			t.Errorf("the untrusted run id was echoed into the result: %q", res.RunID)
		}
		mustNotExist(t, path, "the answered archive request")
	})

	t.Run("unreadable is left for the next tick", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := archiveRequestPath(dir)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}

		d.runScheduledMaintenance()

		if _, err := os.Stat(path); err != nil {
			t.Errorf("the unreadable archive request was removed: %v", err)
		}
		mustNotExist(t, archiveResultPath(dir), "an archive result for an unreadable request")
	})
}

// Every path that cannot start the pass answers failed with the reason, but a
// running cycle leaves the request pending instead of answering it.
func TestArchiveRequestRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a running cycle leaves the request pending", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		d.maintenanceGCRunning.Store(true)
		defer d.maintenanceGCRunning.Store(false)
		path := writeArchiveRequestFile(t, d, "run-waiting", outsideRequestNow)

		d.runScheduledMaintenance()

		if files := archiveFiles(t, d); len(files) != 0 {
			t.Errorf("archived %v while a cycle held the slot", files)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request was answered while a cycle was running: %v", err)
		}
		mustNotExist(t, archiveResultPath(update.Dir(d.config.TownRoot)), "an archive result")
	})

	t.Run("an inactive patrol is answered failed", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		d.patrolConfig.Patrols.ScheduledMaintenance.Enabled = false
		path := writeArchiveRequestFile(t, d, "run-off", outsideRequestNow)

		d.runScheduledMaintenance()

		res := readArchiveAnswer(t, d)
		if res.Outcome != "failed" || !strings.Contains(res.Detail, "not active") {
			t.Errorf("result = %+v, want failed naming the inactive patrol", res)
		}
		mustNotExist(t, path, "the answered archive request")
	})

	t.Run("an unset archive directory is answered failed", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		d.patrolConfig = maintenanceWindowAt("03:00") // no ArchiveDir
		path := writeArchiveRequestFile(t, d, "run-off", outsideRequestNow)

		d.runScheduledMaintenance()

		res := readArchiveAnswer(t, d)
		if res.Outcome != "failed" || !strings.Contains(res.Detail, "archive") {
			t.Errorf("result = %+v, want failed naming the archive setting", res)
		}
		mustNotExist(t, path, "the answered archive request")
	})

	t.Run("a misconfigured archive directory is answered failed", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveDir = "relative/archive"
		path := writeArchiveRequestFile(t, d, "run-relative", outsideRequestNow)

		d.runScheduledMaintenance()

		res := readArchiveAnswer(t, d)
		if res.Outcome != "failed" || !strings.Contains(res.Detail, "misconfigured") {
			t.Errorf("result = %+v, want failed naming the misconfigured directory", res)
		}
		mustNotExist(t, path, "the answered archive request")
	})

	t.Run("no databases is answered failed", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow)
		path := writeArchiveRequestFile(t, d, "run-nodb", outsideRequestNow)

		d.runScheduledMaintenance()

		res := readArchiveAnswer(t, d)
		if res.Outcome != "failed" || !strings.Contains(res.Detail, "no databases") {
			t.Errorf("result = %+v, want failed naming the missing databases", res)
		}
		mustNotExist(t, path, "the answered archive request")
	})

	t.Run("a remote server is answered failed", func(t *testing.T) {
		t.Parallel()
		d, _, _ := archiveRequestDaemon(t, outsideRequestNow, "hq")
		d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
		path := writeArchiveRequestFile(t, d, "run-remote", outsideRequestNow)

		d.runScheduledMaintenance()

		res := readArchiveAnswer(t, d)
		if res.Outcome != "failed" || !strings.Contains(res.Detail, "local server") {
			t.Errorf("result = %+v, want failed naming the local-server requirement", res)
		}
		mustNotExist(t, path, "the answered archive request")
	})

	t.Run("a result write failure leaves the request", func(t *testing.T) {
		t.Parallel()
		d, _, cf := archiveRequestDaemon(t, outsideRequestNow, "hq")
		cf.commits = map[string][]archiveCommit{
			"hq": {{Hash: "bbb222", Committer: "operator", Date: outsideRequestNow.Add(-time.Hour), Message: "hq: one"}},
		}
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// A directory where the result belongs makes the write fail.
		if err := os.Mkdir(archiveResultPath(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		path := writeArchiveRequestFile(t, d, "run-unwritten", outsideRequestNow)

		d.runScheduledMaintenance()

		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request was removed although no result was written: %v", err)
		}
	})
}
