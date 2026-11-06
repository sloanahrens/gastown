package scorecard

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// cacheTestDoc is a document with every block field in use: a good block with
// its report, a failed block kept from the run before, and a failed block that
// never scored.
func cacheTestDoc(at time.Time) CacheDoc {
	scored := at.Add(-2 * time.Hour)
	since := at.Add(-7 * 24 * time.Hour)
	rate := 0.5
	return CacheDoc{
		ScoredAt: at,
		Since:    "7d",
		Batteries: []BatteryBlock{
			{
				Name:     "finding",
				ScoredAt: scored,
				Report: &Report{
					Battery:   "finding",
					Since:     &since,
					Items:     12,
					Labeled:   9,
					Questions: []QuestionReport{{ID: "fix_worthy", Kind: KindNoul, Logged: 12, Labeled: 9, BaseRate: &rate}},
				},
			},
			{
				Name:      "drift",
				ScoredAt:  scored,
				Failed:    true,
				Error:     "reading the drift log",
				Kept:      true,
				KeptSince: &scored,
				Report:    &Report{Battery: "drift", Items: 4, Labeled: 2},
			},
			{Name: "stale", ScoredAt: at, Failed: true, Error: "no labels"},
		},
	}
}

// FingerprintLog counts a log's lines and records its size; a last line with
// no newline still counts, an empty log is zero of both, and a line longer
// than a scanner's token cap is one line.
func TestFingerprintLogCountsLinesAndBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		body  string
		lines int
	}{
		{"trailing newline", "a\nb\n", 2},
		{"no trailing newline", "a\nb", 2},
		{"one line", "a\n", 1},
		{"empty", "", 0},
		{"long line", strings.Repeat("x", 256*1024) + "\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "log.jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			fp, err := FingerprintLog(path)
			if err != nil {
				t.Fatalf("fingerprinting: %v", err)
			}
			if fp.Lines != tc.lines || fp.Bytes != int64(len(tc.body)) {
				t.Errorf("fingerprint = %d lines %d bytes, want %d lines %d bytes", fp.Lines, fp.Bytes, tc.lines, len(tc.body))
			}
		})
	}
}

// A log that cannot be read is an error naming it, not an empty log: the
// refresh must score the battery rather than reuse its block.
func TestFingerprintLogRefusesAMissingLog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent.jsonl")
	_, err := FingerprintLog(path)
	if err == nil {
		t.Fatal("fingerprinting a missing log succeeded")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %s", err, path)
	}
}

// A path that opens but cannot be read, such as a directory, is the same kind
// of error: the refresh scores the battery instead of reading it as empty.
func TestFingerprintLogRefusesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	_, err := FingerprintLog(path)
	if err == nil {
		t.Fatal("fingerprinting a directory succeeded")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %s", err, path)
	}
}

// A block is reused only while it holds a good report scored over the same
// window, less than ReuseAge ago, against the same log fingerprint.
func TestReusableBlock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	scored := now.Add(-24 * time.Hour)
	fp := LogFingerprint{Lines: 3, Bytes: 30}
	prev := func(mut func(*BatteryBlock)) CacheDoc {
		b := BatteryBlock{Name: "finding", ScoredAt: scored, LogLines: 3, LogBytes: 30, Report: &Report{Battery: "finding"}}
		if mut != nil {
			mut(&b)
		}
		return CacheDoc{ScoredAt: scored, Since: "7d", Batteries: []BatteryBlock{b}}
	}
	cases := []struct {
		name   string
		prev   CacheDoc
		fp     LogFingerprint
		window string
		want   bool
	}{
		{"unchanged", prev(nil), fp, "7d", true},
		{"just under ReuseAge", prev(func(b *BatteryBlock) { b.ScoredAt = now.Add(-ReuseAge + time.Minute) }), fp, "7d", true},
		{"log gained a line", prev(nil), LogFingerprint{Lines: 4, Bytes: 31}, "7d", false},
		{"log gained a byte", prev(nil), LogFingerprint{Lines: 3, Bytes: 31}, "7d", false},
		{"log cannot be measured", prev(nil), LogFingerprint{}, "7d", false},
		{"block is ReuseAge old", prev(func(b *BatteryBlock) { b.ScoredAt = now.Add(-ReuseAge) }), fp, "7d", false},
		{"block is past ReuseAge", prev(func(b *BatteryBlock) { b.ScoredAt = now.Add(-4 * 24 * time.Hour) }), fp, "7d", false},
		{"block scored over another window", prev(nil), fp, "14d", false},
		{"block failed", prev(func(b *BatteryBlock) { b.Failed = true }), fp, "7d", false},
		{"block kept", prev(func(b *BatteryBlock) { b.Kept = true }), fp, "7d", false},
		{"block has no report", prev(func(b *BatteryBlock) { b.Report = nil }), fp, "7d", false},
		{"another battery", prev(func(b *BatteryBlock) { b.Name = "drift" }), fp, "7d", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ReusableBlock(tc.prev, "finding", tc.fp, tc.window, now)
			if ok != tc.want {
				t.Fatalf("reusable = %v, want %v", ok, tc.want)
			}
			if !tc.want {
				if got != (BatteryBlock{}) {
					t.Errorf("refused block = %+v, want the zero block", got)
				}
				return
			}
			if !got.ScoredAt.Equal(tc.prev.Batteries[0].ScoredAt) {
				t.Errorf("reused scored_at = %v, want the block's %v", got.ScoredAt, tc.prev.Batteries[0].ScoredAt)
			}
			if got.Report != tc.prev.Batteries[0].Report {
				t.Errorf("reused report = %v, want the block's own", got.Report)
			}
		})
	}
}

