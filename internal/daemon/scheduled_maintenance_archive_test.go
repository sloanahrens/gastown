package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	gmssql "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"

	"github.com/steveyegge/gastown/internal/config"
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
// holds and a per-database read error. It applies the archive's selection the
// way the production read does: commits whose hashes no archive file holds,
// oldest first by (date, hash). TestCommitArchiveQuerySelectsByArchivedHashSet
// runs the production query itself against an in-process MySQL engine, so this
// fake only has to drive the export.
type commitFake struct {
	commits map[string][]archiveCommit
	errs    map[string]error
	reads   int
}

func (f *commitFake) read(_ context.Context, _ *Daemon, db string, archived map[string]struct{}) ([]archiveCommit, error) {
	f.reads++
	if err := f.errs[db]; err != nil {
		return nil, err
	}
	var out []archiveCommit
	for _, c := range f.commits[db] {
		if _, ok := archived[c.Hash]; ok {
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
	d.maint.archiveRead = func(ctx context.Context, d *Daemon, db string, archived map[string]struct{}) ([]archiveCommit, error) {
		f.events = append(f.events, "archive "+db)
		return fake.read(ctx, d, db, archived)
	}
	withEventsFake(d, nil, nil, nil)
	return fake
}

// eventsFake is the events read seam: columns, the rows a database holds (in
// created_at, id order) and a per-database read error. It returns the rows at
// or after the read's window start, the bound the production query applies;
// TestEventsArchiveQueryReadsTheLookbackWindow runs that query itself against
// an in-process MySQL engine, so this fake only has to drive the export.
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
	d.maint.archiveEventsRead = func(_ context.Context, _ *Daemon, db, since string) (archiveEvents, error) {
		f.reads++
		if err := f.errs[db]; err != nil {
			return archiveEvents{}, err
		}
		if len(f.columns) == 0 {
			return archiveEvents{}, nil // nothing installed: the database has no new rows
		}
		createdAt, _, err := eventsWatermarkColumns(f.columns)
		if err != nil {
			return archiveEvents{}, err
		}
		batch := archiveEvents{Columns: f.columns}
		for _, row := range f.rows[db] {
			if since != "" && row[createdAt] < since {
				continue
			}
			batch.Rows = append(batch.Rows, row)
		}
		return batch, nil
	}
	return f
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

// sameEventsWatermark compares two events watermarks field by field; the
// archived ids are a slice, so the struct is not comparable with ==.
func sameEventsWatermark(a, b EventsArchiveWatermark) bool {
	return a.CreatedAt == b.CreatedAt && slices.Equal(a.Archived, b.Archived)
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

// TestMaintenanceCommitExportSelectsByArchivedHashSet: the export selects the
// commits the pre-flatten check counts — the dolt_log hashes no archive file
// holds — so a commit dated before the watermark but missing from the files is
// archived on the next run, exactly once, and the check then finds every log
// hash in the set (gt-98eug).
func TestMaintenanceCommitExportSelectsByArchivedHashSet(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)

	// h1 was archived on an earlier day. h0 is dated before the watermark and
	// was never exported: the date cursor cannot see it, but the check counts
	// it, so the old design could never reach 0 behind.
	if err := appendArchiveTable(filepath.Join(dir, "dolt-log-gt-20260930.csv.gz"), archiveCSVHeader, [][]string{
		{"h1", "c", gcTestNow.Add(-72 * time.Hour).UTC().Format(time.RFC3339), "already archived"},
	}); err != nil {
		t.Fatalf("seeding the archive: %v", err)
	}
	if err := recordArchiveWatermark(d.config.TownRoot, "gt", ArchiveWatermark{
		Date:   gcTestNow.Add(-time.Hour),
		Hashes: []string{"h1"},
	}); err != nil {
		t.Fatalf("seeding the watermark: %v", err)
	}

	log := []archiveCommit{
		{Hash: "h0", Committer: "c", Date: gcTestNow.Add(-48 * time.Hour), Message: "backdated"},
		{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "already archived"},
		{Hash: "h2", Committer: "c", Date: gcTestNow.Add(-time.Minute), Message: "new"},
	}
	fake := withArchiveFakes(d, f, map[string][]archiveCommit{"gt": log}, nil)

	d.runScheduledMaintenance()

	want := [][]string{
		archiveCSVHeader,
		{"h0", "c", log[0].Date.UTC().Format(time.RFC3339), "backdated"},
		{"h2", "c", log[2].Date.UTC().Format(time.RFC3339), "new"},
	}
	if got := readArchiveCSV(t, archiveFile(dir, "gt")); !csvRowsEqual(got, want) {
		t.Errorf("archive rows = %v\nwant %v (the backdated commit exported, the archived one not repeated)", got, want)
	}

	archived, err := LoadArchivedCommitHashes(dir, "gt")
	if err != nil {
		t.Fatalf("loading the archived set: %v", err)
	}
	var missing int
	for _, c := range log {
		if _, ok := archived[c.Hash]; !ok {
			missing++
		}
	}
	if missing != 0 {
		t.Errorf("%d log hash(es) not in the archive after the export; want 0 (the check's rule)", missing)
	}

	// A second run over the same log appends nothing: the file set now holds
	// every hash.
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
		t.Errorf("the second run appended to the day's file (%d -> %d bytes); an archived hash was written twice", before.Size(), after.Size())
	}
}

// TestArchiveWatermarkValidate: a watermark is usable only when it says both
// the date and the hashes written at it; text it cannot place in the log is
// refused rather than shown as a clean archive.
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
		d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
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

		if _, _, _, err := d.maintenanceArchiveCommits(ctx, dir, "gt", ArchiveWatermark{}); err != nil {
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
		d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
			return rows, nil
		}
		d.maint.archiveAppend = func(string, []string, [][]string) error {
			return errors.New("close failed")
		}
		if _, _, _, err := d.maintenanceArchiveCommits(ctx, t.TempDir(), "gt", ArchiveWatermark{}); err == nil {
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

// TestMaintenanceArchiveFilesAreOwnerOnly: the archive holds the town's commit
// messages and event rows, so the export creates its files 0600 and its
// directory 0700.
func TestMaintenanceArchiveFilesAreOwnerOnly(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := filepath.Join(t.TempDir(), "archive")
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, map[string][]archiveCommit{
		"gt": {{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-time.Hour), Message: "first"}},
	}, nil)

	d.runScheduledMaintenance()

	file, err := os.Stat(archiveFile(dir, "gt"))
	if err != nil {
		t.Fatalf("stat archive file: %v", err)
	}
	if got := file.Mode().Perm(); got != 0o600 {
		t.Errorf("archive file mode = %04o, want 0600", got)
	}
	archiveDir, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat archive directory: %v", err)
	}
	if got := archiveDir.Mode().Perm(); got != 0o700 {
		t.Errorf("archive directory mode = %04o, want 0700", got)
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
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		t.Error("the dolt_log read ran with archive_dir unset")
		return nil, nil
	}
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
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
		d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
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
		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v, want none while no database was archived", f.gcCalls)
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
		d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
			t.Error("the dolt_log read ran with an archive directory that cannot be created")
			return nil, nil
		}

		d.runScheduledMaintenance()

		if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "cannot create") {
			t.Errorf("escalations = %v, want one saying the directory cannot be created", f.escalations)
		}
		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v, want none while no database was archived", f.gcCalls)
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
		d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
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
		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v, want none while no database was archived", f.gcCalls)
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
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
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
	// This test drives the commit export. The events read is injected too: a
	// run that reached its default would open a real connection, and the
	// failure it reported would be the connection's, not the watermark's.
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
		return archiveEvents{}, nil
	}
	d.maint.archiveOpenDB = func(string) (*sql.DB, error) {
		t.Error("the default events reader was reached")
		return nil, errors.New("not reached")
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
	events := EventsArchiveWatermark{
		CreatedAt: "2026-10-01 03:00:00",
		Archived:  []ArchivedEventsRow{{CreatedAt: "2026-10-01 03:00:00", ID: "42"}},
	}
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
	if !sameEventsWatermark(got.Events["gt"], events) {
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

// TestLoadArchivedCommitHashes: the pre-flatten check's set is every hash the
// database's commit archive files hold, across every gzip member. A file or a
// name it cannot account for is an error, never a smaller set that would call
// an unarchived commit safe; a database with no file yet is an empty set.
func TestLoadArchivedCommitHashes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := filepath.Join(dir, "dolt-log-gt-20261101.csv.gz")
	if err := appendArchiveTable(first, archiveCSVHeader, [][]string{
		{"h1", "c", "2026-11-01T00:00:00Z", "one"},
	}); err != nil {
		t.Fatalf("writing the first archive file: %v", err)
	}
	// The archive appends a second member to the same day's file.
	if err := appendArchiveTable(first, archiveCSVHeader, [][]string{
		{"h2", "c", "2026-11-01T01:00:00Z", "two"},
	}); err != nil {
		t.Fatalf("appending the second member: %v", err)
	}
	second := filepath.Join(dir, "dolt-log-gt-20261102.csv.gz")
	if err := appendArchiveTable(second, archiveCSVHeader, [][]string{
		{"h3", "c", "2026-11-02T00:00:00Z", "three"},
	}); err != nil {
		t.Fatalf("writing the next day's file: %v", err)
	}
	// Another database's file is not gt's.
	other := filepath.Join(dir, "dolt-log-hq-20261102.csv.gz")
	if err := appendArchiveTable(other, archiveCSVHeader, [][]string{
		{"h4", "c", "2026-11-02T00:00:00Z", "other db"},
	}); err != nil {
		t.Fatalf("writing another database's file: %v", err)
	}

	got, err := LoadArchivedCommitHashes(dir, "gt")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("archived hashes = %v, want h1, h2 and h3 across both files and both members", got)
	}
	for _, hash := range []string{"h1", "h2", "h3"} {
		if _, ok := got[hash]; !ok {
			t.Errorf("archived hashes = %v, want %s", got, hash)
		}
	}
	if _, ok := got["h4"]; ok {
		t.Error("another database's archive hash was loaded")
	}

	t.Run("a database with no file yet is an empty set", func(t *testing.T) {
		t.Parallel()
		got, err := LoadArchivedCommitHashes(dir, "be")
		if err != nil || len(got) != 0 {
			t.Errorf("archived hashes for a never-archived database = %v, %v; want an empty set", got, err)
		}
	})

	t.Run("a missing directory is refused", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadArchivedCommitHashes(filepath.Join(dir, "absent"), "gt"); err == nil {
			t.Error("a missing archive directory loaded as an empty set")
		}
	})

	t.Run("a path-like database name is refused", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadArchivedCommitHashes(dir, "../gt"); err == nil {
			t.Error("a path-like database name was read")
		}
	})

	t.Run("a sibling database whose name extends this one is not read as this one", func(t *testing.T) {
		t.Parallel()
		// dolt-log-gt-extra-20261102.csv.gz ends in a UTC date, but the
		// database before it is gt-extra: gt must not fail on, or read, it.
		sibling := t.TempDir()
		if err := appendArchiveTable(filepath.Join(sibling, "dolt-log-gt-extra-20261102.csv.gz"), archiveCSVHeader, [][]string{
			{"h1", "c", "2026-11-01T00:00:00Z", "one"},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := LoadArchivedCommitHashes(sibling, "gt")
		if err != nil {
			t.Fatalf("a sibling database's file failed gt's check: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("gt's archived hashes = %v, want none (the file belongs to gt-extra)", got)
		}
		if got, err := LoadArchivedCommitHashes(sibling, "gt-extra"); err != nil || len(got) != 1 {
			t.Errorf("gt-extra's archived hashes = %v, %v; want its own h1", got, err)
		}
	})

	t.Run("a name without a UTC date at the end is not attributed to a database", func(t *testing.T) {
		t.Parallel()
		unattributable := t.TempDir()
		if err := appendArchiveTable(filepath.Join(unattributable, "dolt-log-gt-today.csv.gz"), archiveCSVHeader, [][]string{
			{"h1", "c", "2026-11-01T00:00:00Z", "one"},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := LoadArchivedCommitHashes(unattributable, "gt")
		if err != nil {
			t.Fatalf("an unattributable neighbour failed gt's check: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("gt's archived hashes = %v, want none", got)
		}
	})

	t.Run("a file that is not gzip is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, "dolt-log-gt-20261101.csv.gz"), []byte("not gzip"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("an unreadable archive file loaded as a smaller set")
		}
	})

	t.Run("a file without commit_hash is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		if err := appendArchiveTable(filepath.Join(bad, "dolt-log-gt-20261101.csv.gz"), []string{"other"}, [][]string{{"x"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("a file without commit_hash loaded as an empty set")
		}
	})

	t.Run("a directory named like an archive file is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		if err := os.Mkdir(filepath.Join(bad, "dolt-log-gt-20261101.csv.gz"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("a directory named like an archive file was read")
		}
	})

	t.Run("a file that cannot be opened is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		if err := os.Symlink("does-not-exist", filepath.Join(bad, "dolt-log-gt-20261101.csv.gz")); err != nil {
			t.Fatalf("creating a dangling archive symlink: %v", err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("an archive file that cannot be opened loaded as an empty set")
		}
	})

	t.Run("a gzip file with no content is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		path := filepath.Join(bad, "dolt-log-gt-20261101.csv.gz")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("an empty gzip file loaded as an empty set")
		}
	})

	t.Run("a row that is not valid CSV is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write([]byte("commit_hash,committer,date,message\nx\"y,c,d,e\n")); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bad, "dolt-log-gt-20261101.csv.gz"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("a malformed CSV row loaded as a shorter set")
		}
	})

	t.Run("a row without a hash is refused", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		if err := appendArchiveTable(filepath.Join(bad, "dolt-log-gt-20261101.csv.gz"), archiveCSVHeader, [][]string{
			{"", "c", "2026-11-01T00:00:00Z", "one"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArchivedCommitHashes(bad, "gt"); err == nil {
			t.Error("a row without a hash loaded as a shorter set")
		}
	})
}

// TestMaintenanceArchivesNewEventsRows: the events rows past the watermark land
// in events-<db>-<date>.csv.gz with every column, in created_at then id order;
// the watermark moves only once the file is closed; a failed events export
// keeps its watermark while the commit-log export still runs; and a first
// export writes every row one window read returns.
func TestMaintenanceArchivesNewEventsRows(t *testing.T) {
	t.Parallel()
	columns := []string{"id", "issue_id", "event_type", "actor", "old_value", "new_value", "created_at"}
	rows := [][]string{
		{"1", "gt-a", "created", "crew/overseer", "", "", "2026-09-30 10:00:00"},
		{"2", "gt-a", "status", "cheedo", "open", "closed", "2026-10-01 02:00:00"},
	}
	lastCursor := EventsArchiveWatermark{
		CreatedAt: rows[1][6],
		Archived: []ArchivedEventsRow{
			{CreatedAt: rows[0][6], ID: rows[0][0]},
			{CreatedAt: rows[1][6], ID: rows[1][0]},
		},
	}

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
		if got := st.ArchiveEventsWatermarks["gt"]; !sameEventsWatermark(got, lastCursor) {
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
		old := EventsArchiveWatermark{
			CreatedAt: "2026-09-01 00:00:00",
			Archived:  []ArchivedEventsRow{{CreatedAt: "2026-09-01 00:00:00", ID: "7"}},
		}
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
		if got := st.ArchiveEventsWatermarks["gt"]; !sameEventsWatermark(got, old) {
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

	t.Run("a first export writes every row one read returns", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		withArchiveFakes(d, f, nil, nil)

		base := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
		many := make([][]string, 200)
		for i := range many {
			many[i] = []string{strconv.Itoa(i + 1), "gt-a", "x", "c", "", "",
				base.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")}
		}
		fake := withEventsFake(d, columns, map[string][][]string{"gt": many}, nil)

		d.runScheduledMaintenance()

		if fake.reads != 1 {
			t.Errorf("events read ran %d time(s), want one window read", fake.reads)
		}
		if got := readArchiveCSV(t, eventsArchiveFile(dir, "gt")); len(got) != len(many)+1 {
			t.Errorf("events archive holds %d row(s), want %d rows and one header", len(got)-1, len(many))
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		last := many[len(many)-1]
		if got := st.ArchiveEventsWatermarks["gt"]; got.CreatedAt != last[6] || !got.ArchivedIDs()[last[0]] {
			t.Errorf("events watermark = %+v, want %s and id %s in the window", got, last[6], last[0])
		}
	})
}

// TestMaintenanceEventsCursorReadsBackThroughTheLookbackWindow: events.id is a
// UUID, so the cursor cannot be "the newest id". Each run re-reads the window
// ending at the newest written created_at minus the lookback and skips the ids
// the watermark holds. A row inserted with an earlier created_at but not yet
// archived is written exactly once; a row already in the set is not written
// again; a row older than the window is the accepted limit and is not read
// (gt-7nxj4).
func TestMaintenanceEventsCursorReadsBackThroughTheLookbackWindow(t *testing.T) {
	t.Parallel()
	columns := []string{"id", "issue_id", "created_at"}
	rows := [][]string{
		{"outside-window", "gt-a", "2026-09-29 01:00:00"},
		{"backdated", "gt-a", "2026-09-30 10:00:00"},
		{"archived-at-cursor", "gt-a", "2026-10-01 02:00:00"},
		{"new", "gt-a", "2026-10-01 03:00:00"},
	}

	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, nil, nil)
	withEventsFake(d, columns, map[string][][]string{"gt": rows}, nil)
	seeded := EventsArchiveWatermark{
		CreatedAt: "2026-10-01 02:00:00",
		Archived:  []ArchivedEventsRow{{CreatedAt: "2026-10-01 02:00:00", ID: "archived-at-cursor"}},
	}
	if err := recordEventsArchiveWatermark(d.config.TownRoot, "gt", seeded); err != nil {
		t.Fatalf("seeding the events watermark: %v", err)
	}

	d.runScheduledMaintenance()

	want := [][]string{columns, rows[1], rows[3]}
	if got := readArchiveCSV(t, eventsArchiveFile(dir, "gt")); !csvRowsEqual(got, want) {
		t.Errorf("events archive = %v\nwant %v: the backdated row once, the archived row never, the out-of-window row not at all", got, want)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	wm := st.ArchiveEventsWatermarks["gt"]
	if wm.CreatedAt != "2026-10-01 03:00:00" {
		t.Errorf("events watermark = %+v, want the newest written created_at", wm)
	}
	for _, id := range []string{"backdated", "archived-at-cursor", "new"} {
		if !wm.ArchivedIDs()[id] {
			t.Errorf("events watermark does not hold id %q in its window: %+v", id, wm)
		}
	}
	if wm.ArchivedIDs()["outside-window"] {
		t.Errorf("events watermark holds a row older than the window: %+v", wm)
	}

	// A second run re-reads the same window and must not append anything: every
	// row it can see is already in the id set.
	before, err := os.Stat(eventsArchiveFile(dir, "gt"))
	if err != nil {
		t.Fatal(err)
	}
	d.lastMaintenanceRun = time.Time{}
	d.runScheduledMaintenance()
	after, err := os.Stat(eventsArchiveFile(dir, "gt"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Errorf("the second run appended to the events file (%d -> %d bytes); the lookback repeated rows", before.Size(), after.Size())
	}
}

// TestEventsArchiveWatermarkValidate: a cursor is usable only when Dolt can
// compare it and it says which ids in the window are already written; text it
// cannot compare, or a date with no ids, is refused rather than read as the
// start of the table and written out twice.
func TestEventsArchiveWatermarkValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		wm      EventsArchiveWatermark
		wantErr bool
	}{
		{"nothing archived yet", EventsArchiveWatermark{}, false},
		{"timestamp and id", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00", Archived: []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "42"}}}, false},
		{"timestamp with fraction", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00.123456", Archived: []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00.123456", ID: "42"}}}, false},
		{"id with a UUID", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00", Archived: []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "3f2b9c8e-1a4d-4e7f-9b6a-2c5d8e1f0a3b"}}}, false},
		{"text is not a timestamp", EventsArchiveWatermark{CreatedAt: "yesterday", Archived: []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "42"}}}, true},
		{"ids without created_at", EventsArchiveWatermark{Archived: []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "42"}}}, true},
		{"created_at with no ids", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00"}, true},
		{"archived row without a timestamp", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00", Archived: []ArchivedEventsRow{{CreatedAt: "yesterday", ID: "42"}}}, true},
		{"archived row with an empty id", EventsArchiveWatermark{CreatedAt: "2026-11-02 09:15:00", Archived: []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00"}}}, true},
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
// columns, and an events table without them is refused rather than archived
// with a watermark it cannot advance.
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
}

// TestEventsWatermarkRefusesUnusableRows: a row the cursor or the window
// cannot place is refused, so the export never records a watermark that cannot
// bound the next read. A date of the right shape that no calendar has is one
// case: TimestampLiteral accepts it, the parser that computes the window does
// not.
func TestEventsWatermarkRefusesUnusableRows(t *testing.T) {
	t.Parallel()
	columns := []string{"id", "created_at"}
	good := [][]string{{"1", "2026-11-02 09:15:00"}}
	tests := []struct {
		name    string
		wm      EventsArchiveWatermark
		columns []string
		rows    [][]string
	}{
		{"columns without created_at", EventsArchiveWatermark{}, []string{"id"}, good},
		{"a row too short for its columns", EventsArchiveWatermark{}, columns, [][]string{{"1"}}},
		{"a row whose created_at is not a timestamp", EventsArchiveWatermark{}, columns, [][]string{{"1", "yesterday"}}},
		{"a row with no id", EventsArchiveWatermark{}, columns, [][]string{{"", "2026-11-02 09:15:00"}}},
		{"a row whose created_at no calendar has", EventsArchiveWatermark{}, columns, [][]string{{"1", "2026-13-45 99:99:99"}}},
		{
			name: "a previous watermark whose date cannot be parsed",
			wm: EventsArchiveWatermark{
				CreatedAt: "2026-13-45 99:99:99",
				Archived:  []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "1"}},
			},
			columns: columns,
			rows:    good,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.wm.written(tt.columns, tt.rows); err == nil {
				t.Errorf("written(%v, %v) = nil, want an error", tt.columns, tt.rows)
			}
		})
	}

	if _, err := (EventsArchiveWatermark{CreatedAt: "2026-13-45 99:99:99"}).Since(); err == nil {
		t.Error("Since accepted a date no calendar has")
	}
	if _, err := (EventsArchiveWatermark{CreatedAt: "yesterday"}).Since(); err == nil {
		t.Error("Since accepted text that is not a timestamp")
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

// TestDoltArchiveEventsRefusesBadBounds: an unusable window start is refused
// before a connection is opened, so it cannot read as "nothing new".
func TestDoltArchiveEventsRefusesBadBounds(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	ctx := context.Background()
	if _, err := d.doltArchiveEvents(ctx, "gt", "yesterday"); err == nil {
		t.Error("an uncomparable window start was accepted")
	}
	if _, err := d.doltArchiveEvents(ctx, "../gt", "2026-11-02 09:15:00"); err == nil {
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

// TestMaintenanceArchiveEventsRollsBackAFailedExport: rows written before the
// watermark record fails are rolled back, so the file does not hold rows ahead
// of the unadvanced watermark the next run would read again (gt-7nxj4).
func TestMaintenanceArchiveEventsRollsBackAFailedExport(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, nil, nil) // the commit read returns nothing

	statePath := maintenanceGCStatePath(d.config.TownRoot)
	columns := []string{"id", "created_at"}
	rows := [][]string{{"1", "2026-10-01 00:00:00"}}
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
		// The state file becomes a directory after the run's load, so the
		// watermark write fails while the archive file itself can be written.
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			t.Errorf("creating the daemon dir: %v", err)
		}
		if err := os.Mkdir(statePath, 0o755); err != nil {
			t.Errorf("making the state path unwritable: %v", err)
		}
		return archiveEvents{Columns: columns, Rows: rows}, nil
	}

	d.runScheduledMaintenance()

	if _, err := os.Stat(eventsArchiveFile(dir, "gt")); !os.IsNotExist(err) {
		t.Errorf("the failed export left %s behind (stat err %v)", eventsArchiveFile(dir, "gt"), err)
	}
	// The state path is a directory now, so a readable state can only be one
	// that never recorded the failed export.
	if st, err := loadMaintenanceGCState(d.config.TownRoot); err == nil {
		if _, ok := st.ArchiveEventsWatermarks["gt"]; ok {
			t.Errorf("a failed export advanced the events watermark: %+v", st.ArchiveEventsWatermarks["gt"])
		}
	}
	if !strings.Contains(logs.String(), "archive gt events: FAILED") || !strings.Contains(logs.String(), "cannot be recorded") {
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
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		t.Error("the commit read ran while the town was busy")
		return nil, nil
	}
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
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
	var deadline, atRead time.Time
	d.maint.archiveRead = func(ctx context.Context, _ *Daemon, _ string, _ map[string]struct{}) ([]archiveCommit, error) {
		deadline, _ = ctx.Deadline()
		atRead = time.Now()
		return nil, nil
	}
	withEventsFake(d, nil, nil, nil)

	// The baseline is read before the run, and the remaining budget is
	// measured where the read uses it: the deadline was set at the step's
	// start, so comparing it to a clock read after the run would widen the
	// window by however long the run took.
	base := time.Now()
	d.runScheduledMaintenance()

	if deadline.IsZero() {
		t.Fatal("the archive read ran with no deadline")
	}
	if !deadline.After(base) {
		t.Errorf("archive read deadline = %v, want it set after the run began at %v", deadline, base)
	}
	if remaining := deadline.Sub(atRead); remaining <= 0 || remaining > maintenanceArchiveTimeout {
		t.Errorf("archive read had %v left, want (0, %v]", remaining, maintenanceArchiveTimeout)
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
	rows := [][]string{{"1", "2026-10-01 00:00:00"}}
	path := eventsArchiveFile(dir, "gt")
	statePath := maintenanceGCStatePath(d.config.TownRoot)
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
		return archiveEvents{Columns: columns, Rows: rows}, nil
	}
	d.maint.archiveAppend = func(p string, c []string, r [][]string) error {
		if err := appendArchiveTable(p, c, r); err != nil {
			return err
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
		// Make the watermark record fail, which is what starts the rollback.
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			t.Errorf("creating the daemon dir: %v", err)
		}
		if err := os.Mkdir(statePath, 0o755); err != nil {
			t.Errorf("making the state path unwritable: %v", err)
		}
		return nil
	}

	d.runScheduledMaintenance()

	if !strings.Contains(logs.String(), "could not be rolled back") {
		t.Errorf("log does not report the failed rollback:\n%s", logs.String())
	}
}

// TestEventsArchiveQueryReadsTheLookbackWindow runs the production query text
// (eventsArchiveQuery) against an in-process MySQL engine, not a Go fake that
// would only repeat its WHERE clause: the window's lower bound is inclusive,
// a row before it is not read, rows come back in created_at then id order, and
// an empty window start reads the whole table (gt-7nxj4, gt-98eug rework).
func TestEventsArchiveQueryReadsTheLookbackWindow(t *testing.T) {
	t.Parallel()
	ctx, engine := newEventsArchiveEngine(t)
	insert := func(id, createdAt string) {
		archiveEngineExec(t, ctx, engine, fmt.Sprintf(
			"INSERT INTO events (id, created_at) VALUES ('%s', '%s')", id, createdAt))
	}
	insert("before", "2026-09-30 23:59:59")
	insert("at-start", "2026-10-01 00:00:00")
	insert("inside-b", "2026-10-01 12:00:00")
	insert("inside-a", "2026-10-01 12:00:00")

	query, err := eventsArchiveQuery("gt", "2026-10-01 00:00:00")
	if err != nil {
		t.Fatalf("building the query: %v", err)
	}
	if got, want := archiveEngineIDs(t, ctx, engine, query), []string{"at-start", "inside-a", "inside-b"}; !slices.Equal(got, want) {
		t.Errorf("lookback window rows = %v, want %v (the inclusive bound, ordered by created_at then id)", got, want)
	}

	query, err = eventsArchiveQuery("gt", "")
	if err != nil {
		t.Fatalf("building the unbounded query: %v", err)
	}
	if got, want := archiveEngineIDs(t, ctx, engine, query), []string{"before", "at-start", "inside-a", "inside-b"}; !slices.Equal(got, want) {
		t.Errorf("unbounded rows = %v, want the whole table %v", got, want)
	}
}

// TestCommitArchiveQuerySelectsByArchivedHashSet runs the production query
// text (doltArchiveLogQuery) against an in-process MySQL engine, not a Go fake
// that would only repeat its WHERE clause: an archived hash is excluded
// whatever its date, a commit dated before the others is still selected, and
// rows come back oldest first (gt-98eug).
func TestCommitArchiveQuerySelectsByArchivedHashSet(t *testing.T) {
	t.Parallel()
	ctx, engine := newCommitArchiveEngine(t)
	insert := func(hash, date string) {
		archiveEngineExec(t, ctx, engine, fmt.Sprintf(
			"INSERT INTO dolt_log (commit_hash, committer, date, message) VALUES ('%s', 'c', '%s', 'm')", hash, date))
	}
	insert("archived-new", "2026-10-02 09:00:00")
	insert("backdated", "2026-10-01 09:00:00")
	insert("later", "2026-10-03 09:00:00")
	insert("archived-old", "2026-09-01 09:00:00")

	archived := map[string]struct{}{"archived-new": {}, "archived-old": {}}
	if got, want := archiveEngineCommitHashes(t, ctx, engine, archived), []string{"backdated", "later"}; !slices.Equal(got, want) {
		t.Errorf("unarchived hashes = %v, want %v (archived excluded, backdated kept)", got, want)
	}
	if got, want := archiveEngineCommitHashes(t, ctx, engine, nil), []string{"archived-old", "backdated", "archived-new", "later"}; !slices.Equal(got, want) {
		t.Errorf("whole log = %v, want %v (oldest first)", got, want)
	}
}

// TestCommitArchiveQueryLeavesTheRestForTheNextRun: the export reads at most
// the default commit bound per run; the rest are read by the next run from the
// widened archived set, so the pre-flatten check's remainder shrinks to zero
// (gt-98eug).
func TestCommitArchiveQueryLeavesTheRestForTheNextRun(t *testing.T) {
	t.Parallel()
	ctx, engine := newCommitArchiveEngine(t)
	const total = config.DefaultArchiveCommitLimit + 2
	values := make([]string, 0, total)
	for i := 1; i <= total; i++ {
		values = append(values, fmt.Sprintf("('h%05d', 'c', '2026-10-01 09:00:00', 'm')", i))
	}
	archiveEngineExec(t, ctx, engine,
		"INSERT INTO dolt_log (commit_hash, committer, date, message) VALUES "+strings.Join(values, ","))

	first := archiveEngineCommitHashes(t, ctx, engine, nil)
	if len(first) != config.DefaultArchiveCommitLimit {
		t.Fatalf("first read returned %d commits, want the %d bound", len(first), config.DefaultArchiveCommitLimit)
	}
	if first[0] != "h00001" || first[len(first)-1] != fmt.Sprintf("h%05d", config.DefaultArchiveCommitLimit) {
		t.Errorf("first read = %s..%s, want the oldest %d", first[0], first[len(first)-1], config.DefaultArchiveCommitLimit)
	}

	archived := make(map[string]struct{}, len(first))
	for _, hash := range first {
		archived[hash] = struct{}{}
	}
	want := []string{
		fmt.Sprintf("h%05d", config.DefaultArchiveCommitLimit+1),
		fmt.Sprintf("h%05d", total),
	}
	if got := archiveEngineCommitHashes(t, ctx, engine, archived); !slices.Equal(got, want) {
		t.Errorf("next read = %v, want the remaining %v", got, want)
	}
}

// newCommitArchiveEngine builds an in-memory go-mysql-server engine with a gt
// database and the dolt_log table the commit archive query reads.
func newCommitArchiveEngine(t *testing.T) (*gmssql.Context, *sqle.Engine) {
	t.Helper()
	db := memory.NewDatabase("gt")
	pro := memory.NewDBProvider(db)
	session := memory.NewSession(gmssql.NewBaseSession(), pro)
	ctx := gmssql.NewContext(context.Background(), gmssql.WithSession(session))
	ctx.SetCurrentDatabase("gt")
	db.AddTable("dolt_log", memory.NewTable(ctx, db, "dolt_log", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "commit_hash", Type: types.Text, Nullable: false, Source: "dolt_log"},
		{Name: "committer", Type: types.Text, Nullable: false, Source: "dolt_log"},
		{Name: "date", Type: types.Datetime, Nullable: false, Source: "dolt_log"},
		{Name: "message", Type: types.Text, Nullable: false, Source: "dolt_log"},
	}), db.GetForeignKeyCollection()))
	engine := sqle.NewDefault(pro)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("closing the test engine: %v", err)
		}
	})
	return ctx, engine
}

