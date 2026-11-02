package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The archive step runs through the gc fakes plus one seam per read (dolt_log
// and events): no SQL connection and no real database. The archive directory is
// a t.TempDir().

// archiveConfig is a config whose window is open at the fake clock's reading and
// whose history archive writes to dir.
func archiveConfig(dir string) *DaemonPatrolConfig {
	cfg := gcModeConfig()
	cfg.Patrols.ScheduledMaintenance.ArchiveDir = dir
	return cfg
}

// commitFake is the dolt_log read seam: the commits a database's fake log
// holds and a per-database read error. It applies the archive's cursor the way
// the production read does: commits dated at or after the watermark's date,
// minus the hashes the watermark already holds, oldest first by (date, hash).
type commitFake struct {
	commits map[string][]archiveCommit
	errs    map[string]error
	reads   int
}

func (f *commitFake) read(_ context.Context, _ *Daemon, db string, after ArchiveWatermark) ([]archiveCommit, error) {
	f.reads++
	if err := f.errs[db]; err != nil {
		return nil, err
	}
	var out []archiveCommit
	for _, c := range f.commits[db] {
		if !after.Date.IsZero() && c.Date.Before(after.Date) {
			continue
		}
		if after.covers(c.Hash) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].Hash < out[j].Hash
	})
	return out, nil
}

// withArchiveFakes supplies both reads, recording "archive <db>" in the event
// trail so a test can see where the step ran. The events read returns nothing
// unless a test installs rows through withEventsFake.
func withArchiveFakes(d *Daemon, f *gcFakes, commits map[string][]archiveCommit, errs map[string]error) *commitFake {
	fake := &commitFake{commits: commits, errs: errs}
	d.maint.archiveRead = func(ctx context.Context, d *Daemon, db string, after ArchiveWatermark) ([]archiveCommit, error) {
		f.events = append(f.events, "archive "+db)
		return fake.read(ctx, d, db, after)
	}
	withEventsFake(d, nil, nil, nil)
	return fake
}

// eventsFake is the events read seam: columns, the rows a database holds (in
// created_at, id order) and a per-database read error. It pages like the
// production query, returning only rows after the cursor and at most limit of
// them.
type eventsFake struct {
	columns []string
	rows    map[string][][]string
	errs    map[string]error
	reads   int
}

// withEventsFake installs the events read seam. columns and rows may be nil,
// which serves no rows.
func withEventsFake(d *Daemon, columns []string, rows map[string][][]string, errs map[string]error) *eventsFake {
	f := &eventsFake{columns: columns, rows: rows, errs: errs}
	d.maint.archiveEventsRead = func(_ context.Context, _ *Daemon, db string, after EventsArchiveWatermark, limit int) (archiveEvents, error) {
		f.reads++
		if err := f.errs[db]; err != nil {
			return archiveEvents{}, err
		}
		if len(f.columns) == 0 {
			return archiveEvents{}, nil // nothing installed: the database has no new rows
		}
		createdAt, id, err := eventsWatermarkColumns(f.columns)
		if err != nil {
			return archiveEvents{}, err
		}
		batch := archiveEvents{Columns: f.columns}
		for _, row := range f.rows[db] {
			if len(batch.Rows) == limit {
				break
			}
			if !eventsRowAfter(row[createdAt], row[id], after) {
				continue
			}
			batch.Rows = append(batch.Rows, row)
		}
		return batch, nil
	}
	return f
}

// eventsRowAfter reports whether a row's (created_at, id) cursor is past after,
// the comparison the production WHERE clause makes.
func eventsRowAfter(createdAt, id string, after EventsArchiveWatermark) bool {
	if after.CreatedAt == "" {
		return true
	}
	if createdAt != after.CreatedAt {
		return createdAt > after.CreatedAt
	}
	a, errA := strconv.ParseInt(id, 10, 64)
	b, errB := strconv.ParseInt(after.ID, 10, 64)
	return errA == nil && errB == nil && a > b
}

// readArchiveCSV reads one archive file back as CSV rows.
func readArchiveCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("reading %s as gzip: %v", path, err)
	}
	defer gz.Close()
	rows, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		t.Fatalf("reading %s as CSV: %v", path, err)
	}
	return rows
}

func csvRowsEqual(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !slices.Equal(got[i], want[i]) {
			return false
		}
	}
	return true
}

// archiveFile is the commit archive path for a database on the fake clock's
// UTC day.
func archiveFile(dir, db string) string {
	return filepath.Join(dir, archiveFileName("dolt-log", db, gcTestNow))
}

// eventsArchiveFile is the events archive path for a database on the fake
// clock's UTC day.
func eventsArchiveFile(dir, db string) string {
	return filepath.Join(dir, archiveFileName("events", db, gcTestNow))
}

