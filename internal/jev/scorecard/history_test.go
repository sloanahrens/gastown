package scorecard

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var historyNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// historyDoc is a refresh's document: a battery scored fresh, one reused from
// the cache, one kept from a failed refresh, and one that failed with nothing
// to keep.
func historyDoc() CacheDoc {
	report := func(name string, labeled, positives int) *Report {
		return &Report{Battery: name, Questions: []QuestionReport{
			{ID: "severity", Labeled: labeled, Positives: positives, Verdict: VerdictNotYet},
		}}
	}
	kept := historyNow.Add(-time.Hour)
	return CacheDoc{
		ScoredAt: historyNow,
		Since:    "7d",
		Batteries: []BatteryBlock{
			{Name: "finding", ScoredAt: historyNow, Report: report("finding", 12, 3)},
			{Name: "drift", ScoredAt: historyNow.Add(-24 * time.Hour), Report: report("drift", 9, 1)},
			{Name: "stale", ScoredAt: kept, Failed: true, Kept: true, KeptSince: &kept, Report: report("stale", 5, 0)},
			{Name: "logtriage", ScoredAt: historyNow, Failed: true, Error: "no log"},
		},
	}
}

// Each refresh that runs appends exactly one line to the history, so two
// refreshes leave two rows, in the order they ran.
func TestRefreshAppendsOneHistoryLinePerRefresh(t *testing.T) {
	t.Parallel()
	path := HistoryPath(t.TempDir())
	cache := Cache{}
	fresh := map[string]bool{"finding": true}

	cache.AppendHistory(path, HistoryRowFor(historyNow, "7d", historyDoc(), fresh), historyNow, nil)
	cache.AppendHistory(path, HistoryRowFor(historyNow.Add(time.Hour), "7d", historyDoc(), fresh), historyNow.Add(time.Hour), nil)

	rows, err := cache.ReadHistory(path)
	if err != nil {
		t.Fatalf("reading the history: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("history has %d rows, want 2", len(rows))
	}
	if !rows[1].At.Equal(historyNow.Add(time.Hour)) {
		t.Errorf("last row at %v, want the second refresh's time", rows[1].At)
	}
}

// A battery reused unchanged, kept from a failed refresh, or failed with no
// report has no entry in the row; only the battery scored fresh does.
func TestHistorySkipsAReusedOrKeptBlock(t *testing.T) {
	t.Parallel()
	row := HistoryRowFor(historyNow, "7d", historyDoc(), map[string]bool{
		"finding":   true,
		"stale":     true,
		"logtriage": true,
	})
	if len(row.Batteries) != 1 || row.Batteries[0].Name != "finding" {
		t.Fatalf("row batteries = %+v, want only finding", row.Batteries)
	}
	q := row.Batteries[0].Questions
	if len(q) != 1 || q[0].ID != "severity" || q[0].Labeled != 12 || q[0].Positives != 3 || q[0].Verdict != VerdictNotYet {
		t.Errorf("finding questions = %+v, want severity 12 labeled, 3 positive, not-yet", q)
	}
}

// A row older than HistoryRetention is dropped on the next write, a row inside
// it is kept, and a line that is not a dated row is kept as it was written.
func TestHistoryTrimsToNinetyDays(t *testing.T) {
	t.Parallel()
	path := HistoryPath(t.TempDir())
	old := HistoryRow{At: historyNow.Add(-91 * 24 * time.Hour), Since: "7d"}
	recent := HistoryRow{At: historyNow.Add(-89 * 24 * time.Hour), Since: "7d"}
	cache := Cache{}
	seed := []string{mustJSON(t, old), mustJSON(t, recent), "not a dated row"}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(seed, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache.AppendHistory(path, HistoryRow{At: historyNow, Since: "7d"}, historyNow, nil)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("history has %d lines, want 3 (recent, unparseable, new):\n%s", len(lines), data)
	}
	if lines[0] != mustJSON(t, recent) || lines[1] != "not a dated row" {
		t.Errorf("kept lines = %q, want the recent row and the unparseable line in order", lines[:2])
	}
}

// A history write that fails costs one log line and nothing else: the call
// returns no error, and the previous file is left as it was.
func TestHistoryWriteFailureDoesNotFailTheRefresh(t *testing.T) {
	t.Parallel()
	path := HistoryPath(t.TempDir())
	before := HistoryRow{At: historyNow.Add(-time.Hour), Since: "7d"}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(mustJSON(t, before)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failing := Cache{WriteFile: func(string, []byte, os.FileMode) error {
		return errors.New("disk full")
	}}
	var log bytes.Buffer

	failing.AppendHistory(path, HistoryRow{At: historyNow, Since: "7d"}, historyNow, &log)

	if got := strings.Count(log.String(), "\n"); got != 1 {
		t.Errorf("log has %d lines, want one: %q", got, log.String())
	}
	if !strings.Contains(log.String(), "disk full") {
		t.Errorf("log %q does not name the write failure", log.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != mustJSON(t, before) {
		t.Errorf("history changed after a failed write: %s", data)
	}
}

// A history that cannot be read is not rewritten from nothing: the call logs
// one line and leaves the write undone.
func TestHistoryUnreadableFileIsNotRewritten(t *testing.T) {
	t.Parallel()
	path := HistoryPath(t.TempDir())
	written := false
	cache := Cache{
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("permission denied") },
		WriteFile: func(string, []byte, os.FileMode) error {
			written = true
			return nil
		},
	}
	var log bytes.Buffer

	cache.AppendHistory(path, HistoryRow{At: historyNow}, historyNow, &log)

	if written {
		t.Error("history was written over a file that could not be read")
	}
	if !strings.Contains(log.String(), "permission denied") {
		t.Errorf("log %q does not name the read failure", log.String())
	}
}

// A missing history is ErrNoHistory, and a line that does not parse is
// ErrCorruptHistory naming the line.
func TestReadHistoryReportsMissingAndCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.jsonl")
	if _, err := (Cache{}).ReadHistory(missing); !errors.Is(err, ErrNoHistory) {
		t.Errorf("missing history: err = %v, want ErrNoHistory", err)
	}

	corrupt := filepath.Join(dir, "corrupt.jsonl")
	body := mustJSON(t, HistoryRow{At: historyNow}) + "\n{not json\n"
	if err := os.WriteFile(corrupt, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (Cache{}).ReadHistory(corrupt)
	if !errors.Is(err, ErrCorruptHistory) {
		t.Fatalf("corrupt history: err = %v, want ErrCorruptHistory", err)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q does not name line 2", err)
	}
}

// The view prints a change in labeled per row, and ends a series with the
// count of refreshes that saw no growth once three or more share a count.
func TestWriteHistoryPrintsChangeAndNoGrowthRun(t *testing.T) {
	t.Parallel()
	rowAt := func(i, labeled int) HistoryRow {
		return HistoryRow{
			At: historyNow.Add(time.Duration(i) * time.Hour),
			Batteries: []HistoryBattery{{Name: "logtriage", Questions: []HistoryQuestion{
				{ID: "severity", Labeled: labeled, Positives: 2, Verdict: VerdictNotYet},
			}}},
		}
	}
	cases := []struct {
		name    string
		labeled []int
		want    []string
		absent  []string
	}{
		{
			name:    "growth then three equal rows",
			labeled: []int{10, 12, 12, 12},
			want:    []string{"-", "+2", "+0", "no growth in 3 refreshes"},
		},
		{
			name:    "two equal rows are not a run",
			labeled: []int{10, 12, 12},
			absent:  []string{"no growth"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rows []HistoryRow
			for i, n := range tc.labeled {
				rows = append(rows, rowAt(i, n))
			}
			var out bytes.Buffer
			if err := WriteHistory(&out, rows, "logtriage", "severity"); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out.String(), a) {
					t.Errorf("output has %q, want none:\n%s", a, out.String())
				}
			}
		})
	}
}

// A row that cannot be encoded, as a date past JSON's year range, costs one log
// line and leaves the file as it was.
func TestHistoryRefusesARowThatCannotEncode(t *testing.T) {
	t.Parallel()
	path := HistoryPath(t.TempDir())
	var log bytes.Buffer
	row := HistoryRow{At: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Since: "7d"}

	(Cache{}).AppendHistory(path, row, historyNow, &log)

	if !strings.Contains(log.String(), "encoding the scorecard history row") {
		t.Errorf("log %q does not name the encoding failure", log.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("history file exists after an unencodable row: %v", err)
	}
}

// A history directory that cannot be created costs one log line: the history
// reads as absent, and its parent is a regular file, so MkdirAll fails and
// nothing is written.
func TestHistoryDirectoryFailureIsLoggedNotReturned(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	blocker := filepath.Join(town, "blocked")
	if err := os.WriteFile(blocker, []byte("a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "scorecard.history.jsonl")
	cache := Cache{ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }}
	var log bytes.Buffer

	cache.AppendHistory(path, HistoryRow{At: historyNow, Since: "7d"}, historyNow, &log)

	if got := strings.Count(log.String(), "\n"); got != 1 {
		t.Errorf("log has %d lines, want one: %q", got, log.String())
	}
	if !strings.Contains(log.String(), "creating the scorecard history directory") {
		t.Errorf("log %q does not name the directory failure", log.String())
	}
}

// A history file that cannot be read is an error, not an absent history, so
// the view never shows an empty history for a file it could not read.
func TestReadHistoryReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	cache := Cache{ReadFile: func(string) ([]byte, error) { return nil, errors.New("permission denied") }}

	_, err := cache.ReadHistory("unused.jsonl")
	if err == nil || errors.Is(err, ErrNoHistory) {
		t.Fatalf("err = %v, want a read error that is not ErrNoHistory", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not name the read failure", err)
	}
}

// A history line with no at is refused, naming its line, as one that does not
// parse is.
func TestReadHistoryRefusesALineWithoutAt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "scorecard.history.jsonl")
	if err := os.WriteFile(path, []byte("{\"since\":\"7d\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (Cache{}).ReadHistory(path)
	if !errors.Is(err, ErrCorruptHistory) || !strings.Contains(err.Error(), "line 1 has no at") {
		t.Errorf("err = %v, want ErrCorruptHistory naming line 1 with no at", err)
	}
}

// A view whose output cannot be written returns the write error, whether it
// was writing a series or the line that says there is no history to show.
func TestWriteHistoryReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	series := []HistoryRow{{At: historyNow, Batteries: []HistoryBattery{{Name: "logtriage", Questions: []HistoryQuestion{
		{ID: "severity", Labeled: 1, Verdict: VerdictNotYet},
	}}}}}
	cases := []struct {
		name string
		rows []HistoryRow
	}{
		{"a series", series},
		{"no rows", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := WriteHistory(failingWriter{}, tc.rows, "", ""); err == nil {
				t.Error("WriteHistory returned nil on a failing writer, want the write error")
			}
		})
	}
}

// failingWriter refuses every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// mustJSON encodes v as one line of JSON, as the writer does.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %T: %v", v, err)
	}
	return string(data)
}