// archiveEngineCommitHashes runs the production commit archive query
// (doltArchiveLogQuery, which takes no arguments) against the in-process engine
// and feeds its rows, oldest first, through the production selection
// (firstUnarchivedCommits), so the test drives the code that skips archived
// hashes and applies the per-run bound rather than a second copy of it.
func archiveEngineCommitHashes(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, archived map[string]struct{}) []string {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, doltArchiveLogQuery())
	if err != nil {
		t.Fatalf("commit archive query: %v", err)
	}
	rows, err := gmssql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("reading the commit archive rows: %v", err)
	}
	i := 0
	commits, err := firstUnarchivedCommits(func() (archiveCommit, bool, error) {
		if i >= len(rows) {
			return archiveCommit{}, false, nil
		}
		row := rows[i]
		i++
		hash, ok := row[0].(string)
		if !ok {
			return archiveCommit{}, false, fmt.Errorf("row hash is %T, want string", row[0])
		}
		return archiveCommit{Hash: hash}, true, nil
	}, archived, config.DefaultArchiveCommitLimit)
	if err != nil {
		t.Fatalf("selecting the unarchived commits: %v", err)
	}
	hashes := make([]string, 0, len(commits))
	for _, c := range commits {
		hashes = append(hashes, c.Hash)
	}
	return hashes
}