// TestMaintenanceArchivesNewCommitsBeforeGc: a database's new commits land in
// the day's gzip CSV, read back row for row; its watermark becomes the last
// commit written; a database with nothing new writes no file; and every gc runs
// after the archive step.
func TestMaintenanceArchivesNewCommitsBeforeGc(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)

	commits := []archiveCommit{
		{Hash: "h1", Committer: "gastown/crew/overseer", Date: gcTestNow.Add(-2 * time.Hour), Message: "first"},
		{Hash: "h2", Committer: "gastown/polecats/cheedo", Date: gcTestNow.Add(-time.Hour), Message: "second, with a comma"},
		{Hash: "h3", Committer: "gastown/polecats/furiosa", Date: gcTestNow.Add(-time.Hour), Message: "third, same second"},
	}
	withArchiveFakes(d, f, map[string][]archiveCommit{"gt": commits}, nil)

	d.runScheduledMaintenance()

	want := [][]string{
		archiveCSVHeader,
		{"h1", "gastown/crew/overseer", commits[0].Date.UTC().Format(time.RFC3339), "first"},
		{"h2", "gastown/polecats/cheedo", commits[1].Date.UTC().Format(time.RFC3339), "second, with a comma"},
		{"h3", "gastown/polecats/furiosa", commits[2].Date.UTC().Format(time.RFC3339), "third, same second"},
	}
	if got := readArchiveCSV(t, archiveFile(dir, "gt")); !csvRowsEqual(got, want) {
		t.Errorf("archive rows = %v\nwant %v", got, want)
	}

	// hq had no new commits: no file, and no watermark.
	if _, err := os.Stat(archiveFile(dir, "hq")); !os.IsNotExist(err) {
		t.Errorf("hq with no new commits wrote %s (stat err %v)", archiveFile(dir, "hq"), err)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	// The two commits sharing the newest date are both in the cursor's hash
	// set, so the next read neither repeats nor skips either.
	if wm := st.ArchiveWatermarks["gt"]; !wm.Date.Equal(commits[1].Date) || !slices.Equal(wm.Hashes, []string{"h2", "h3"}) {
		t.Errorf("gt watermark = %+v, want the date %v with hashes [h2 h3]", wm, commits[1].Date)
	}
	if _, ok := st.ArchiveWatermarks["hq"]; ok {
		t.Errorf("hq watermark recorded with no commits exported: %+v", st.ArchiveWatermarks["hq"])
	}

	// The archive step precedes every gc call.
	lastArchive, firstGC := -1, -1
	for i, e := range f.events {
		if strings.HasPrefix(e, "archive ") {
			lastArchive = i
		}
		if strings.HasPrefix(e, "gc ") && firstGC < 0 {
			firstGC = i
		}
	}
	if lastArchive < 0 || firstGC < 0 || firstGC < lastArchive {
		t.Errorf("events = %v; want every archive before every gc", f.events)
	}
	if len(f.gcCalls) != 2 {
		t.Errorf("gc calls = %v, want both databases", f.gcCalls)
	}
}

// TestMaintenanceCommitCursorSurvivesAFlatten: the commit cursor is a date
// plus the hashes archived at it, not commit_order. A flatten replaces the
// history with one root commit dated after the watermark; the run archives it
// and every commit after it exactly once, and a second run over the same log
// repeats neither the boundary commits nor anything below them (gt-7nxj4).
func TestMaintenanceCommitCursorSurvivesAFlatten(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)

	// Before the flatten the archive had written through this date; those
	// commits no longer exist, and a height cursor would now be far ahead of
	// the rebuilt history's heights.
	if err := recordArchiveWatermark(d.config.TownRoot, "gt",
		ArchiveWatermark{Date: gcTestNow.Add(-72 * time.Hour), Hashes: []string{"old-boundary"}}); err != nil {
		t.Fatalf("seeding the pre-flatten watermark: %v", err)
	}

	root := gcTestNow.Add(-2 * time.Hour)
	log := []archiveCommit{
		{Hash: "root", Committer: "c", Date: root, Message: "flattened history"},
		{Hash: "n1", Committer: "c", Date: root.Add(time.Minute), Message: "after the flatten"},
		{Hash: "n2", Committer: "c", Date: root.Add(2 * time.Minute), Message: "latest"},
		{Hash: "n2b", Committer: "c", Date: root.Add(2 * time.Minute), Message: "same second as latest"},
	}
	fake := withArchiveFakes(d, f, map[string][]archiveCommit{"gt": log}, nil)

	d.runScheduledMaintenance()

	want := [][]string{
		archiveCSVHeader,
		{"root", "c", root.UTC().Format(time.RFC3339), "flattened history"},
		{"n1", "c", root.Add(time.Minute).UTC().Format(time.RFC3339), "after the flatten"},
		{"n2", "c", root.Add(2 * time.Minute).UTC().Format(time.RFC3339), "latest"},
		{"n2b", "c", root.Add(2 * time.Minute).UTC().Format(time.RFC3339), "same second as latest"},
	}
	if got := readArchiveCSV(t, archiveFile(dir, "gt")); !csvRowsEqual(got, want) {
		t.Errorf("archive after the flatten = %v\nwant %v", got, want)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if wm := st.ArchiveWatermarks["gt"]; !wm.Date.Equal(root.Add(2*time.Minute)) || !slices.Equal(wm.Hashes, []string{"n2", "n2b"}) {
		t.Errorf("watermark = %+v, want the newest date with both hashes written at it", wm)
	}

	// A second run over the same log must not append anything: every commit
	// at or above the cursor is already in its hash set.
	before, err := os.Stat(archiveFile(dir, "gt"))
	if err != nil {
		t.Fatal(err)
	}
	firstReads := fake.reads
	d.lastMaintenanceRun = time.Time{}
	d.runScheduledMaintenance()
	if fake.reads == firstReads {
		t.Fatal("the second run never read dolt_log; the size check below would prove nothing")
	}
	after, err := os.Stat(archiveFile(dir, "gt"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Errorf("the second run appended to the day's file (%d -> %d bytes); the cursor repeated rows", before.Size(), after.Size())
	}
}

// TestArchiveWatermarkValidate: a cursor is usable only when it says both the
// date and the hashes written at it; text it cannot place in the log is
// refused rather than read as the start of the log.
func TestArchiveWatermarkValidate(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		wm      ArchiveWatermark
		wantErr bool
	}{
		{"nothing archived yet", ArchiveWatermark{}, false},
		{"date and hashes", ArchiveWatermark{Date: when, Hashes: []string{"a", "b"}}, false},
		{"date with no hashes", ArchiveWatermark{Date: when}, true},
		{"hashes with no date", ArchiveWatermark{Hashes: []string{"a"}}, true},
		{"an empty hash", ArchiveWatermark{Date: when, Hashes: []string{""}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.wm.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", tt.wm)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate(%+v): %v", tt.wm, err)
			}
		})
	}
}