// A kept block carries the fingerprint of the report it keeps, so a later
// refresh compares the log against the score that report came from.
func TestKeepFailedKeepsTheGoodBlocksFingerprint(t *testing.T) {
	t.Parallel()
	scored := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	prev := CacheDoc{Batteries: []BatteryBlock{{
		Name: "finding", ScoredAt: scored, LogLines: 7, LogBytes: 700,
		Report: &Report{Battery: "finding", Items: 7},
	}}}
	failed := BatteryBlock{Name: "finding", ScoredAt: scored.Add(time.Hour), Failed: true, Error: "boom", LogLines: 9, LogBytes: 900}

	got := KeepFailed(prev, failed)
	if !got.Kept {
		t.Fatalf("kept = false, want the previous report kept")
	}
	if got.LogLines != 7 || got.LogBytes != 700 {
		t.Errorf("kept fingerprint = %d lines %d bytes, want the kept report's 7 and 700", got.LogLines, got.LogBytes)
	}
	if !got.ScoredAt.Equal(scored) {
		t.Errorf("kept scored_at = %v, want %v", got.ScoredAt, scored)
	}
}

func TestCacheWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "scorecard.json")
	want := cacheTestDoc(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))

	if err := (Cache{}).Write(path, want); err != nil {
		t.Fatalf("writing the scorecard: %v", err)
	}
	got, err := (Cache{}).Read(path)
	if err != nil {
		t.Fatalf("reading the scorecard: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read %+v, want %+v", got, want)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "scorecard.json" {
		t.Errorf("cache directory holds %v, want only scorecard.json", entries)
	}
}

// A cache path whose parent cannot be created is an error that names the
// directory, not a quiet success.
func TestCacheWriteReportsAnUncreatableDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := CachePath(town)
	if err := os.WriteFile(filepath.Dir(filepath.Dir(path)), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := (Cache{}).Write(path, cacheTestDoc(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)))
	if err == nil {
		t.Fatal("writing a cache under a file succeeded")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %s", err, path)
	}
}

// A write that cannot create its temp file leaves the previous file exactly as
// it was.
func TestCacheFailedWriteLeavesThePreviousFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "scorecard.json")
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := (Cache{}).Write(path, cacheTestDoc(at)); err != nil {
		t.Fatalf("writing the first scorecard: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	//testpolicy:allow no-exec-files — the target is the cache directory: a directory that cannot be written to keeps its search bit, and the write bit is what this drops
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the same cache directory: traversal needs its search bit back
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	next := cacheTestDoc(at.Add(time.Hour))
	if err := (Cache{}).Write(path, next); err == nil {
		t.Fatal("writing the scorecard into a read-only directory succeeded")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("failed write changed the cache file:\nbefore: %s\nafter:  %s", before, after)
	}
}

// A document JSON cannot carry is an error, not a cache file holding a
// truncated one.
func TestCacheWriteRefusesAnUnencodableDocument(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "scorecard.json")
	nan := math.NaN()
	doc := CacheDoc{
		ScoredAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		Since:    "7d",
		Batteries: []BatteryBlock{{
			Name:     "finding",
			ScoredAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
			Report:   &Report{Questions: []QuestionReport{{ID: "fix_worthy", BaseRate: &nan}}},
		}},
	}
	if err := (Cache{}).Write(path, doc); err == nil {
		t.Fatal("writing a document holding NaN succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused write left a file at %s", path)
	}
}

func TestCacheMissingFileIsNoScorecardYet(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "scorecard.json")
	_, err := (Cache{}).Read(path)
	if !errors.Is(err, ErrNoScorecard) {
		t.Fatalf("reading a missing cache gave %v, want ErrNoScorecard", err)
	}
	if errors.Is(err, ErrCorruptScorecard) {
		t.Errorf("a missing cache is also reported corrupt: %v", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "no scorecard yet") {
		t.Errorf("error %q does not name %s and say no scorecard yet", err, path)
	}
}

func TestCacheCorruptFileNamesThePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"not json", "not a scorecard"},
		{"no scored_at", `{}`},
		{"battery without a name", `{"scored_at":"2026-11-06T12:00:00Z","batteries":[{"scored_at":"2026-11-06T12:00:00Z"}]}`},
		{"battery without a scored_at", `{"scored_at":"2026-11-06T12:00:00Z","batteries":[{"name":"finding"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "scorecard.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := (Cache{}).Read(path)
			if !errors.Is(err, ErrCorruptScorecard) {
				t.Fatalf("reading %q gave %v, want ErrCorruptScorecard", tc.body, err)
			}
			if errors.Is(err, ErrNoScorecard) {
				t.Errorf("a corrupt cache is also reported missing: %v", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name %s", err, path)
			}
		})
	}
}

// A file that exists but cannot be read is neither missing nor corrupt: the
// error says which file failed and carries the read error.
func TestCacheUnreadableFileNamesThePath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "scorecard.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := (Cache{}).Read(path)
	if err == nil {
		t.Fatal("reading an unreadable cache succeeded")
	}
	if errors.Is(err, ErrNoScorecard) || errors.Is(err, ErrCorruptScorecard) {
		t.Errorf("an unreadable cache is reported as missing or corrupt: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %s", err, path)
	}
}