// TestFirstUnarchivedCommitsStopsReadingAtTheBound: the selection stops asking
// for rows once it holds the default commit bound's fresh commits, so a run
// never reads past the bound, and archived rows before the bound do not count
// toward it.
func TestFirstUnarchivedCommitsStopsReadingAtTheBound(t *testing.T) {
	t.Parallel()
	archived := map[string]struct{}{"a1": {}, "a2": {}}
	reads := 0
	next := func() (archiveCommit, bool, error) {
		reads++
		switch reads {
		case 1:
			return archiveCommit{Hash: "a1"}, true, nil
		case 2:
			return archiveCommit{Hash: "a2"}, true, nil
		}
		// A finite source: if the bound were missing the selection would run on
		// to the end and return more than the bound, which the assertion below
		// reports, instead of looping on an endless stream until memory is gone.
		if reads > config.DefaultArchiveCommitLimit+len(archived)+100 {
			return archiveCommit{}, false, nil
		}
		return archiveCommit{Hash: fmt.Sprintf("n%05d", reads)}, true, nil
	}
	got, err := firstUnarchivedCommits(next, archived, config.DefaultArchiveCommitLimit)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if len(got) != config.DefaultArchiveCommitLimit {
		t.Fatalf("selected %d commits, want the %d bound", len(got), config.DefaultArchiveCommitLimit)
	}
	if want := config.DefaultArchiveCommitLimit + len(archived); reads != want {
		t.Errorf("read %d rows, want exactly %d (the bound plus the skipped archived rows)", reads, want)
	}
}

