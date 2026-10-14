package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The reader's clock: every fixture file below is written against this instant.
var usageReaderNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// usageTestDir is a town root whose usage records directory exists and is
// empty. The reader resolves <root>/.runtime/usage from the root alone, so the
// fixtures are the same files the scan writes.
func usageTestDir(t *testing.T) (root, records string) {
	t.Helper()
	root = t.TempDir()
	records = filepath.Join(root, ".runtime", "usage", usageRecordsSubdir)
	if err := os.MkdirAll(records, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, records
}

// usageRecordBody is one scan file: a complete polecat record ending at end,
// with the fields the panel adds up.
func usageRecordBody(bead, kind, class string, end time.Time, wallS int64) string {
	return fmt.Sprintf(`{"bead":%q,"kind":%q,"class":%q,"rig":"gastown","seat":null,`+
		`"complete":true,"start":%q,"end":%q,"wall_s":%d,"est_cost_usd":0.5,`+
		`"models":{"deepseek-flash":{"calls":4,"input":100,"output":20,"cache_creation":0,`+
		`"cache_read":0,"est_cost_usd":0.5}},`+
		`"totals":{"calls":4,"input":100,"output":20,"cache_creation":0,"cache_read":0,`+
		`"uncached":120},"subagents":{"calls":0,"share":0,"uncached":0},`+
		`"activity":{"tool_calls":9,"edits":1,"test_runs":2,"failed_test_runs":0}}`,
		bead, kind, class,
		end.Add(-time.Minute).UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), wallS)
}

// writeUsageRecord writes one file into the records directory, aged by age, and
// returns its path.
func writeUsageRecord(t *testing.T, records, name, body string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(records, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	setUsageMtime(t, path, usageReaderNow.Add(-age))
	return path
}

// setUsageMtime dates a file, so a fixture's age is the reader's clock's rather
// than the test run's.
func setUsageMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The reading the panel draws: the records the scan wrote, the scan's own
// state file and the sessions it could not match, and nothing that is not a
// record.
func TestUsageReaderAggregatesTheRecordsDirectory(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)
	writeUsageRecord(t, records, "gt-b.om.json",
		usageRecordBody("gt-b", "om", "om-review", usageReaderNow.Add(-2*time.Hour), 600), 2*time.Hour)
	// A file the scan did not write: the reader opens only .json records.
	writeUsageRecord(t, records, "scan.log", "not a record", time.Hour)

	usageRoot := filepath.Dir(records)
	scanAt := usageReaderNow.Add(-3 * time.Hour)
	state := filepath.Join(usageRoot, usageStateFile)
	if err := os.WriteFile(state, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	setUsageMtime(t, state, scanAt)
	unmatched := "one session\nanother session\n\n"
	if err := os.WriteFile(filepath.Join(usageRoot, usageUnmatchedFile), []byte(unmatched), 0o644); err != nil {
		t.Fatal(err)
	}

	u := newDashUsageReader(root).read(usageReaderNow)

	if u.Unavailable {
		t.Fatalf("a readable directory came back unavailable: %+v", u)
	}
	if u.Window != "24h" {
		t.Errorf("window = %q, want 24h", u.Window)
	}
	if got := len(u.TopBeads); got != 2 {
		t.Errorf("top beads = %d, want the two records: %+v", got, u.TopBeads)
	}
	if len(u.ByClassModel) != 2 {
		t.Errorf("class totals = %+v, want one row per class and model", u.ByClassModel)
	}
	if want := scanAt.UTC().Format(time.RFC3339); u.AsOf != want {
		t.Errorf("as of = %q, want the state file's mtime %q", u.AsOf, want)
	}
	if u.Unmatched != 2 {
		t.Errorf("unmatched = %d, want the file's two sessions", u.Unmatched)
	}
}

// A file too old for any list the page draws is left unopened. The observable
// is the skipped count: an unreadable file the reader opens is counted, so an
// ancient one that is not counted was never opened.
func TestUsageReaderLeavesOldFilesUnopened(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-fresh.work.json",
		usageRecordBody("gt-fresh", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)
	writeUsageRecord(t, records, "gt-bad.work.json", `{"bead":`, time.Hour)
	writeUsageRecord(t, records, "gt-old.om.json", `{"bead":`, 9*24*time.Hour)

	u := newDashUsageReader(root).read(usageReaderNow)

	if u.Skipped != 1 {
		t.Errorf("skipped = %d, want only the unreadable file inside the window: %+v", u.Skipped, u)
	}
	if len(u.TopBeads) != 1 || u.TopBeads[0].Bead != "gt-fresh" {
		t.Errorf("top beads = %+v, want the one readable record", u.TopBeads)
	}
}

// A poll re-decodes only the files whose mtime moved: the scan rewrites a
// record in place when a run ends, and 1700 files is too many to reopen on
// every minute.
func TestUsageReaderReDecodesOnlyRecordsWhoseMtimeMoved(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	path := writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	r := newDashUsageReader(root)
	if u := r.read(usageReaderNow); len(u.TopBeads) != 1 || u.TopBeads[0].Bead != "gt-a" {
		t.Fatalf("the first read gave %+v, want gt-a", u.TopBeads)
	}

	// Rewritten content under the same mtime is served from the cache.
	newBody := usageRecordBody("gt-b", "work", "polecat", usageReaderNow.Add(-time.Hour), 300)
	if err := os.WriteFile(path, []byte(newBody), 0o644); err != nil {
		t.Fatal(err)
	}
	setUsageMtime(t, path, info.ModTime())
	if u := r.read(usageReaderNow); u.TopBeads[0].Bead != "gt-a" {
		t.Errorf("a poll re-read a file whose mtime had not moved: %+v", u.TopBeads)
	}

	// An mtime that moved is a file the scan rewrote, and it is re-decoded.
	setUsageMtime(t, path, info.ModTime().Add(time.Minute))
	if u := r.read(usageReaderNow); u.TopBeads[0].Bead != "gt-b" {
		t.Errorf("a poll served a stale decode for a file that changed: %+v", u.TopBeads)
	}

	// A file the scan removed leaves the cache with it, so the map stays the
	// size of the directory the poll just read.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r.read(usageReaderNow)
	if _, ok := r.cache[path]; ok {
		t.Error("the cache kept a file the directory no longer holds")
	}
}

// A directory that is not there and a directory with nothing in it are
// different readings: the first is a fault the page names, the second is a town
// that spent nothing.
func TestUsageReaderTellsAnEmptyDayFromAnUnreadableOne(t *testing.T) {
	t.Parallel()

	if u := newDashUsageReader(t.TempDir()).read(usageReaderNow); !u.Unavailable {
		t.Errorf("a missing records directory gave %+v, want unavailable", u)
	}

	root, _ := usageTestDir(t)
	u := newDashUsageReader(root).read(usageReaderNow)
	if u.Unavailable {
		t.Errorf("an empty records directory read as unavailable: %+v", u)
	}
	if len(u.TopBeads) != 0 || u.Skipped != 0 || u.Incomplete != 0 || u.Unmatched != 0 {
		t.Errorf("an empty day is not empty: %+v", u)
	}
	// Without the scan's state file there is no scan time to draw.
	if u.AsOf != "" {
		t.Errorf("as of = %q, want empty with no state file", u.AsOf)
	}
}
