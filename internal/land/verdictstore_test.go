package land

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The verdict store keeps om's answer whole: the bead notes keep only each
// finding's id, severity, path and title, and reviews.jsonl keeps a count
// (gt-uxqb9).

const storeTestHead = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

// storedVerdict reads the one record stored for bead and head back.
func storedVerdict(t *testing.T, store *VerdictStore, bead, head string) VerdictRecord {
	t.Helper()
	path := filepath.Join(VerdictsDir(store.TownRoot), store.Rig, bead, headPrefix(head)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the stored verdict: %v", err)
	}
	var rec VerdictRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("the stored verdict is not JSON: %v\n%s", err, data)
	}
	return rec
}

// TestOMReviewerStoresTheVerdict: a review that reaches a verdict keeps om's
// JSON verbatim beside the bead and head it judged, so the summary and the
// finding details survive the landing.
func TestOMReviewerStoresTheVerdict(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store := TownVerdicts(town, "gastown")
	store.Now = func() time.Time { return now }
	body := `{"score":0.9,"verdict":"approve","summary":"ok","findings":[{"id":"F1","severity":"minor","path":"a.go","line":3,"title":"nit","detail":"explain"}]}`
	var argv []string
	r := OMReviewer{OutDir: t.TempDir(), Verdicts: store}
	r.run = fakeOM(t, body, 0, nil, &argv)
	v, err := r.ReviewBead(context.Background(), ReviewInput{
		Dir: "/wt", Base: "aaa", Head: storeTestHead, BeadID: "gt-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictApprove || v.Version != stubOMVersion {
		t.Fatalf("verdict = %+v, want an approve by %s", v, stubOMVersion)
	}
	rec := storedVerdict(t, store, "gt-abc", storeTestHead)
	if rec.Bead != "gt-abc" || rec.Rig != "gastown" || rec.Head != storeTestHead {
		t.Errorf("stored bead/rig/head = %q/%q/%q", rec.Bead, rec.Rig, rec.Head)
	}
	if !rec.At.Equal(now) {
		t.Errorf("stored at = %v, want the store's clock %v", rec.At, now)
	}
	if rec.OMVersion != stubOMVersion {
		t.Errorf("stored om_version = %q, want %q", rec.OMVersion, stubOMVersion)
	}
	if want := []string{"-C", "--base", "--head", "--out", "--bead"}; !slices.Equal(rec.Flags, want) {
		t.Errorf("stored flags = %q, want %q", rec.Flags, want)
	}
	// The wrapper nests om's JSON whole rather than flattening it, so a reader
	// gets the fields the notes dropped.
	if string(rec.Verdict) != body {
		t.Errorf("stored verdict = %s, want om's JSON verbatim %s", rec.Verdict, body)
	}
	// The write is atomic: the directory holds the record and no temp file.
	beadDir := filepath.Join(VerdictsDir(town), "gastown", "gt-abc")
	entries, err := os.ReadDir(beadDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != headPrefix(storeTestHead)+".json" {
		t.Errorf("bead directory holds %v, want only the verdict file", entries)
	}
}

// TestOMReviewerStoresTheVerdictBeforeTheThreshold: the store records what om
// said, though the landing's own threshold turns the approval into a rejection.
func TestOMReviewerStoresTheVerdictBeforeTheThreshold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".om.json"), []byte(`{"threshold":0.95}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := TownVerdicts(t.TempDir(), "gastown")
	body := `{"score":0.9,"verdict":"approve","findings":[]}`
	var argv []string
	r := OMReviewer{OutDir: t.TempDir(), Verdicts: store}
	r.run = fakeOM(t, body, 0, nil, &argv)
	v, err := r.ReviewBead(context.Background(), ReviewInput{
		Dir: dir, Base: "aaa", Head: storeTestHead, BeadID: "gt-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictRequestChanges {
		t.Fatalf("verdict = %q, want the threshold to turn the approve into a rejection", v.Verdict)
	}
	if rec := storedVerdict(t, store, "gt-abc", storeTestHead); string(rec.Verdict) != body {
		t.Errorf("stored verdict = %s, want om's own %s", rec.Verdict, body)
	}
}

// TestOMReviewerKeepsTheVerdictWhenTheStoreFails: the store is a record, not a
// precondition of the landing, so a write that fails is a warning and the
// verdict stands.
func TestOMReviewerKeepsTheVerdictWhenTheStoreFails(t *testing.T) {
	t.Parallel()
	store := &VerdictStore{TownRoot: t.TempDir(), Rig: "gastown"}
	store.writeFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	var log strings.Builder
	var argv []string
	r := OMReviewer{OutDir: t.TempDir(), Verdicts: store}
	r.run = fakeOM(t, `{"score":0.9,"verdict":"approve","findings":[]}`, 0, nil, &argv)
	v, err := r.ReviewBead(context.Background(), ReviewInput{
		Dir: "/wt", Base: "aaa", Head: storeTestHead, BeadID: "gt-abc", Logf: logfTo(&log)})
	if err != nil {
		t.Fatalf("ReviewBead = %v, want the verdict to stand without the store", err)
	}
	if v.Verdict != VerdictApprove {
		t.Errorf("verdict = %+v, want the approve om wrote", v)
	}
	if got := log.String(); !strings.Contains(got, "could not store the verdict") || !strings.Contains(got, "disk full") {
		t.Errorf("log = %q, want the store failure named", got)
	}
	path := filepath.Join(VerdictsDir(store.TownRoot), "gastown", "gt-abc", headPrefix(storeTestHead)+".json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a verdict file survived the failed write: %v", err)
	}
}

// TestVerdictStoreAFailedWriteKeepsTheEarlierRecord: a write that fails is the
// atomic replace never happening, so the record already stored for the head is
// still whole and the directory holds no partial file.
func TestVerdictStoreAFailedWriteKeepsTheEarlierRecord(t *testing.T) {
	t.Parallel()
	store := TownVerdicts(t.TempDir(), "gastown")
	first := VerdictRecord{Bead: "gt-a", Head: storeTestHead, Verdict: json.RawMessage(`{"verdict":"approve"}`)}
	if err := store.Write(first, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(VerdictsDir(store.TownRoot), "gastown", "gt-a")
	path := filepath.Join(dir, headPrefix(storeTestHead)+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store.writeFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	second := VerdictRecord{Bead: "gt-a", Head: storeTestHead, Verdict: json.RawMessage(`{"verdict":"request_changes"}`)}
	if err := store.Write(second, nil); err == nil {
		t.Fatal("Write = nil, want the failed write reported")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the earlier record is gone after a failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a failed write changed the stored record:\n got %s\nwant %s", after, before)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("bead directory holds %v, want only the earlier record", entries)
	}
}

// TestVerdictStoreLogsReplacingARecord: the head names the file, so a second
// review of the same head replaces the first, and the replacement leaves a line
// rather than losing the earlier verdict silently.
func TestVerdictStoreLogsReplacingARecord(t *testing.T) {
	t.Parallel()
	store := TownVerdicts(t.TempDir(), "gastown")
	rec := VerdictRecord{Bead: "gt-a", Head: storeTestHead, Verdict: json.RawMessage(`{"verdict":"approve"}`)}
	if err := store.Write(rec, nil); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	rec.Verdict = json.RawMessage(`{"verdict":"request_changes"}`)
	if err := store.Write(rec, logfTo(&log)); err != nil {
		t.Fatal(err)
	}
	if got := log.String(); !strings.Contains(got, "replacing the record for gt-a@"+headPrefix(storeTestHead)) {
		t.Errorf("log = %q, want the replacement named", got)
	}
	if got := storedVerdict(t, store, "gt-a", storeTestHead); string(got.Verdict) != `{"verdict":"request_changes"}` {
		t.Errorf("stored verdict = %s, want the newer one", got.Verdict)
	}
}

// brokenEntry is a directory entry whose Info fails, which is what retention
// sees for a file it cannot stat.
type brokenEntry struct{ name string }

func (e brokenEntry) Name() string               { return e.name }
func (e brokenEntry) IsDir() bool                { return false }
func (e brokenEntry) Type() os.FileMode          { return 0 }
func (e brokenEntry) Info() (os.FileInfo, error) { return nil, errors.New("unreadable") }

// TestVerdictStoreReportsRetentionFailures: retention is cleanup, so each of its
// failures is a log line and the verdict the write stored stays, rather than a
// silent pass or a failed store.
func TestVerdictStoreReportsRetentionFailures(t *testing.T) {
	t.Parallel()
	rec := VerdictRecord{Bead: "gt-a", Head: storeTestHead, Verdict: json.RawMessage(`{}`)}
	write := func(t *testing.T, mutate func(*VerdictStore), seedOld int) (*VerdictStore, string, []string) {
		t.Helper()
		store := TownVerdicts(t.TempDir(), "gastown")
		store.Now = func() time.Time { return time.Now().UTC() }
		dir := filepath.Join(VerdictsDir(store.TownRoot), "gastown", "gt-a")
		var old []string
		// Each seed is a minute older than the one before, so which ones fall
		// outside the newest ten does not depend on directory order.
		for i := range seedOld {
			name := storeTestName(i)
			seedVerdictFile(t, dir, name, time.Now().Add(-40*24*time.Hour).Add(-time.Duration(i)*time.Minute))
			old = append(old, name)
		}
		mutate(store)
		return store, dir, old
	}
	t.Run("an unreadable directory", func(t *testing.T) {
		t.Parallel()
		store, _, _ := write(t, func(s *VerdictStore) {
			s.readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("permission denied") }
		}, 12)
		var log strings.Builder
		if err := store.Write(rec, logfTo(&log)); err != nil {
			t.Fatalf("Write = %v, want the verdict stored", err)
		}
		if got := log.String(); !strings.Contains(got, "for retention") || !strings.Contains(got, "permission denied") {
			t.Errorf("log = %q, want the unreadable directory named", got)
		}
	})
	t.Run("a file that cannot be stat'ed", func(t *testing.T) {
		t.Parallel()
		store, _, _ := write(t, func(s *VerdictStore) {
			s.readDir = func(string) ([]os.DirEntry, error) {
				return []os.DirEntry{brokenEntry{name: "0123456789ab.json"}}, nil
			}
		}, 0)
		var log strings.Builder
		if err := store.Write(rec, logfTo(&log)); err != nil {
			t.Fatalf("Write = %v, want the verdict stored", err)
		}
		if got := log.String(); !strings.Contains(got, "0123456789ab.json") || !strings.Contains(got, "unreadable") {
			t.Errorf("log = %q, want the unreadable file named", got)
		}
	})
	t.Run("a file that cannot be removed", func(t *testing.T) {
		t.Parallel()
		store, dir, old := write(t, func(s *VerdictStore) {
			s.remove = func(string) error { return errors.New("read-only file system") }
		}, 12)
		var log strings.Builder
		if err := store.Write(rec, logfTo(&log)); err != nil {
			t.Fatalf("Write = %v, want the verdict stored", err)
		}
		if got := log.String(); !strings.Contains(got, "read-only file system") {
			t.Errorf("log = %q, want the removal failure named", got)
		}
		// Retention continued past the first failure: all three names outside
		// the newest ten (the write and nine seeds) were attempted.
		if got := strings.Count(log.String(), "read-only file system"); got != 3 {
			t.Errorf("%d removal failures logged, want 3 (13 files, 10 kept)", got)
		}
		for _, name := range old {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("%s is gone though removal failed: %v", name, err)
			}
		}
	})
}

// TestOMReviewerStoresNothingForAReviewWithNoBead: the Review path names no
// work bead, so there is no bead directory to store a verdict under.
func TestOMReviewerStoresNothingForAReviewWithNoBead(t *testing.T) {
	t.Parallel()
	store := TownVerdicts(t.TempDir(), "gastown")
	var log strings.Builder
	var argv []string
	r := OMReviewer{OutDir: t.TempDir(), Verdicts: store, Out: &log}
	r.run = fakeOM(t, `{"score":0.9,"verdict":"approve","findings":[]}`, 0, nil, &argv)
	if _, err := r.Review(context.Background(), "/wt", "aaa", storeTestHead); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(VerdictsDir(store.TownRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a review with no bead wrote to the store: %v", err)
	}
	if got := log.String(); strings.Contains(got, "could not store the verdict") {
		t.Errorf("a review with no bead warned %q, want the store skipped rather than refused", got)
	}
}

// TestVerdictStoreRefusesAnUnsafePath: every component is one directory level,
// so a bead id or sha outside the store's alphabet is refused rather than
// naming a file elsewhere.
func TestVerdictStoreRefusesAnUnsafePath(t *testing.T) {
	t.Parallel()
	good := VerdictRecord{Bead: "gt-a", Head: storeTestHead, Verdict: json.RawMessage(`{"verdict":"approve"}`)}
	for _, tc := range []struct {
		name string
		mod  func(*VerdictStore, *VerdictRecord)
		want string
	}{
		{name: "no town root", mod: func(s *VerdictStore, _ *VerdictRecord) { s.TownRoot = "" }, want: "town root"},
		{name: "no rig", mod: func(s *VerdictStore, _ *VerdictRecord) { s.Rig = "" }, want: "rig"},
		{name: "rig names a parent", mod: func(s *VerdictStore, _ *VerdictRecord) { s.Rig = ".." }, want: "rig"},
		{name: "bead names a parent", mod: func(_ *VerdictStore, r *VerdictRecord) { r.Bead = ".." }, want: "bead"},
		{name: "bead carries a separator", mod: func(_ *VerdictStore, r *VerdictRecord) { r.Bead = "gt/a" }, want: "bead"},
		{name: "no head", mod: func(_ *VerdictStore, r *VerdictRecord) { r.Head = "" }, want: "head"},
		{name: "head is not a sha", mod: func(_ *VerdictStore, r *VerdictRecord) { r.Head = "../../etc/passwd" }, want: "head"},
		{name: "verdict is not JSON", mod: func(_ *VerdictStore, r *VerdictRecord) { r.Verdict = json.RawMessage(`{`) }, want: "encoding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			store := TownVerdicts(town, "gastown")
			rec := good
			tc.mod(store, &rec)
			err := store.Write(rec, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Write = %v, want an error naming %q", err, tc.want)
			}
			if entries, err := os.ReadDir(VerdictsDir(town)); err == nil && len(entries) > 0 {
				t.Errorf("a refused record left %v", entries)
			}
		})
	}
}

// TestVerdictStoreReportsADirectoryItCannotMake: the store's own tree is
// created privately, and a path that is not a directory is an error rather
// than a write somewhere else.
func TestVerdictStoreReportsADirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landings := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(landings, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(landings, "verdicts"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := TownVerdicts(town, "gastown")
	err := store.Write(VerdictRecord{Bead: "gt-a", Head: storeTestHead, Verdict: json.RawMessage(`{}`)}, nil)
	if err == nil {
		t.Fatal("Write = nil, want the directory failure")
	}
}

// TestVerdictStoreRetention: after each write the bead's directory keeps the
// newest verdictRetainNewest files and everything younger than
// verdictRetainAge, never the file just written and never a name the store did
// not write (gt-uxqb9).
func TestVerdictStoreRetention(t *testing.T) {
	t.Parallel()
	// The baseline is read before anything is written, so the file the store
	// writes now is newer than every seeded one.
	base := time.Now().UTC()
	const old = 40 * 24 * time.Hour
	seeds := func(n int, age func(i int) time.Duration) []seedFile {
		out := make([]seedFile, n)
		for i := range out {
			out[i] = seedFile{name: storeTestName(i), age: age(i)}
		}
		return out
	}
	minutes := func(d time.Duration) func(int) time.Duration {
		return func(i int) time.Duration { return d + time.Duration(i+1)*time.Minute }
	}
	all := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = storeTestName(i)
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		now       time.Time
		seeds     []seedFile
		unrelated []seedFile
		gone      []string
		kept      []string
		want      int // the store's files left, the one just written included
	}{
		{
			name:  "keeps the newest ten whatever their age",
			now:   base,
			seeds: seeds(13, minutes(old)),
			gone:  all(13)[9:],
			kept:  all(13)[:9],
			want:  10,
		},
		{
			name:  "keeps every file younger than thirty days",
			now:   base,
			seeds: seeds(15, func(i int) time.Duration { return time.Duration(i+1) * time.Hour }),
			kept:  all(15),
			want:  16,
		},
		{
			name: "keeps the file just written",
			now:  base.Add(60 * 24 * time.Hour),
			// Ten verdicts newer than the one this write stores, so the write
			// is outside the newest ten and old enough for the window.
			seeds: seeds(10, func(i int) time.Duration { return -time.Duration(i+1) * time.Hour }),
			kept:  all(10),
			want:  11,
		},
		{
			name:  "a file exactly at the window is not younger than it",
			now:   base,
			seeds: seeds(11, func(int) time.Duration { return verdictRetainAge }),
			want:  10,
		},
		{
			name:  "leaves a name the store did not write",
			now:   base,
			seeds: seeds(13, minutes(old)),
			unrelated: []seedFile{
				{name: "notes.txt", age: old},
				{name: "not-a-verdict.json", age: old},
			},
			gone: all(13)[9:],
			kept: all(13)[:9],
			want: 10,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			store := TownVerdicts(town, "gastown")
			store.Now = func() time.Time { return tc.now }
			dir := filepath.Join(VerdictsDir(town), "gastown", "gt-a")
			for _, sf := range append(tc.seeds, tc.unrelated...) {
				seedVerdictFile(t, dir, sf.name, tc.now.Add(-sf.age))
			}
			rec := VerdictRecord{Bead: "gt-a", Head: strings.Repeat("f", 40), Verdict: json.RawMessage(`{}`)}
			var log strings.Builder
			if err := store.Write(rec, logfTo(&log)); err != nil {
				t.Fatal(err)
			}
			if got := log.String(); got != "" {
				t.Errorf("retention logged %q, want nothing", got)
			}
			for _, name := range tc.gone {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s survived, want it pruned", name)
				}
			}
			for _, name := range append(tc.kept, headPrefix(rec.Head)+".json") {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("%s is gone, want it kept: %v", name, err)
				}
			}
			for _, sf := range tc.unrelated {
				if _, err := os.Stat(filepath.Join(dir, sf.name)); err != nil {
					t.Errorf("unrelated file %s is gone: %v", sf.name, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			stored := 0
			for _, e := range entries {
				if isVerdictFile(e.Name()) {
					stored++
				}
			}
			if stored != tc.want {
				t.Errorf("%d verdict files left, want %d", stored, tc.want)
			}
		})
	}
}

// seedFile is one file a test lays down in a bead directory: its name and how
// long before the fake clock it was last written.
type seedFile struct {
	name string
	age  time.Duration
}

// storeTestName is the file name of the i'th seeded verdict.
func storeTestName(i int) string { return fmt.Sprintf("%012x.json", i) }

// seedVerdictFile writes one file with its mtime at mod, the form the store's
// retention reads.
func seedVerdictFile(t *testing.T, dir, name string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}