// TestEventsArchiveFirstReadIsBounded: a table with more rows than the per-run
// bound is exported in bounded runs, oldest first; the next run reads the rest
// from the advanced watermark, and no row is lost or written twice (gt-3w3hk).
func TestEventsArchiveFirstReadIsBounded(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()

	columns := []string{"id", "created_at"}
	total := config.DefaultArchiveEventsLimit + 2
	base := gcTestNow.Add(-3 * time.Hour)
	rows := make([][]driver.Value, 0, total)
	createdAt := func(i int) string {
		return base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
	}
	for i := 1; i <= total; i++ {
		rows = append(rows, []driver.Value{fmt.Sprintf("h%05d", i), createdAt(i)})
	}
	// The fake serves the whole table on every read, whatever the window, so
	// the read has to stop at the bound and skip the ids the watermark holds:
	// a bound counting rows rather than fresh rows would re-read the first run's
	// rows and never reach the rest.
	d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{columns: columns, rows: rows})

	ctx := context.Background()
	if _, _, _, err := d.maintenanceArchiveEvents(ctx, dir, "gt", EventsArchiveWatermark{}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	wantRows := func(n int) [][]string {
		out := make([][]string, 0, n)
		for i := 1; i <= n; i++ {
			out = append(out, []string{fmt.Sprintf("h%05d", i), createdAt(i)})
		}
		return out
	}
	got := readArchiveCSV(t, eventsArchiveFile(dir, "gt"))
	if len(got) != config.DefaultArchiveEventsLimit+1 {
		t.Fatalf("first export wrote %d row(s), want the %d bound and one header", len(got)-1, config.DefaultArchiveEventsLimit)
	}
	if want := wantRows(config.DefaultArchiveEventsLimit); !csvRowsEqual(got[1:], want) {
		t.Errorf("first export = %s..%s, want the oldest %d rows h00001..h%05d",
			got[1][0], got[len(got)-1][0], config.DefaultArchiveEventsLimit, config.DefaultArchiveEventsLimit)
	}

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if _, _, _, err := d.maintenanceArchiveEvents(ctx, dir, "gt", st.ArchiveEventsWatermarks["gt"]); err != nil {
		t.Fatalf("second export: %v", err)
	}
	got = readArchiveCSV(t, eventsArchiveFile(dir, "gt"))
	want := append([][]string{columns}, wantRows(total)...)
	if !csvRowsEqual(got, want) {
		t.Errorf("after two runs the archive holds %d data row(s), want all %d once, oldest first", len(got)-1, total)
	}
}

// TestEventsArchiveRefusesAnIncompatibleHeader: appending to a file whose
// header differs from the events table's current columns fails that database's
// export, keeps its watermark, and names the database in the log (gt-3w3hk).
func TestEventsArchiveRefusesAnIncompatibleHeader(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, nil, nil)

	path := eventsArchiveFile(dir, "gt")
	old := []string{"id", "created_at", "payload"}
	oldRow := []string{"1", "2026-10-01 00:00:00", "x"}
	if err := appendArchiveTable(path, old, [][]string{oldRow}); err != nil {
		t.Fatalf("seeding the older file: %v", err)
	}
	withEventsFake(d, []string{"id", "created_at"}, map[string][][]string{
		"gt": {{"2", "2026-10-01 01:00:00"}},
	}, nil)

	d.runScheduledMaintenance()

	if got := readArchiveCSV(t, path); !csvRowsEqual(got, [][]string{old, oldRow}) {
		t.Errorf("archive file = %v, want the older member untouched", got)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if _, ok := st.ArchiveEventsWatermarks["gt"]; ok {
		t.Errorf("events watermark advanced although the header was refused: %+v", st.ArchiveEventsWatermarks["gt"])
	}
	if !strings.Contains(logs.String(), "archive gt events: FAILED") || !strings.Contains(logs.String(), "columns") {
		t.Errorf("log does not name gt's refused events export and why:\n%s", logs.String())
	}
}