// TestMaintenanceArchiveAdvancesTheWatermarkOnlyAfterTheFileIsClosed: while the
// member is being appended — which includes the close — the state file holds no
// watermark yet; one is recorded only after the append returns. An append that
// fails leaves the watermark where it was.
func TestMaintenanceArchiveAdvancesTheWatermarkOnlyAfterTheFileIsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := []archiveCommit{{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "first"}}

	t.Run("the watermark is absent while the member is appended", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		dir := t.TempDir()
		statePath := maintenanceGCStatePath(d.config.TownRoot)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			return rows, nil
		}
		var stateAtAppend []byte
		var appended bool
		d.maint.archiveAppend = func(path string, columns []string, rowValues [][]string) error {
			appended = true
			var readErr error
			stateAtAppend, readErr = os.ReadFile(statePath)
			// A missing state file is the expected first run; any other error
			// would make the assertion below vacuous.
			if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
				t.Errorf("reading the state file while appending: %v", readErr)
			}
			return appendArchiveTable(path, columns, rowValues)
		}

		if err := d.maintenanceArchiveCommits(ctx, dir, "gt", ArchiveWatermark{}); err != nil {
			t.Fatalf("export: %v", err)
		}
		if !appended {
			t.Fatal("the export never appended")
		}
		if bytes.Contains(stateAtAppend, []byte(`"h1"`)) {
			t.Errorf("the watermark was recorded before the archive file was closed: %s", stateAtAppend)
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if wm := st.ArchiveWatermarks["gt"]; !slices.Equal(wm.Hashes, []string{"h1"}) {
			t.Errorf("watermark = %+v, want h1 recorded after the close", wm)
		}
	})

	t.Run("a failed append records no watermark", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			return rows, nil
		}
		d.maint.archiveAppend = func(string, []string, [][]string) error {
			return errors.New("close failed")
		}
		if err := d.maintenanceArchiveCommits(ctx, t.TempDir(), "gt", ArchiveWatermark{}); err == nil {
			t.Fatal("an append that failed reported success")
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if _, ok := st.ArchiveWatermarks["gt"]; ok {
			t.Errorf("a failed append still recorded a watermark: %+v", st.ArchiveWatermarks["gt"])
		}
	})
}

// TestMaintenanceArchiveAppendsToTodaysFile: a second run on the same UTC day
// appends another gzip member instead of truncating the day's history.
func TestMaintenanceArchiveAppendsToTodaysFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := [][]string{{"h1", "c", gcTestNow.Add(-2 * time.Hour).UTC().Format(time.RFC3339), "first"}}
	if err := appendArchiveTable(archiveFile(dir, "gt"), archiveCSVHeader, first); err != nil {
		t.Fatalf("first append: %v", err)
	}
	second := [][]string{{"h2", "c", gcTestNow.Add(-time.Hour).UTC().Format(time.RFC3339), "second"}}
	if err := appendArchiveTable(archiveFile(dir, "gt"), archiveCSVHeader, second); err != nil {
		t.Fatalf("second append: %v", err)
	}

	rows := readArchiveCSV(t, archiveFile(dir, "gt"))
	if len(rows) != 3 {
		t.Fatalf("rows = %v, want one header and two commits", rows)
	}
	if rows[1][0] != "h1" || rows[2][0] != "h2" {
		t.Errorf("rows = %v, want h1 then h2", rows)
	}
	if strings.Join(rows[0], ",") == strings.Join(rows[1], ",") {
		t.Error("second append repeated the header")
	}
}

