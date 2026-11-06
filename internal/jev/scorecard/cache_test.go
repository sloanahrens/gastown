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