// TestEventsArchiveRefusesAnUnreadableHeader: a file that cannot be read as a
// gzip CSV has no header the export can check, so the export is refused rather
// than appended to blind (gt-3w3hk).
func TestEventsArchiveRefusesAnUnreadableHeader(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	withArchiveFakes(d, f, nil, nil)

	path := eventsArchiveFile(dir, "gt")
	if err := os.WriteFile(path, []byte("not a gzip member"), 0o600); err != nil {
		t.Fatalf("seeding an unreadable file: %v", err)
	}
	withEventsFake(d, []string{"id", "created_at"}, map[string][][]string{
		"gt": {{"2", "2026-10-01 01:00:00"}},
	}, nil)

	d.runScheduledMaintenance()

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if _, ok := st.ArchiveEventsWatermarks["gt"]; ok {
		t.Errorf("events watermark advanced although the file's header could not be read: %+v", st.ArchiveEventsWatermarks["gt"])
	}
	if !strings.Contains(logs.String(), "archive gt events: FAILED") || !strings.Contains(logs.String(), "header") {
		t.Errorf("log does not name gt's refused events export and why:\n%s", logs.String())
	}
}

// TestCheckEventsArchiveHeaderRefusals: a file the checker cannot open, or one
// whose first record is not a CSV header, is refused, so no export appends
// under a header it could not compare (gt-3w3hk).
func TestCheckEventsArchiveHeaderRefusals(t *testing.T) {
	t.Parallel()
	columns := []string{"id", "created_at"}

	t.Run("a file that cannot be opened", func(t *testing.T) {
		t.Parallel()
		if err := checkEventsArchiveHeader(filepath.Join(t.TempDir(), "absent.csv.gz"), columns); err == nil {
			t.Error("a missing file passed the header check")
		}
	})

	t.Run("a first record that is not valid CSV", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "events-gt-20261001.csv.gz")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		if _, err := gz.Write([]byte("id\"created_at\n")); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := checkEventsArchiveHeader(path, columns); err == nil {
			t.Error("a malformed CSV header passed the check")
		}
	})
}

// TestEventsArchiveBatchRefusals: an events result set the archive cannot read,
// or one without the watermark's columns, is an error rather than an empty
// batch the watermark would walk past (gt-3w3hk).
func TestEventsArchiveBatchRefusals(t *testing.T) {
	t.Parallel()
	openRows := func(t *testing.T, server *archiveFakeServer) (*sql.DB, *sql.Rows) {
		t.Helper()
		db, err := archiveFakeOpener(t, server)("")
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM events")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return db, rows
	}

	t.Run("a result set closed before it was read", func(t *testing.T) {
		t.Parallel()
		db, rows := openRows(t, &archiveFakeServer{columns: []string{"id", "created_at"}})
		defer db.Close()
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := eventsArchiveBatch(rows, nil, config.DefaultArchiveEventsLimit); err == nil {
			t.Error("a closed result set was read as an empty batch")
		}
	})

	t.Run("a result set without the watermark columns", func(t *testing.T) {
		t.Parallel()
		db, rows := openRows(t, &archiveFakeServer{columns: []string{"payload"}})
		defer db.Close()
		defer rows.Close()
		if _, err := eventsArchiveBatch(rows, nil, config.DefaultArchiveEventsLimit); err == nil || !strings.Contains(err.Error(), "no created_at and id columns") {
			t.Errorf("result set without the watermark columns = %v, want it refused", err)
		}
	})
}

// TestDoltArchiveEventsRefusesAnUnreadableWatermark: the read's bound counts
// only rows the watermark does not hold, so an unreadable watermark refuses the
// read before it opens a connection instead of counting archived rows against
// the bound (gt-3w3hk).
func TestDoltArchiveEventsRefusesAnUnreadableWatermark(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	path := maintenanceGCStatePath(d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.maint.archiveOpenDB = func(string) (*sql.DB, error) {
		t.Error("the events read opened a connection with an unreadable watermark")
		return nil, errors.New("not reached")
	}
	if _, err := d.doltArchiveEvents(context.Background(), "gt", ""); err == nil || !strings.Contains(err.Error(), "watermark") {
		t.Errorf("read with an unreadable watermark = %v, want it refused", err)
	}
}

// TestArchiveTightensAnExistingLooseDirectory: the step sets an archive
// directory that already exists to 0700, and a mode it cannot set is logged
// once without failing the run (gt-3w3hk).
func TestArchiveTightensAnExistingLooseDirectory(t *testing.T) {
	t.Parallel()

	t.Run("an existing loose directory is tightened", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
		dir := filepath.Join(t.TempDir(), "archive")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The creating umask can narrow the mode; set the loose one explicitly.
		//testpolicy:allow no-exec-files — a directory needs its search bit to be entered, and 0755 is the loose state the step must tighten
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		d.patrolConfig = archiveConfig(dir)
		withArchiveFakes(d, f, nil, nil)

		d.runScheduledMaintenance()

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat archive directory: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("archive directory mode = %04o, want 0700", got)
		}
		if strings.Contains(logs.String(), "cannot set") {
			t.Errorf("tightening a writable directory was logged as a failure:\n%s", logs.String())
		}
	})

	t.Run("a mode that cannot be set is logged once and does not fail", func(t *testing.T) {
		t.Parallel()
		d, logs := gcTestDaemon(t)
		dir := t.TempDir()
		before, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat archive directory: %v", err)
		}
		// An owned temporary directory cannot be made unchmodable on every
		// host, so the refusal is driven through the same call the step makes,
		// with a chmod that refuses.
		d.tightenArchiveDir(dir, func(string, fs.FileMode) error {
			return errors.New("operation not permitted")
		})

		if got := strings.Count(logs.String(), "cannot set"); got != 1 {
			t.Errorf("log holds %d 'cannot set' line(s), want one:\n%s", got, logs.String())
		}
		if !strings.Contains(logs.String(), "operation not permitted") || !strings.Contains(logs.String(), "continuing") {
			t.Errorf("log does not say why the mode was left alone:\n%s", logs.String())
		}
		after, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat archive directory: %v", err)
		}
		if after.Mode().Perm() != before.Mode().Perm() {
			t.Errorf("archive directory mode = %04o, want the refused %04o left alone",
				after.Mode().Perm(), before.Mode().Perm())
		}
	})
}

// newEventsArchiveEngine builds an in-memory go-mysql-server engine with a gt
// database and the events table the archive query reads.
func newEventsArchiveEngine(t *testing.T) (*gmssql.Context, *sqle.Engine) {
	t.Helper()
	db := memory.NewDatabase("gt")
	pro := memory.NewDBProvider(db)
	session := memory.NewSession(gmssql.NewBaseSession(), pro)
	ctx := gmssql.NewContext(context.Background(), gmssql.WithSession(session))
	ctx.SetCurrentDatabase("gt")
	db.AddTable("events", memory.NewTable(ctx, db, "events", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "id", Type: types.Text, Nullable: false, Source: "events"},
		{Name: "created_at", Type: types.Datetime, Nullable: false, Source: "events"},
	}), db.GetForeignKeyCollection()))
	engine := sqle.NewDefault(pro)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("closing the test engine: %v", err)
		}
	})
	return ctx, engine
}

// archiveEngineExec runs one statement against the in-process engine.
func archiveEngineExec(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if _, err := gmssql.RowIterToRows(ctx, iter); err != nil {
		t.Fatalf("draining %q: %v", query, err)
	}
}

// archiveEngineIDs runs query and returns its first column as strings.
func archiveEngineIDs(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) []string {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	rows, err := gmssql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("reading rows for %q: %v", query, err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id, ok := row[0].(string)
		if !ok {
			t.Fatalf("row id is %T, want string: %v", row[0], row[0])
		}
		ids = append(ids, id)
	}
	return ids
}

// archiveCommitRows serves total synthetic dolt_log rows, one second apart from
// base, to the archive read's fake driver.
func archiveCommitRows(total int, base time.Time) [][]driver.Value {
	rows := make([][]driver.Value, 0, total)
	for i := 1; i <= total; i++ {
		rows = append(rows, []driver.Value{
			fmt.Sprintf("h%05d", i), "committer", base.Add(time.Duration(i) * time.Second), "message",
		})
	}
	return rows
}

// archiveEventRows serves total synthetic events rows, one second apart from
// base, to the archive read's fake driver.
func archiveEventRows(total int, base time.Time) [][]driver.Value {
	rows := make([][]driver.Value, 0, total)
	for i := 1; i <= total; i++ {
		rows = append(rows, []driver.Value{
			fmt.Sprintf("h%05d", i), base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05"),
		})
	}
	return rows
}

// TestArchiveLimitsDefaultTo5000: with neither setting present the archive step
// reads the 5000-row default for each batch, the behavior before the settings
// existed (gt-s6srv). The bounded reads at the default are driven by
// TestEventsArchiveFirstReadIsBounded and TestFirstUnarchivedCommitsStopsReadingAtTheBound,
// so this test only pins the unset section to the defaults and builds no
// fixture as large as them.
func TestArchiveLimitsDefaultTo5000(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.patrolConfig = archiveConfig(t.TempDir())

	commit, events, err := d.maintenanceArchiveLimits()
	if err != nil {
		t.Fatalf("reading the unset archive bounds: %v", err)
	}
	if commit != config.DefaultArchiveCommitLimit {
		t.Errorf("archive commit bound = %d, want the %d default", commit, config.DefaultArchiveCommitLimit)
	}
	if events != config.DefaultArchiveEventsLimit {
		t.Errorf("archive events bound = %d, want the %d default", events, config.DefaultArchiveEventsLimit)
	}
}