// TestMaintenanceArchiveLeavesWatermarkPutWhenTheFileCannotBeWritten: the
// watermark moves only after the file is written and closed, so a write that
// fails leaves it where it was and the next run exports those commits again.
func TestMaintenanceArchiveLeavesWatermarkPutWhenTheFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)

	// gt's target file is a directory, so opening it for append fails.
	if err := os.Mkdir(archiveFile(dir, "gt"), 0o755); err != nil {
		t.Fatal(err)
	}
	withArchiveFakes(d, f, map[string][]archiveCommit{
		"gt": {{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "first"}},
		"hq": {{Hash: "q1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "hq first"}},
	}, nil)

	d.runScheduledMaintenance()

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if _, ok := st.ArchiveWatermarks["gt"]; ok {
		t.Errorf("gt watermark advanced although its file could not be written: %+v", st.ArchiveWatermarks["gt"])
	}
	if wm := st.ArchiveWatermarks["hq"]; !slices.Equal(wm.Hashes, []string{"q1"}) {
		t.Errorf("hq watermark = %+v, want q1 (one database's write failure must not stop the others)", wm)
	}
	if !strings.Contains(logs.String(), "archive gt dolt_log: FAILED") {
		t.Errorf("log does not name gt's failed archive:\n%s", logs.String())
	}
}

// TestMaintenanceArchiveFailureKeepsWatermarkAndOtherDatabasesRun: a database
// whose dolt_log read fails keeps its watermark, is named in the log and the
// run's one attention item, its gc is skipped, and the other databases are
// archived and gc'd.
func TestMaintenanceArchiveFailureKeepsWatermarkAndOtherDatabasesRun(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)

	old := ArchiveWatermark{Date: gcTestNow.Add(-48 * time.Hour), Hashes: []string{"old-gt"}}
	if err := recordArchiveWatermark(d.config.TownRoot, "gt", old); err != nil {
		t.Fatalf("seeding gt's watermark: %v", err)
	}
	withArchiveFakes(d, f,
		map[string][]archiveCommit{"hq": {{Hash: "q1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "hq first"}}},
		map[string]error{"gt": errors.New("connection refused")})

	d.runScheduledMaintenance()

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if wm := st.ArchiveWatermarks["gt"]; !wm.Date.Equal(old.Date) || !slices.Equal(wm.Hashes, old.Hashes) {
		t.Errorf("gt watermark = %+v, want its old one kept %+v", wm, old)
	}
	if wm := st.ArchiveWatermarks["hq"]; !slices.Equal(wm.Hashes, []string{"q1"}) {
		t.Errorf("hq watermark = %+v, want q1", wm)
	}
	if rows := readArchiveCSV(t, archiveFile(dir, "hq")); len(rows) != 2 {
		t.Errorf("hq archive rows = %v, want header and one commit", rows)
	}
	if !strings.Contains(logs.String(), "archive gt dolt_log: FAILED") || !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("log does not name gt's failed archive and why:\n%s", logs.String())
	}
	if len(f.escalations) != 1 {
		t.Fatalf("escalations = %v, want one attention item for the run", f.escalations)
	}
	if !strings.Contains(f.escalations[0], "gt dolt_log: reading dolt_log: connection refused") {
		t.Errorf("attention item = %q, want it to name gt's dolt_log export", f.escalations[0])
	}
	// History is not collected for a database whose archive step did not
	// finish; the other database's gc still runs, and the skip is logged by
	// name.
	if strings.Join(f.gcCalls, ",") != "hq" {
		t.Errorf("gc calls = %v, want hq only (gt's archive failed)", f.gcCalls)
	}
	if !strings.Contains(logs.String(), "gc gt: skipped") {
		t.Errorf("log does not name gt's skipped gc:\n%s", logs.String())
	}
}

// TestMaintenanceArchiveOffWithoutArchiveDir: with archive_dir unset the step
// logs once per run, reads nothing, writes nothing, and the gc still runs.
func TestMaintenanceArchiveOffWithoutArchiveDir(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	d.patrolConfig = gcModeConfig() // no archive_dir
	d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
		t.Error("the dolt_log read ran with archive_dir unset")
		return nil, nil
	}
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
		t.Error("the events read ran with archive_dir unset")
		return archiveEvents{}, nil
	}

	d.runScheduledMaintenance()

	if got := strings.Count(logs.String(), "history archiving is off"); got != 1 {
		t.Errorf("log holds %d 'history archiving is off' lines, want one per run:\n%s", got, logs.String())
	}
	if len(f.gcCalls) != 1 {
		t.Errorf("gc calls = %v, want gt (archiving off must not stop the gc)", f.gcCalls)
	}
	if len(f.escalations) != 0 {
		t.Errorf("archiving off escalated: %v", f.escalations)
	}
}

// TestMaintenanceArchiveRefusesUnusableDirectory: a directory the daemon cannot
// use, and a watermark file it cannot read, each archive nothing and raise one
// attention item; neither reads a dolt_log.
func TestMaintenanceArchiveRefusesUnusableDirectory(t *testing.T) {
	t.Parallel()
	t.Run("relative directory", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		d.patrolConfig = archiveConfig("relative/archive")
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			t.Error("the dolt_log read ran with a relative archive_dir")
			return nil, nil
		}

		d.runScheduledMaintenance()

		if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "maintenance.archive_dir") {
			t.Errorf("escalations = %v, want one naming maintenance.archive_dir", f.escalations)
		}
		if !strings.Contains(logs.String(), "want an absolute path") {
			t.Errorf("log does not say the directory was refused:\n%s", logs.String())
		}
	})

	t.Run("uncreatable directory", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		parent := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		d.patrolConfig = archiveConfig(filepath.Join(parent, "archive"))
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			t.Error("the dolt_log read ran with an archive directory that cannot be created")
			return nil, nil
		}

		d.runScheduledMaintenance()

		if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "cannot create") {
			t.Errorf("escalations = %v, want one saying the directory cannot be created", f.escalations)
		}
	})

	t.Run("corrupt watermark file", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		d.patrolConfig = archiveConfig(t.TempDir())
		path := maintenanceGCStatePath(d.config.TownRoot)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			t.Error("the dolt_log read ran with an unreadable watermark file")
			return nil, nil
		}

		d.runScheduledMaintenance()

		if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "cannot read the watermarks") {
			t.Errorf("escalations = %v, want one saying the watermarks cannot be read", f.escalations)
		}
		if !strings.Contains(logs.String(), "no history archived this run") {
			t.Errorf("log does not say nothing was archived:\n%s", logs.String())
		}
	})
}

// TestMaintenanceArchiveRollsBackWhenTheWatermarkCannotBeRecorded: the file is
// written but the watermark cannot be recorded, so the run rolls the member
// back — the next run re-exports those commits instead of appending them twice
// — reports the failure, and does not report success.
func TestMaintenanceArchiveRollsBackWhenTheWatermarkCannotBeRecorded(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	statePath := maintenanceGCStatePath(d.config.TownRoot)
	d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
		// The state file becomes a directory after the run's load, so the
		// watermark write fails while the archive file itself still can be
		// written.
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			t.Errorf("creating the daemon dir: %v", err)
		}
		if err := os.Mkdir(statePath, 0o755); err != nil {
			t.Errorf("making the state path unwritable: %v", err)
		}
		return []archiveCommit{{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "first"}}, nil
	}

	d.runScheduledMaintenance()

	if !strings.Contains(logs.String(), "watermark for") || !strings.Contains(logs.String(), "cannot be recorded") {
		t.Errorf("log does not report the unrecorded watermark:\n%s", logs.String())
	}
	if len(f.escalations) != 1 {
		t.Errorf("escalations = %v, want one for the failed archive", f.escalations)
	}
	if _, err := os.Stat(archiveFile(dir, "gt")); !os.IsNotExist(err) {
		t.Errorf("the rolled-back export left %s behind (stat err %v)", archiveFile(dir, "gt"), err)
	}
}

// TestLoadArchiveWatermarks: both watermarks survive a write and read, and an
// unreadable file is an error rather than an empty set that reads as "never
// archived".
func TestLoadArchiveWatermarks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	wm := ArchiveWatermark{Date: gcTestNow, Hashes: []string{"abc123", "def456"}}
	if err := recordArchiveWatermark(town, "gt", wm); err != nil {
		t.Fatalf("recording commit watermark: %v", err)
	}
	events := EventsArchiveWatermark{CreatedAt: "2026-10-01 03:00:00", ID: "42"}
	if err := recordEventsArchiveWatermark(town, "gt", events); err != nil {
		t.Fatalf("recording events watermark: %v", err)
	}
	got, err := LoadArchiveWatermarks(town)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if !got.Commits["gt"].Date.Equal(wm.Date) || !slices.Equal(got.Commits["gt"].Hashes, wm.Hashes) {
		t.Errorf("commit watermarks = %+v, want gt=%+v", got.Commits, wm)
	}
	if got.Events["gt"] != events {
		t.Errorf("events watermarks = %+v, want gt=%+v", got.Events, events)
	}

	corrupt := t.TempDir()
	path := maintenanceGCStatePath(corrupt)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchiveWatermarks(corrupt); err == nil {
		t.Error("corrupt state file loaded as an empty watermark set")
	}
}