// TestArchiveLimitsComeFromConfig: small archive_commit_limit and
// archive_events_limit settings bound the production reads, and each run's
// watermark advances past exactly the rows it wrote (gt-s6srv).
func TestArchiveLimitsComeFromConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("commits", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		withGCFakes(t, d)
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		limit := 3
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveCommitLimit = &limit
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"commit_hash", "committer", "date", "message"},
			rows:    archiveCommitRows(10, gcTestNow),
		})

		if _, _, _, err := d.maintenanceArchiveCommits(ctx, dir, "gt", ArchiveWatermark{}); err != nil {
			t.Fatalf("export: %v", err)
		}
		got := readArchiveCSV(t, archiveFile(dir, "gt"))
		if len(got) != limit+1 {
			t.Fatalf("export wrote %d row(s), want %d and one header", len(got)-1, limit)
		}
		for i, row := range got[1:] {
			if want := fmt.Sprintf("h%05d", i+1); row[0] != want {
				t.Errorf("row %d = %s, want %s", i, row[0], want)
			}
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		wm := st.ArchiveWatermarks["gt"]
		if want := gcTestNow.Add(time.Duration(limit) * time.Second); !wm.Date.Equal(want) {
			t.Errorf("watermark date = %s, want %s (the newest row written)", wm.Date, want)
		}
		if want := []string{fmt.Sprintf("h%05d", limit)}; !slices.Equal(wm.Hashes, want) {
			t.Errorf("watermark hashes = %v, want %v", wm.Hashes, want)
		}
	})

	t.Run("events", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		withGCFakes(t, d)
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		limit := 4
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &limit
		base := gcTestNow.Add(-time.Hour)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"id", "created_at"},
			rows:    archiveEventRows(10, base),
		})

		if _, _, _, err := d.maintenanceArchiveEvents(ctx, dir, "gt", EventsArchiveWatermark{}); err != nil {
			t.Fatalf("export: %v", err)
		}
		got := readArchiveCSV(t, eventsArchiveFile(dir, "gt"))
		if len(got) != limit+1 {
			t.Fatalf("export wrote %d row(s), want %d and one header", len(got)-1, limit)
		}
		for i, row := range got[1:] {
			if want := fmt.Sprintf("h%05d", i+1); row[0] != want {
				t.Errorf("row %d = %s, want %s", i, row[0], want)
			}
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		wm := st.ArchiveEventsWatermarks["gt"]
		if want := base.Add(time.Duration(limit) * time.Second).Format("2006-01-02 15:04:05"); wm.CreatedAt != want {
			t.Errorf("watermark created_at = %s, want %s (the newest row written)", wm.CreatedAt, want)
		}
		ids := make([]string, 0, len(wm.Archived))
		for _, row := range wm.Archived {
			ids = append(ids, row.ID)
		}
		sort.Strings(ids)
		want := make([]string, 0, limit)
		for i := 1; i <= limit; i++ {
			want = append(want, fmt.Sprintf("h%05d", i))
		}
		if !slices.Equal(ids, want) {
			t.Errorf("watermark ids = %v, want %v", ids, want)
		}
	})
}

// TestMaintenanceArchiveRefusesABadLimitSetting: a negative batch bound is a
// configuration error: the step archives nothing, escalates once naming the
// key, and skips every database's gc (gt-s6srv).
func TestMaintenanceArchiveRefusesABadLimitSetting(t *testing.T) {
	t.Parallel()
	bad := -1
	for _, tt := range []struct {
		name    string
		set     func(*ScheduledMaintenanceConfig)
		wantKey string
	}{
		{
			name:    "commit bound",
			set:     func(c *ScheduledMaintenanceConfig) { c.ArchiveCommitLimit = &bad },
			wantKey: config.MaintenanceArchiveCommitLimitKey,
		},
		{
			name:    "events bound",
			set:     func(c *ScheduledMaintenanceConfig) { c.ArchiveEventsLimit = &bad },
			wantKey: config.MaintenanceArchiveEventsLimitKey,
		},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs := gcTestDaemon(t)
			f := withGCFakes(t, d)
			f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
			d.patrolConfig = archiveConfig(t.TempDir())
			tt.set(d.patrolConfig.Patrols.ScheduledMaintenance)
			d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
				t.Error("the dolt_log read ran with an invalid batch bound")
				return nil, nil
			}

			d.runScheduledMaintenance()

			if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], tt.wantKey) {
				t.Errorf("escalations = %v, want one naming %s", f.escalations, tt.wantKey)
			}
			if len(f.gcCalls) != 0 {
				t.Errorf("gc calls = %v, want none while no database was archived", f.gcCalls)
			}
			if !strings.Contains(logs.String(), tt.wantKey) {
				t.Errorf("log does not name the refused setting %s:\n%s", tt.wantKey, logs.String())
			}
		})
	}
}

// TestArchiveLoopTakesSeveralBatchesInOneCycle: a database 12 commits behind
// with archive_commit_limit 5 is fully archived by one call to the step, in
// three batches of 5, 5 and the short 2; its watermark ends at the last commit
// written and the file holds all 12 rows (gt-o4sk8.1).
func TestArchiveLoopTakesSeveralBatchesInOneCycle(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveCommitLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	withEventsFake(d, nil, nil, nil)
	open := archiveFakeOpener(t, &archiveFakeServer{
		columns: []string{"commit_hash", "committer", "date", "message"},
		rows:    archiveCommitRows(12, gcTestNow),
	})
	reads := 0
	d.maint.archiveOpenDB = func(dsn string) (*sql.DB, error) {
		reads++
		return open(dsn)
	}

	out, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
		t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
	}
	if reads != 3 {
		t.Errorf("dolt_log read %d time(s), want 3 batches (5, 5 and the short 2)", reads)
	}
	got := readArchiveCSV(t, archiveFile(dir, "gt"))
	if len(got) != 13 {
		t.Fatalf("archive holds %d row(s) and a header, want all 12 rows once", len(got)-1)
	}
	for i, row := range got[1:] {
		if want := fmt.Sprintf("h%05d", i+1); row[0] != want {
			t.Errorf("row %d = %s, want %s", i, row[0], want)
		}
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	wm := st.ArchiveWatermarks["gt"]
	if want := gcTestNow.Add(12 * time.Second); !wm.Date.Equal(want) {
		t.Errorf("watermark date = %s, want %s (the last commit)", wm.Date, want)
	}
	if want := []string{"h00012"}; !slices.Equal(wm.Hashes, want) {
		t.Errorf("watermark hashes = %v, want %v", wm.Hashes, want)
	}
}

// TestArchiveEventsLoopTakesSeveralBatchesInOneCycle: the events export takes
// the same batches in one call — 12 rows with archive_events_limit 5 — and its
// watermark ends at the last row written (gt-o4sk8.1).
func TestArchiveEventsLoopTakesSeveralBatchesInOneCycle(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		return nil, nil // no commits: this test drives the events export
	}
	base := gcTestNow.Add(-time.Hour)
	open := archiveFakeOpener(t, &archiveFakeServer{
		columns: []string{"id", "created_at"},
		rows:    archiveEventRows(12, base),
	})
	reads := 0
	d.maint.archiveOpenDB = func(dsn string) (*sql.DB, error) {
		reads++
		return open(dsn)
	}

	out, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
		t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
	}
	if reads != 3 {
		t.Errorf("events read %d time(s), want 3 batches (5, 5 and the short 2)", reads)
	}
	got := readArchiveCSV(t, eventsArchiveFile(dir, "gt"))
	if len(got) != 13 {
		t.Fatalf("archive holds %d row(s) and a header, want all 12 rows once", len(got)-1)
	}
	for i, row := range got[1:] {
		if want := fmt.Sprintf("h%05d", i+1); row[0] != want {
			t.Errorf("row %d = %s, want %s", i, row[0], want)
		}
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	wm := st.ArchiveEventsWatermarks["gt"]
	if want := base.Add(12 * time.Second).Format("2006-01-02 15:04:05"); wm.CreatedAt != want {
		t.Errorf("watermark created_at = %s, want %s (the last row)", wm.CreatedAt, want)
	}
	if len(wm.Archived) != 12 {
		t.Errorf("watermark holds %d id(s), want all 12 written rows", len(wm.Archived))
	}
}

// TestArchiveEventsLoopKeepsGoingPastTheLookbackReRead: the events read
// re-reads its lookback window, so a page it returns can carry rows the
// watermark already holds. A full page the export filters down to a few fresh
// rows must not end the loop while more rows are still behind (gt-o4sk8.1).
func TestArchiveEventsLoopKeepsGoingPastTheLookbackReRead(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		return nil, nil // no commits: this test drives the events export
	}
	columns := []string{"id", "created_at"}
	rows := make([][]string, 0, 12)
	// Ten hours apart, so the window's start moves past the archived prefix as
	// the watermark advances.
	for i := 1; i <= 12; i++ {
		rows = append(rows, []string{
			fmt.Sprintf("h%05d", i),
			gcTestNow.Add(-200*time.Hour + time.Duration(i)*10*time.Hour).Format("2006-01-02 15:04:05"),
		})
	}
	// The lookback re-read: a page bounded by rows, archived ones included.
	reads := 0
	d.maint.archiveEventsRead = func(_ context.Context, _ *Daemon, _ string, since string) (archiveEvents, error) {
		reads++
		page := archiveEvents{Columns: columns}
		for _, row := range rows {
			if since != "" && row[1] < since {
				continue
			}
			page.Rows = append(page.Rows, row)
			if len(page.Rows) == limit {
				break
			}
		}
		return page, nil
	}

	out, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
		t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
	}
	if reads != 5 {
		t.Errorf("events read %d time(s), want 5: a full page of mostly archived rows keeps the loop going", reads)
	}
	got := readArchiveCSV(t, eventsArchiveFile(dir, "gt"))
	if len(got) != 13 {
		t.Fatalf("archive holds %d row(s) and a header, want all 12 rows once", len(got)-1)
	}
	for i, row := range got[1:] {
		if want := fmt.Sprintf("h%05d", i+1); row[0] != want {
			t.Errorf("row %d = %s, want %s", i, row[0], want)
		}
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if want := rows[len(rows)-1][1]; st.ArchiveEventsWatermarks["gt"].CreatedAt != want {
		t.Errorf("watermark created_at = %s, want %s (the last row)", st.ArchiveEventsWatermarks["gt"].CreatedAt, want)
	}
}