// TestMaintenanceArchivesNewEventsRows: the events rows past the watermark land
// in events-<db>-<date>.csv.gz with every column, in created_at then id order;
// the watermark moves only once the file is closed; a failed events export
// keeps its watermark while the commit-log export still runs; and a first
// export larger than one batch pages to the end.
func TestMaintenanceArchivesNewEventsRows(t *testing.T) {
	t.Parallel()
	columns := []string{"id", "issue_id", "event_type", "actor", "old_value", "new_value", "created_at"}
	rows := [][]string{
		{"1", "gt-a", "created", "crew/overseer", "", "", "2026-09-30 10:00:00"},
		{"2", "gt-a", "status", "cheedo", "open", "closed", "2026-10-01 02:00:00"},
	}
	lastCursor := EventsArchiveWatermark{CreatedAt: rows[1][6], ID: rows[1][0]}

	t.Run("every column in created_at then id order", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		withArchiveFakes(d, f, nil, nil)
		withEventsFake(d, columns, map[string][][]string{"gt": rows}, nil)

		d.runScheduledMaintenance()

		want := append([][]string{columns}, rows...)
		if got := readArchiveCSV(t, eventsArchiveFile(dir, "gt")); !csvRowsEqual(got, want) {
			t.Errorf("events archive = %v\nwant %v", got, want)
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if got := st.ArchiveEventsWatermarks["gt"]; got != lastCursor {
			t.Errorf("events watermark = %+v, want %+v", got, lastCursor)
		}
		if len(f.escalations) != 0 {
			t.Errorf("successful events archive escalated: %v", f.escalations)
		}
	})

	t.Run("a failed write leaves the events watermark put", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		withArchiveFakes(d, f, nil, nil)
		withEventsFake(d, columns, map[string][][]string{"gt": rows}, nil)
		// The target file is a directory, so opening it for append fails.
		if err := os.Mkdir(eventsArchiveFile(dir, "gt"), 0o755); err != nil {
			t.Fatal(err)
		}

		d.runScheduledMaintenance()

		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if _, ok := st.ArchiveEventsWatermarks["gt"]; ok {
			t.Errorf("events watermark advanced although its file could not be written: %+v", st.ArchiveEventsWatermarks["gt"])
		}
		if !strings.Contains(logs.String(), "archive gt events: FAILED") {
			t.Errorf("log does not name the failed events archive:\n%s", logs.String())
		}
	})

	t.Run("a failed events export keeps its watermark and the commit export runs", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		old := EventsArchiveWatermark{CreatedAt: "2026-09-01 00:00:00", ID: "7"}
		if err := recordEventsArchiveWatermark(d.config.TownRoot, "gt", old); err != nil {
			t.Fatalf("seeding the events watermark: %v", err)
		}
		withArchiveFakes(d, f,
			map[string][]archiveCommit{"gt": {{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "first"}}},
			nil)
		withEventsFake(d, columns, nil, map[string]error{"gt": errors.New("connection refused")})

		d.runScheduledMaintenance()

		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if got := st.ArchiveEventsWatermarks["gt"]; got != old {
			t.Errorf("events watermark = %+v, want its old one kept %+v", got, old)
		}
		if wm := st.ArchiveWatermarks["gt"]; !slices.Equal(wm.Hashes, []string{"h1"}) {
			t.Errorf("commit watermark = %+v, want h1 (the commit export still runs)", wm)
		}
		if rows := readArchiveCSV(t, archiveFile(dir, "gt")); len(rows) != 2 {
			t.Errorf("commit archive rows = %v, want header and one commit", rows)
		}
		if !strings.Contains(logs.String(), "archive gt events: FAILED") || !strings.Contains(logs.String(), "connection refused") {
			t.Errorf("log does not name the failed events archive and why:\n%s", logs.String())
		}
		if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "gt events: reading events: connection refused") {
			t.Errorf("escalations = %v, want one naming gt's events export", f.escalations)
		}
	})

	t.Run("a first export longer than one batch pages to the end", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		withArchiveFakes(d, f, nil, nil)

		base := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
		many := make([][]string, maintenanceArchiveEventsBatch+1)
		for i := range many {
			many[i] = []string{strconv.Itoa(i + 1), "gt-a", "x", "c", "", "",
				base.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")}
		}
		fake := withEventsFake(d, columns, map[string][][]string{"gt": many}, nil)

		d.runScheduledMaintenance()

		if fake.reads < 2 {
			t.Errorf("events read ran %d time(s), want at least two batches", fake.reads)
		}
		if got := readArchiveCSV(t, eventsArchiveFile(dir, "gt")); len(got) != len(many)+1 {
			t.Errorf("events archive holds %d row(s), want %d rows and one header", len(got)-1, len(many))
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		last := many[len(many)-1]
		if got := st.ArchiveEventsWatermarks["gt"]; got.CreatedAt != last[6] || got.ID != last[0] {
			t.Errorf("events watermark = %+v, want %s/%s", got, last[6], last[0])
		}
	})

	t.Run("a read that never advances stops the export", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		d.patrolConfig = archiveConfig(t.TempDir())
		withArchiveFakes(d, f, nil, nil)

		// A full batch repeated whatever cursor it is given: the loop must stop
		// rather than page for ever.
		stuck := make([][]string, maintenanceArchiveEventsBatch)
		for i := range stuck {
			stuck[i] = []string{strconv.Itoa(i + 1), "gt-a", "x", "c", "", "", "2026-09-30 10:00:00"}
		}
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
			return archiveEvents{Columns: columns, Rows: stuck}, nil
		}

		d.runScheduledMaintenance()

		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if _, ok := st.ArchiveEventsWatermarks["gt"]; ok {
			t.Errorf("a read that never advanced still moved the watermark: %+v", st.ArchiveEventsWatermarks["gt"])
		}
		if !strings.Contains(logs.String(), "made no progress") {
			t.Errorf("log does not report the stalled read:\n%s", logs.String())
		}
	})
}

// TestEventsArchiveWatermarkValidate: a cursor is usable only when Dolt can
// compare it; text it cannot compare is refused rather than matched against no
// row and read as "nothing new".
func TestEventsArchiveWatermarkValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		wm      EventsArchiveWatermark
		wantErr bool
	}{
		{"nothing archived yet", EventsArchiveWatermark{}, false},
		{"timestamp and id", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00", ID: "42"}, false},
		{"timestamp with fraction", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00.123456", ID: "42"}, false},
		{"text is not a timestamp", EventsArchiveWatermark{CreatedAt: "yesterday", ID: "42"}, true},
		{"missing created_at", EventsArchiveWatermark{ID: "42"}, true},
		{"id is not a number", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00", ID: "abc"}, true},
		{"missing id", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.wm.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", tt.wm)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate(%+v): %v", tt.wm, err)
			}
		})
	}
}

// TestEventsArchiveColumns: the watermark is built from the created_at and id
// columns, and an events table without them, or a row too short to hold them,
// is refused rather than archived with a cursor it cannot advance.
func TestEventsArchiveColumns(t *testing.T) {
	t.Parallel()
	if _, _, err := eventsWatermarkColumns([]string{"id", "issue_id", "created_at"}); err != nil {
		t.Errorf("columns with id and created_at refused: %v", err)
	}
	if _, _, err := eventsWatermarkColumns([]string{"id", "issue_id"}); err == nil {
		t.Error("columns without created_at accepted")
	}
	if _, _, err := eventsWatermarkColumns([]string{"created_at", "issue_id"}); err == nil {
		t.Error("columns without id accepted")
	}
	if _, err := eventsWatermarkFrom([]string{"id", "created_at"}, []string{"1"}); err == nil {
		t.Error("a short row was turned into a watermark")
	}
	if _, err := eventsWatermarkFrom([]string{"id"}, []string{"1"}); err == nil {
		t.Error("a watermark was built from columns without created_at")
	}
	if _, err := eventsWatermarkFrom([]string{"created_at", "id"}, []string{"yesterday", "1"}); err == nil {
		t.Error("a watermark was built from text Dolt cannot compare")
	}
}

// TestArchiveMemberRefusesRowWidthMismatch: a row whose values do not line up
// with the header is refused, so the CSV cannot be written with a column's
// value under another column's name.
func TestArchiveMemberRefusesRowWidthMismatch(t *testing.T) {
	t.Parallel()
	_, err := archiveMember([]string{"a", "b"}, [][]string{{"1"}}, true)
	if err == nil {
		t.Fatal("a row with one value was written against two columns")
	}
}

// TestDoltArchiveEventsRefusesBadBounds: an unusable limit or watermark is
// refused before a connection is opened, so it cannot read as "nothing new".
func TestDoltArchiveEventsRefusesBadBounds(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	ctx := context.Background()
	if _, err := d.doltArchiveEvents(ctx, "gt", EventsArchiveWatermark{}, 0); err == nil {
		t.Error("a zero limit was accepted")
	}
	if _, err := d.doltArchiveEvents(ctx, "gt", EventsArchiveWatermark{CreatedAt: "yesterday", ID: "1"}, 10); err == nil {
		t.Error("an uncomparable watermark was accepted")
	}
	if _, err := d.doltArchiveEvents(ctx, "../gt", EventsArchiveWatermark{}, 10); err == nil {
		t.Error("a path-escaping database name was accepted")
	}
}

// failingAppendTarget stands in for the archive file when the write itself
// fails: it refuses the bytes and records the truncate that rolls them back.
type failingAppendTarget struct {
	truncated int64
	closed    bool
}

func (f *failingAppendTarget) Write([]byte) (int, error) {
	return 0, errors.New("no space left on device")
}
func (f *failingAppendTarget) Truncate(size int64) error { f.truncated = size; return nil }
func (f *failingAppendTarget) Sync() error               { return nil }
func (f *failingAppendTarget) Close() error              { f.closed = true; return nil }

// TestAppendArchiveMemberRollsBackAPartialWrite: a write that fails mid-member
// is cut back to the size the file had, so the day's archive stays a sequence
// of whole gzip members, and the close is still done.
func TestAppendArchiveMemberRollsBackAPartialWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dolt-log-gt-20261001.csv.gz")
	const existing = "an earlier member"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	target := &failingAppendTarget{}

	err := appendArchiveMember(path,
		archiveCSVHeader,
		[][]string{{"h1", "c", gcTestNow.Add(-time.Hour).UTC().Format(time.RFC3339), "first"}},
		func(string) (archiveAppendTarget, error) { return target, nil })

	if err == nil {
		t.Fatal("a write that failed reported success")
	}
	if target.truncated != int64(len(existing)) {
		t.Errorf("truncated to %d, want the pre-append size %d", target.truncated, len(existing))
	}
	if !target.closed {
		t.Error("failed append left the file open")
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != existing {
		t.Errorf("file = %q, %v; want the earlier member untouched", got, readErr)
	}
}

// TestAppendArchiveTableRefusesAnUnreadablePath: a path whose directory is a
// regular file is refused, not written through.
func TestAppendArchiveTableRefusesAnUnreadablePath(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := appendArchiveTable(filepath.Join(parent, "dolt-log-gt-20261001.csv.gz"), archiveCSVHeader,
		[][]string{{"h1", "c", gcTestNow.UTC().Format(time.RFC3339), "first"}})
	if err == nil {
		t.Fatal("append through a regular file reported success")
	}
}