// TestArchiveEventsFullPageOfArchivedRowsAdvances: the events read re-reads its
// lookback window, so a page it returns can be full of rows the watermark
// already holds while rows after them are still behind. The export must read
// past that page, not end the batch there: with five archived rows warm and a
// five-row page over twelve hourly rows, one maintenance call still archives
// all twelve (gt-o4sk8.1).
func TestArchiveEventsFullPageOfArchivedRowsAdvances(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		return nil, nil // no commits: this test drives the events export
	}
	columns := []string{"id", "created_at"}
	base := gcTestNow.Add(-100 * time.Hour)
	rows := make([][]string, 0, 12)
	for i := 1; i <= 12; i++ {
		rows = append(rows, []string{
			fmt.Sprintf("h%05d", i),
			base.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05"),
		})
	}
	// A previous cycle wrote the first five rows and recorded them; all are
	// inside the lookback window, so the next read returns them again as a full
	// page.
	if err := appendArchiveTable(eventsArchiveFile(dir, "gt"), columns, rows[:5]); err != nil {
		t.Fatalf("seeding the events file: %v", err)
	}
	seeded := EventsArchiveWatermark{CreatedAt: rows[4][1]}
	for _, row := range rows[:5] {
		seeded.Archived = append(seeded.Archived, ArchivedEventsRow{CreatedAt: row[1], ID: row[0]})
	}
	if err := recordEventsArchiveWatermark(d.config.TownRoot, "gt", seeded); err != nil {
		t.Fatalf("seeding the events watermark: %v", err)
	}
	// A page bounded by rows, archived ones included: what a read over the
	// lookback window returns when it is bounded before the id filter.
	reads := 0
	d.maint.archiveEventsRead = func(_ context.Context, _ *Daemon, _ string, since string) (archiveEvents, error) {
		reads++
		page := archiveEvents{Columns: columns}
		for _, row := range rows {
			if since != "" && row[1] < since {
				continue
			}
			page.Rows = append(page.Rows, row)
			if len(page.Rows) == limit {
				break
			}
		}
		return page, nil
	}

	out, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
		t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
	}
	if reads < 4 {
		t.Errorf("events read %d time(s), want at least 4: the archived pages must be read past", reads)
	}
	got := readArchiveCSV(t, eventsArchiveFile(dir, "gt"))
	if len(got) != 13 {
		t.Fatalf("archive holds %d row(s) and a header, want all 12 rows once", len(got)-1)
	}
	for i, row := range got[1:] {
		if want := fmt.Sprintf("h%05d", i+1); row[0] != want {
			t.Errorf("row %d = %s, want %s", i, row[0], want)
		}
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	wm := st.ArchiveEventsWatermarks["gt"]
	if want := rows[len(rows)-1][1]; wm.CreatedAt != want {
		t.Errorf("watermark created_at = %s, want %s (the last row)", wm.CreatedAt, want)
	}
	if len(wm.Archived) != 12 {
		t.Errorf("watermark holds %d id(s), want all 12 written rows", len(wm.Archived))
	}
}

// TestArchiveEventsOneTimestampFillingAPageFailsVisibly: when more rows than
// one page share a created_at and the first page is already archived, the read
// cannot move past them. Ending the loop quietly would leave the database
// behind on every cycle with nothing in the log, so the export fails, keeps the
// watermark and names the cause (gt-o4sk8.1).
func TestArchiveEventsOneTimestampFillingAPageFailsVisibly(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		return nil, nil
	}
	columns := []string{"id", "created_at"}
	at := gcTestNow.Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	rows := make([][]string, 0, 8)
	for i := 1; i <= 8; i++ {
		rows = append(rows, []string{fmt.Sprintf("s%05d", i), at})
	}
	if err := appendArchiveTable(eventsArchiveFile(dir, "gt"), columns, rows[:5]); err != nil {
		t.Fatalf("seeding the events file: %v", err)
	}
	seeded := EventsArchiveWatermark{CreatedAt: at}
	for _, row := range rows[:5] {
		seeded.Archived = append(seeded.Archived, ArchivedEventsRow{CreatedAt: row[1], ID: row[0]})
	}
	if err := recordEventsArchiveWatermark(d.config.TownRoot, "gt", seeded); err != nil {
		t.Fatalf("seeding the events watermark: %v", err)
	}
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
		return archiveEvents{Columns: columns, Rows: rows[:limit]}, nil // the same full page every time
	}

	_, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if !strings.Contains(logs.String(), "fill more than one page") || !strings.Contains(logs.String(), "archive_events_limit") {
		t.Errorf("log does not name the same-timestamp page and the setting to raise:\n%s", logs.String())
	}
	if len(unarchived) == 0 {
		t.Errorf("unarchived = %v, want gt listed", unarchived)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if got := len(st.ArchiveEventsWatermarks["gt"].Archived); got != 5 {
		t.Errorf("watermark holds %d id(s), want the 5 seeded ones kept", got)
	}
}

// TestArchiveEventsRefusesAnUndateableArchivedPage: moving the read past a
// full page of archived rows needs the page's newest created_at. A row the
// export cannot date fails the batch and keeps the watermark, rather than
// leaving the read where it was (gt-o4sk8.1).
func TestArchiveEventsRefusesAnUndateableArchivedPage(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 1
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	seeded := EventsArchiveWatermark{
		CreatedAt: "2026-10-01 02:00:00",
		Archived:  []ArchivedEventsRow{{CreatedAt: "2026-10-01 02:00:00", ID: "h1"}},
	}
	if err := recordEventsArchiveWatermark(d.config.TownRoot, "gt", seeded); err != nil {
		t.Fatalf("seeding the events watermark: %v", err)
	}
	// The only row the read returns is already archived, and it is the whole
	// page, so the export tries to date it to find where to read next.
	d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
		return archiveEvents{
			Columns: []string{"id", "created_at"},
			Rows:    [][]string{{"h1", "not a timestamp"}},
		}, nil
	}

	_, _, _, err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", seeded)

	if err == nil || !strings.Contains(err.Error(), "cannot be parsed") {
		t.Errorf("maintenanceArchiveEvents = %v, want an error naming the undateable created_at", err)
	}
	if _, statErr := os.Stat(eventsArchiveFile(dir, "gt")); !os.IsNotExist(statErr) {
		t.Errorf("a refused page wrote %s (stat err %v)", eventsArchiveFile(dir, "gt"), statErr)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if got := st.ArchiveEventsWatermarks["gt"]; !sameEventsWatermark(got, seeded) {
		t.Errorf("watermark = %+v, want the seeded one kept", got)
	}
}

// TestArchiveLoopStopsAtTheBudgetButEveryDatabaseGetsABatch: one deadline
// bounds every database's extra batches. Once the first database's second
// batch ends past the budget it takes no third one, and the next database still
// takes its first (gt-o4sk8.1).
func TestArchiveLoopStopsAtTheBudgetButEveryDatabaseGetsABatch(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	sm := d.patrolConfig.Patrols.ScheduledMaintenance
	sm.ArchiveCommitLimit = &limit
	sm.ArchiveBudget = "10m"
	clock := gcTestNow
	d.maint.now = func() time.Time { return clock }
	withEventsFake(d, nil, nil, nil)
	open := archiveFakeOpener(t, &archiveFakeServer{
		columns: []string{"commit_hash", "committer", "date", "message"},
		rows:    archiveCommitRows(20, gcTestNow),
	})
	reads := map[string]int{}
	d.maint.archiveRead = func(ctx context.Context, dm *Daemon, db string, archived map[string]struct{}) ([]archiveCommit, error) {
		commits, err := dm.doltArchiveLog(ctx, db, archived)
		if err != nil {
			return nil, err
		}
		reads[db]++
		if db == "gt" && reads[db] == 2 {
			// The first database's second batch ends after the budget.
			clock = gcTestNow.Add(11 * time.Minute)
		}
		return commits, nil
	}
	d.maint.archiveOpenDB = open

	out, unarchived, _ := d.maintenanceArchive([]string{"gt", "hq"})

	if out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
		t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
	}
	if reads["gt"] != 2 {
		t.Errorf("gt read %d time(s), want 2: the deadline ends its loop after the second batch", reads["gt"])
	}
	if reads["hq"] != 1 {
		t.Errorf("hq read %d time(s), want 1: every database gets a batch even past the deadline", reads["hq"])
	}
	if got := readArchiveCSV(t, archiveFile(dir, "gt")); len(got) != 2*limit+1 {
		t.Errorf("gt archive holds %d row(s), want the 2 batches' %d", len(got)-1, 2*limit)
	}
	if got := readArchiveCSV(t, archiveFile(dir, "hq")); len(got) != limit+1 {
		t.Errorf("hq archive holds %d row(s), want its one batch's %d", len(got)-1, limit)
	}
	// A budget stop leaves the database possibly behind, so the log names the
	// export and says so rather than reading as a finished step.
	for _, want := range []string{"gt dolt_log: stopped at the cycle's 10m0s archive_budget, still behind", "hq dolt_log: stopped at the cycle's 10m0s archive_budget, still behind"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not report the budget stop %q:\n%s", want, logs.String())
		}
	}
}

// TestArchiveLoopStopsWhenNothingIsBehindOrThereIsNoProgress: a database with
// no fresh rows costs one read and writes no file, and a batch that records the
// watermark it started with ends the loop rather than repeating itself
// (gt-o4sk8.1).
func TestArchiveLoopStopsWhenNothingIsBehindOrThereIsNoProgress(t *testing.T) {
	t.Parallel()

	t.Run("nothing is behind", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		fake := withArchiveFakes(d, f, nil, nil)

		if out, unarchived, _ := d.maintenanceArchive([]string{"gt"}); out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
			t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
		}
		if fake.reads != 1 {
			t.Errorf("dolt_log read %d time(s), want 1: a short batch ends the loop", fake.reads)
		}
		if _, err := os.Stat(archiveFile(dir, "gt")); !os.IsNotExist(err) {
			t.Errorf("a database with nothing new wrote %s (stat err %v)", archiveFile(dir, "gt"), err)
		}
	})

	t.Run("a batch records no progress", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		withGCFakes(t, d)
		dir := t.TempDir()
		d.patrolConfig = archiveConfig(dir)
		limit := 1 // every batch is limit-sized, so only the no-progress rule can end the loop
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveCommitLimit = &limit
		d.maint.now = func() time.Time { return gcTestNow }
		withEventsFake(d, nil, nil, nil)
		wm := ArchiveWatermark{Date: gcTestNow, Hashes: []string{"h1"}}
		if err := recordArchiveWatermark(d.config.TownRoot, "gt", wm); err != nil {
			t.Fatal(err)
		}
		reads := 0
		d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
			reads++
			if reads > 2 {
				return nil, errors.New("the loop kept taking batches that made no progress")
			}
			// Dated before the watermark, so writing it cannot move the
			// recorded watermark.
			return []archiveCommit{{Hash: "h0", Committer: "c", Date: gcTestNow.Add(-time.Second), Message: "stale"}}, nil
		}

		if out, unarchived, _ := d.maintenanceArchive([]string{"gt"}); out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
			t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
		}
		if reads != 1 {
			t.Errorf("dolt_log read %d time(s), want 1: a batch that records no progress ends the loop", reads)
		}
	})
}

// TestArchiveLoopStopsWhenABatchFailsMidLoop: a failure after a full batch ends
// the loop, keeps the watermark that batch recorded, and names the export in one
// escalation (gt-o4sk8.1).
func TestArchiveLoopStopsWhenABatchFailsMidLoop(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 5
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveCommitLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow }
	withEventsFake(d, nil, nil, nil)
	commits := make([]archiveCommit, 0, 20)
	for i := 1; i <= 20; i++ {
		commits = append(commits, archiveCommit{
			Hash: fmt.Sprintf("h%05d", i), Committer: "c", Date: gcTestNow.Add(time.Duration(i) * time.Second), Message: "m",
		})
	}
	reads := 0
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		reads++
		if reads == 1 {
			return commits[:limit], nil
		}
		return nil, errors.New("the server went away")
	}

	out, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("maintenanceArchive = %v, want completed", out)
	}
	if reads != 2 {
		t.Errorf("dolt_log read %d time(s), want 2: the failing batch ends the loop", reads)
	}
	if !unarchived["gt"] || len(unarchived) != 1 {
		t.Errorf("unarchived = %v, want gt marked: its archive did not finish", unarchived)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "gt dolt_log") || !strings.Contains(f.escalations[0], "the server went away") {
		t.Errorf("escalations = %v, want one naming the failed dolt_log export", f.escalations)
	}
	if got := readArchiveCSV(t, archiveFile(dir, "gt")); len(got) != limit+1 {
		t.Errorf("archive holds %d row(s), want the first batch's %d once", len(got)-1, limit)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if want := gcTestNow.Add(time.Duration(limit) * time.Second); !st.ArchiveWatermarks["gt"].Date.Equal(want) {
		t.Errorf("watermark date = %s, want %s (the first batch kept after the failure)", st.ArchiveWatermarks["gt"].Date, want)
	}
}

// TestArchiveLoopHasAnIterationBound: a read that never runs dry ends at the
// per-cycle batch bound even when the injected clock never reaches the
// deadline, and says so (gt-o4sk8.1).
func TestArchiveLoopHasAnIterationBound(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	withGCFakes(t, d)
	dir := t.TempDir()
	d.patrolConfig = archiveConfig(dir)
	limit := 1
	d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveCommitLimit = &limit
	d.maint.now = func() time.Time { return gcTestNow } // never reaches the deadline
	withEventsFake(d, nil, nil, nil)
	// A no-op append keeps the batches on the loop rather than on writing and
	// re-reading a growing archive.
	d.maint.archiveAppend = func(string, []string, [][]string) error { return nil }
	reads := 0
	d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
		reads++
		return []archiveCommit{{
			Hash: fmt.Sprintf("h%06d", reads), Committer: "c", Date: gcTestNow.Add(time.Duration(reads) * time.Second), Message: "m",
		}}, nil
	}

	out, unarchived, _ := d.maintenanceArchive([]string{"gt"})

	if out.outcome != gcOutcomeCompleted || len(unarchived) != 0 {
		t.Fatalf("maintenanceArchive = %v, unarchived %v; want completed with none", out, unarchived)
	}
	if reads != maintenanceArchiveMaxBatchesPerExport {
		t.Errorf("dolt_log read %d time(s), want the %d-batch per-cycle bound", reads, maintenanceArchiveMaxBatchesPerExport)
	}
	if !strings.Contains(logs.String(), "per-cycle bound") {
		t.Errorf("log does not report the per-cycle bound:\n%s", logs.String())
	}
}

// TestArchiveBudgetSetting: archive_budget unset reads ten minutes and "10m"
// parses; a value below a minute, above the step's 30-minute timeout, or one
// that does not parse is refused naming the key, and the step archives nothing
// and runs no gc (gt-o4sk8.1).
func TestArchiveBudgetSetting(t *testing.T) {
	t.Parallel()

	t.Run("unset reads ten minutes", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		if got, err := d.maintenanceArchiveBudget(); err != nil || got != config.DefaultArchiveBudget {
			t.Errorf("unset budget = %v, %v; want the %v default", got, err, config.DefaultArchiveBudget)
		}
		d.patrolConfig = archiveConfig(t.TempDir())
		if got, err := d.maintenanceArchiveBudget(); err != nil || got != 10*time.Minute {
			t.Errorf("budget with the section present but unset = %v, %v; want 10m", got, err)
		}
	})

	t.Run("10m parses", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.patrolConfig = archiveConfig(t.TempDir())
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveBudget = "10m"
		if got, err := d.maintenanceArchiveBudget(); err != nil || got != 10*time.Minute {
			t.Errorf("archive_budget 10m = %v, %v; want 10m", got, err)
		}
	})

	for _, tt := range []struct {
		name    string
		setting string
	}{
		{name: "below a minute", setting: "30s"},
		{name: "above the step timeout", setting: "45m"},
		{name: "unparseable", setting: "abc"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs := gcTestDaemon(t)
			f := withGCFakes(t, d)
			f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}}
			d.patrolConfig = archiveConfig(t.TempDir())
			d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveBudget = tt.setting
			d.maint.archiveRead = func(context.Context, *Daemon, string, map[string]struct{}) ([]archiveCommit, error) {
				t.Error("the dolt_log read ran with an invalid archive_budget")
				return nil, nil
			}

			d.runScheduledMaintenance()

			if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], config.MaintenanceArchiveBudgetKey) {
				t.Errorf("escalations = %v, want one naming %s", f.escalations, config.MaintenanceArchiveBudgetKey)
			}
			if len(f.gcCalls) != 0 {
				t.Errorf("gc calls = %v, want none while no database was archived", f.gcCalls)
			}
			if !strings.Contains(logs.String(), config.MaintenanceArchiveBudgetKey) {
				t.Errorf("log does not name the refused setting %s:\n%s", config.MaintenanceArchiveBudgetKey, logs.String())
			}
		})
	}
}

// TestArchiveLimitsRefuseABadSettingAtTheRead: each production read refuses a
// bound it cannot parse before opening a connection, so a caller that bypasses
// the step's own check still fails closed and names the key (gt-s6srv).
func TestArchiveLimitsRefuseABadSettingAtTheRead(t *testing.T) {
	t.Parallel()
	bad := -1

	t.Run("commits", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		withGCFakes(t, d)
		d.patrolConfig = archiveConfig(t.TempDir())
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveCommitLimit = &bad
		d.maint.archiveOpenDB = func(string) (*sql.DB, error) {
			t.Error("the dolt_log read opened a connection with an invalid archive_commit_limit")
			return nil, errors.New("not reached")
		}

		_, err := d.doltArchiveLog(context.Background(), "gt", nil)
		if err == nil || !strings.Contains(err.Error(), config.MaintenanceArchiveCommitLimitKey) {
			t.Errorf("dolt_log read = %v, want an error naming %s", err, config.MaintenanceArchiveCommitLimitKey)
		}
	})

	t.Run("events", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		withGCFakes(t, d)
		d.patrolConfig = archiveConfig(t.TempDir())
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &bad
		d.maint.archiveOpenDB = func(string) (*sql.DB, error) {
			t.Error("the events read opened a connection with an invalid archive_events_limit")
			return nil, errors.New("not reached")
		}

		_, err := d.doltArchiveEvents(context.Background(), "gt", "")
		if err == nil || !strings.Contains(err.Error(), config.MaintenanceArchiveEventsLimitKey) {
			t.Errorf("events read = %v, want an error naming %s", err, config.MaintenanceArchiveEventsLimitKey)
		}
	})

	t.Run("events export", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		withGCFakes(t, d)
		d.patrolConfig = archiveConfig(t.TempDir())
		d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveEventsLimit = &bad
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
			t.Error("the events export read with an invalid archive_events_limit")
			return archiveEvents{}, errors.New("not reached")
		}

		_, _, _, err := d.maintenanceArchiveEvents(context.Background(), t.TempDir(), "gt", EventsArchiveWatermark{})
		if err == nil || !strings.Contains(err.Error(), config.MaintenanceArchiveEventsLimitKey) {
			t.Errorf("events export = %v, want an error naming %s", err, config.MaintenanceArchiveEventsLimitKey)
		}
	})
}