// TestMaintenanceArchiveEventsRollsBackAFailedExport: a batch written before a
// later read fails is rolled back, so the file does not hold rows ahead of the
// unadvanced watermark the next run would read again (gt-7nxj4).
func TestMaintenanceArchiveEventsRollsBackAFailedExport(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, nil, nil) // the commit read returns nothing

	columns := []string{"id", "created_at"}
	first := make([][]string, maintenanceArchiveEventsBatch) // a full batch keeps the loop reading
	for i := range first {
		first[i] = []string{strconv.Itoa(i + 1), "2026-10-01 00:00:00"}
	}
	reads := 0
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
		reads++
		if reads == 1 {
			return archiveEvents{Columns: columns, Rows: first}, nil
		}
		return archiveEvents{}, errors.New("connection reset")
	}

	d.runScheduledMaintenance()

	if _, err := os.Stat(eventsArchiveFile(dir, "gt")); !os.IsNotExist(err) {
		t.Errorf("the failed export left %s behind (stat err %v)", eventsArchiveFile(dir, "gt"), err)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if _, ok := st.ArchiveEventsWatermarks["gt"]; ok {
		t.Errorf("a failed export advanced the events watermark: %+v", st.ArchiveEventsWatermarks["gt"])
	}
	if !strings.Contains(logs.String(), "archive gt events: FAILED") || !strings.Contains(logs.String(), "connection reset") {
		t.Errorf("log does not name the failed export and why:\n%s", logs.String())
	}
}

// TestMaintenanceArchiveDefersWhileTheTownIsBusy: the step asks the gc's
// quiet-window guard and defers the whole cycle when the town is busy, so the
// backup and gc wait for the retry instead of running ahead of an unarchived
// database (gt-7nxj4).
func TestMaintenanceArchiveDefersWhileTheTownIsBusy(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	f.quietUntil = 0 // busy on the first probe
	d.patrolConfig = archiveConfig(t.TempDir())
	d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
		t.Error("the commit read ran while the town was busy")
		return nil, nil
	}
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
		t.Error("the events read ran while the town was busy")
		return archiveEvents{}, nil
	}

	d.runScheduledMaintenance()

	if !strings.Contains(logs.String(), "archive held") {
		t.Errorf("log does not say the archive was held:\n%s", logs.String())
	}
	if len(f.backupCalls) != 0 || len(f.gcCalls) != 0 {
		t.Errorf("backup calls = %v, gc calls = %v; want neither while the archive is deferred", f.backupCalls, f.gcCalls)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st.PendingDeferral == nil || !strings.HasPrefix(st.PendingDeferral.Reason, "archive: ") {
		t.Errorf("pending deferral = %+v, want the archive's quiet-guard reason", st.PendingDeferral)
	}
}

// TestMaintenanceArchiveBoundsTheWholeStep: the context handed to the archive
// reads carries a deadline no later than the step's overall bound, so a board
// of many databases cannot hold the maintenance cycle indefinitely.
func TestMaintenanceArchiveBoundsTheWholeStep(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	d.patrolConfig = archiveConfig(t.TempDir())
	var deadline time.Time
	d.maint.archiveRead = func(ctx context.Context, _ *Daemon, _ string, _ ArchiveWatermark) ([]archiveCommit, error) {
		deadline, _ = ctx.Deadline()
		return nil, nil
	}
	withEventsFake(d, nil, nil, nil)

	d.runScheduledMaintenance()

	if deadline.IsZero() {
		t.Fatal("the archive read ran with no deadline")
	}
	if now := time.Now(); !deadline.After(now) || deadline.After(now.Add(maintenanceArchiveTimeout)) {
		t.Errorf("archive read deadline = %v, want it within %v of now", deadline, maintenanceArchiveTimeout)
	}
}

// TestMaintenanceArchiveReportsAFailedRollback: an export that wrote rows and
// then cannot undo them reports the failed rollback, so the leftover is visible
// rather than read as a clean export (gt-7nxj4).
func TestMaintenanceArchiveReportsAFailedRollback(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, nil, nil)

	columns := []string{"id", "created_at"}
	full := make([][]string, maintenanceArchiveEventsBatch) // a full batch keeps the loop reading
	for i := range full {
		full[i] = []string{strconv.Itoa(i + 1), "2026-10-01 00:00:00"}
	}
	path := eventsArchiveFile(dir, "gt")
	reads := 0
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
		reads++
		if reads == 1 {
			return archiveEvents{Columns: columns, Rows: full}, nil
		}
		// Replace the written file with a non-empty directory: the export's
		// rollback (remove, because it created the file) must fail and say so.
		if err := os.Remove(path); err != nil {
			t.Errorf("removing the written archive: %v", err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Errorf("replacing the archive with a directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o644); err != nil {
			t.Errorf("filling the replacement directory: %v", err)
		}
		return archiveEvents{}, errors.New("read failed")
	}

	d.runScheduledMaintenance()

	if !strings.Contains(logs.String(), "could not be rolled back") {
		t.Errorf("log does not report the failed rollback:\n%s", logs.String())
	}
}
