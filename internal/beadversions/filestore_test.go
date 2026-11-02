package beadversions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// storePath is a fresh JSONL file path under the test's own temporary tree.
func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "beadversions.jsonl")
}

// versionLine renders v as the one line the store appends, with its hash.
func versionLine(t *testing.T, v Version) []byte {
	t.Helper()
	v.Hash = hashText(v.Text)
	line, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(version) error = %v; want nil", err)
	}
	return append(line, '\n')
}

// TestFileStoreSurvivesReopen: versions put through one FileStore are still
// there, oldest first and byte for byte, through a new FileStore on the same
// path, including text that JSON must escape.
func TestFileStoreSurvivesReopen(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	texts := []string{"first text", "second\nline \"quoted\"\tcódigo"}

	first := NewFileStore(path)
	for i, text := range texts {
		recorded, err := first.Put("gt-1", text, "furiosa", testStart.Add(time.Duration(i)*time.Minute))
		if err != nil || !recorded {
			t.Fatalf("Put %q = %v, %v; want recorded", text, recorded, err)
		}
	}

	second := NewFileStore(path)
	versions, err := second.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions after reopen error = %v; want nil", err)
	}
	if len(versions) != len(texts) {
		t.Fatalf("Versions after reopen length = %d; want %d", len(versions), len(texts))
	}
	for i, want := range texts {
		if versions[i].Text != want {
			t.Errorf("versions[%d].Text = %q; want %q", i, versions[i].Text, want)
		}
		if versions[i].Hash != hashText(want) {
			t.Errorf("versions[%d].Hash = %q; want the hash of the text", i, versions[i].Hash)
		}
	}

	got, err := second.AsOf("gt-1", testStart.Add(30*time.Second))
	if err != nil {
		t.Fatalf("AsOf after reopen error = %v; want nil", err)
	}
	if got.Text != texts[0] {
		t.Errorf("AsOf after reopen text = %q; want %q", got.Text, texts[0])
	}
}

// TestFileStoreAppendsOneLinePerVersion: each recorded version is one line of
// the file, so the history survives a restart without a rewrite.
func TestFileStoreAppendsOneLinePerVersion(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	store := NewFileStore(path)
	for i, text := range []string{"one", "two"} {
		if _, err := store.Put("gt-1", text, "furiosa", testStart.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Put %q error = %v; want nil", text, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error = %v; want nil", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("file lines = %d; want 2", len(lines))
	}
	for i, line := range lines {
		var v Version
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("line %d is not a Version: %v", i, err)
		}
	}
}

// TestFileStoreSkipsATornLine: a torn line and an unparsable line are counted
// and left out, the good lines load, and a final line with no newline still
// counts as a line.
func TestFileStoreSkipsATornLine(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	one := versionLine(t, Version{Bead: "gt-1", Text: "one", Actor: "furiosa", At: testStart})
	two := versionLine(t, Version{Bead: "gt-1", Text: "two", Actor: "furiosa", At: testStart.Add(time.Minute)})
	three := versionLine(t, Version{Bead: "gt-2", Text: "three", Actor: "furiosa", At: testStart.Add(2 * time.Minute)})

	data := append([]byte{}, one...)
	data = append(data, "not a version\n"...)
	data = append(data, three[:len(three)/2]...) // a write cut in half
	data = append(data, '\n')
	data = append(data, two[:len(two)-1]...) // a final line with no newline
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v; want nil", err)
	}

	store := NewFileStore(path)
	skipped, err := store.Load()
	if err != nil {
		t.Fatalf("Load error = %v; want nil", err)
	}
	if skipped != 2 {
		t.Errorf("Load skipped = %d; want 2", skipped)
	}
	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 2 || versions[0].Text != "one" || versions[1].Text != "two" {
		t.Errorf("Versions = %+v; want one, two", versions)
	}
}

// TestFileStoreSkipsALineWithNoBeadOrTime: a line missing its bead or its time
// is counted and left out, since no Put could have written it.
func TestFileStoreSkipsALineWithNoBeadOrTime(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	noBead, err := json.Marshal(Version{Text: "text", Hash: hashText("text"), Actor: "furiosa", At: testStart})
	if err != nil {
		t.Fatalf("json.Marshal error = %v; want nil", err)
	}
	noTime, err := json.Marshal(Version{Bead: "gt-2", Text: "text", Hash: hashText("text"), Actor: "furiosa"})
	if err != nil {
		t.Fatalf("json.Marshal error = %v; want nil", err)
	}
	data := append(append(noBead, '\n'), append(noTime, '\n')...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v; want nil", err)
	}

	store := NewFileStore(path)
	skipped, err := store.Load()
	if err != nil || skipped != 2 {
		t.Fatalf("Load = %d, %v; want 2, nil", skipped, err)
	}
	versions, err := store.Versions("gt-2")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 0 {
		t.Errorf("Versions = %+v; want none", versions)
	}
}

// TestFileStoreSkipsALineOutOfOrder: a line whose time does not advance its
// bead's history would break AsOf's search, so it is counted and left out.
func TestFileStoreSkipsALineOutOfOrder(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	one := versionLine(t, Version{Bead: "gt-1", Text: "one", Actor: "furiosa", At: testStart})
	two := versionLine(t, Version{Bead: "gt-1", Text: "two", Actor: "furiosa", At: testStart.Add(time.Minute)})
	back := versionLine(t, Version{Bead: "gt-1", Text: "back", Actor: "furiosa", At: testStart.Add(-time.Minute)})
	data := append(append(append([]byte{}, one...), back...), two...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v; want nil", err)
	}

	store := NewFileStore(path)
	skipped, err := store.Load()
	if err != nil || skipped != 1 {
		t.Fatalf("Load = %d, %v; want 1, nil", skipped, err)
	}
	text, err := versionsText(store, "gt-1")
	if err != nil {
		t.Fatalf("versions error = %v; want nil", err)
	}
	if text != "one|two" {
		t.Errorf("versions = %q; want one|two", text)
	}
}

// TestFileStoreSkipsALineWithAForeignHash: a line whose hash is not its text's
// could answer AsOf with text it never recorded, so it is counted and left out.
func TestFileStoreSkipsALineWithAForeignHash(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	good := versionLine(t, Version{Bead: "gt-1", Text: "one", Actor: "furiosa", At: testStart})
	forged, err := json.Marshal(Version{
		Bead: "gt-1", Text: "forged", Hash: "deadbeef", Actor: "furiosa",
		At: testStart.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("json.Marshal error = %v; want nil", err)
	}
	data := append(append([]byte{}, good...), append(forged, '\n')...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v; want nil", err)
	}

	store := NewFileStore(path)
	skipped, err := store.Load()
	if err != nil || skipped != 1 {
		t.Fatalf("Load = %d, %v; want 1, nil", skipped, err)
	}
	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 1 || versions[0].Text != "one" {
		t.Errorf("Versions = %+v; want one", versions)
	}
}

// TestFileStoreMissingFileLoadsEmpty: a path with no file is an empty history,
// and merely constructing a store does not create the file.
func TestFileStoreMissingFileLoadsEmpty(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	store := NewFileStore(path)

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat before use error = %v; want ErrNotExist", err)
	}
	skipped, err := store.Load()
	if err != nil || skipped != 0 {
		t.Fatalf("Load = %d, %v; want 0, nil", skipped, err)
	}
	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 0 {
		t.Errorf("Versions = %+v; want none", versions)
	}
	if _, err := store.AsOf("gt-1", testStart); !errors.Is(err, ErrNotFound) {
		t.Errorf("AsOf error = %v; want ErrNotFound", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat after reads error = %v; want ErrNotExist", err)
	}
}

// TestFileStoreLoadRefusesAnUnreadableFile: an open, read or close failure during
// a load is returned by Load and by every read and write that needs the history,
// never treated as an empty file.
func TestFileStoreLoadRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tests := []struct {
		name string
		ops  fileOps
	}{
		{
			name: "open fails",
			ops: fileOps{
				mkdir: os.MkdirAll,
				open: func(string, int, os.FileMode) (versionFile, error) {
					return nil, boom
				},
			},
		},
		{
			name: "read fails",
			ops: fileOps{
				mkdir: os.MkdirAll,
				open: func(string, int, os.FileMode) (versionFile, error) {
					return &fakeFile{readErr: boom}, nil
				},
			},
		},
		{
			name: "close fails",
			ops: fileOps{
				mkdir: os.MkdirAll,
				open: func(string, int, os.FileMode) (versionFile, error) {
					return &fakeFile{closeErr: boom}, nil
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := NewFileStore(storePath(t))
			store.files = tt.ops

			if _, err := store.Load(); !errors.Is(err, boom) {
				t.Errorf("Load error = %v; want boom", err)
			}
			if _, err := store.Versions("gt-1"); !errors.Is(err, boom) {
				t.Errorf("Versions error = %v; want boom", err)
			}
			if _, err := store.AsOf("gt-1", testStart); !errors.Is(err, boom) {
				t.Errorf("AsOf error = %v; want boom", err)
			}
			if _, err := store.Put("gt-1", "text", "furiosa", testStart); !errors.Is(err, boom) {
				t.Errorf("Put error = %v; want boom", err)
			}
		})
	}
}

// TestFileStoreCreatesItsDirectoryAndFilePrivate: the store creates the file
// 0600 and every directory it makes 0700.
func TestFileStoreCreatesItsDirectoryAndFilePrivate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "runtime", "metrics", "beadversions.jsonl")
	store := NewFileStore(path)
	if _, err := store.Put("gt-1", "text", "furiosa", testStart); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat file error = %v; want nil", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o; want 600", got)
	}
	dir := filepath.Dir(path)
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir error = %v; want nil", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o; want 700", got)
	}
}

// failingWriteOps opens for reading normally and fails the append at the named
// step: the directory, the open, the write, the short write or the close.
func failingWriteOps(step string) fileOps {
	return fileOps{
		mkdir: func(string, os.FileMode) error {
			if step == "mkdir" {
				return errBoom
			}
			return nil
		},
		open: func(name string, flag int, perm os.FileMode) (versionFile, error) {
			if flag&os.O_WRONLY == 0 {
				return openVersionFile(name, flag, perm)
			}
			if step == "open" {
				return nil, errBoom
			}
			f := &fakeFile{}
			if step == "write" {
				f.writeErr = errBoom
			}
			if step == "short write" {
				f.writeN = 3
			}
			if step == "close" {
				f.closeErr = errBoom
			}
			return f, nil
		},
	}
}

var errBoom = errors.New("boom")

// TestFileStoreWriteFailureRecordsNothing: a Put that fails after its dedupe
// check returns the error, records nothing in memory and keeps the earlier
// versions.
func TestFileStoreWriteFailureRecordsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		at   time.Time
		ops  func() fileOps
	}{
		{name: "mkdir fails", at: testStart.Add(time.Minute), ops: func() fileOps { return failingWriteOps("mkdir") }},
		{name: "open fails", at: testStart.Add(time.Minute), ops: func() fileOps { return failingWriteOps("open") }},
		{name: "write fails", at: testStart.Add(time.Minute), ops: func() fileOps { return failingWriteOps("write") }},
		{name: "short write", at: testStart.Add(time.Minute), ops: func() fileOps { return failingWriteOps("short write") }},
		{name: "close fails", at: testStart.Add(time.Minute), ops: func() fileOps { return failingWriteOps("close") }},
		{
			name: "unencodable time",
			at:   time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
			ops:  hostFileOps,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := storePath(t)
			store := NewFileStore(path)
			if _, err := store.Put("gt-1", "first text", "furiosa", testStart); err != nil {
				t.Fatalf("setup Put error = %v; want nil", err)
			}
			store.files = tt.ops()

			recorded, err := store.Put("gt-1", "second text", "furiosa", tt.at)
			if err == nil {
				t.Fatal("Put error = nil; want the write failure")
			}
			if recorded {
				t.Error("Put recorded = true; want false")
			}
			versions, verr := store.Versions("gt-1")
			if verr != nil {
				t.Fatalf("Versions error = %v; want nil", verr)
			}
			if len(versions) != 1 || versions[0].Text != "first text" {
				t.Errorf("Versions = %+v; want only the first version", versions)
			}
		})
	}
}

// TestFileStoreRetriesALoadThatFailed: a load that failed is not cached, so a
// later call reads the file again rather than serving an empty history.
func TestFileStoreRetriesALoadThatFailed(t *testing.T) {
	t.Parallel()
	store := NewFileStore(storePath(t))
	store.files = fileOps{
		mkdir: os.MkdirAll,
		open: func(string, int, os.FileMode) (versionFile, error) {
			return nil, errBoom
		},
	}
	if _, err := store.Versions("gt-1"); !errors.Is(err, errBoom) {
		t.Fatalf("Versions error = %v; want boom", err)
	}

	store.files = hostFileOps()
	recorded, err := store.Put("gt-1", "text", "furiosa", testStart)
	if err != nil || !recorded {
		t.Fatalf("Put after the failed load = %v, %v; want recorded", recorded, err)
	}
}

// TestFileStoreRecordsAfterAFailedWriteIsRetried: the store keeps nothing from a
// failed write, so retrying the same Put records it once.
func TestFileStoreRecordsAfterAFailedWriteIsRetried(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	store := NewFileStore(path)
	host := hostFileOps()
	calls := 0
	store.files = fileOps{
		mkdir: host.mkdir,
		open: func(name string, flag int, perm os.FileMode) (versionFile, error) {
			if flag&os.O_WRONLY != 0 {
				calls++
				if calls == 1 {
					return nil, errBoom
				}
			}
			return host.open(name, flag, perm)
		},
	}

	if _, err := store.Put("gt-1", "text", "furiosa", testStart); err == nil {
		t.Fatal("first Put error = nil; want the injected failure")
	}
	recorded, err := store.Put("gt-1", "text", "furiosa", testStart)
	if err != nil || !recorded {
		t.Fatalf("second Put = %v, %v; want recorded", recorded, err)
	}

	reopened := NewFileStore(path)
	versions, err := reopened.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions after reopen error = %v; want nil", err)
	}
	if len(versions) != 1 || versions[0].Text != "text" {
		t.Errorf("Versions after reopen = %+v; want one version with text", versions)
	}
}

// shortWritingFile writes at most its first n bytes, standing in for a disk
// that accepts part of a record and then stops.
type shortWritingFile struct {
	versionFile
	n int
}

func (f *shortWritingFile) Write(p []byte) (int, error) {
	if len(p) > f.n {
		p = p[:f.n]
	}
	return f.versionFile.Write(p)
}

// shortWriteOps opens files as usual and cuts every append short.
func shortWriteOps(n int) fileOps {
	host := hostFileOps()
	return fileOps{
		mkdir: host.mkdir,
		open: func(name string, flag int, perm os.FileMode) (versionFile, error) {
			f, err := host.open(name, flag, perm)
			if err != nil {
				return nil, err
			}
			if flag&os.O_WRONLY != 0 {
				return &shortWritingFile{versionFile: f, n: n}, nil
			}
			return f, nil
		},
	}
}

// TestFileStoreEndsAPartialLineBeforeTheNextAppend: a write the disk cut short
// leaves bytes with no newline, and the next recorded Put starts on a line of
// its own, so its record reloads instead of joining the fragment.
func TestFileStoreEndsAPartialLineBeforeTheNextAppend(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	store := NewFileStore(path)
	if _, err := store.Put("gt-1", "first", "furiosa", testStart); err != nil {
		t.Fatalf("first Put error = %v; want nil", err)
	}

	store.files = shortWriteOps(5)
	recorded, err := store.Put("gt-1", "second", "furiosa", testStart.Add(time.Minute))
	if err == nil || recorded {
		t.Fatalf("Put with a short write = %v, %v; want the short-write error", recorded, err)
	}

	store.files = hostFileOps()
	recorded, err = store.Put("gt-1", "third", "furiosa", testStart.Add(2*time.Minute))
	if err != nil || !recorded {
		t.Fatalf("Put after the short write = %v, %v; want recorded", recorded, err)
	}

	reopened := NewFileStore(path)
	skipped, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load error = %v; want nil", err)
	}
	if skipped != 1 {
		t.Errorf("Load skipped = %d; want 1 (the fragment)", skipped)
	}
	if text, err := versionsText(reopened, "gt-1"); err != nil || text != "first|third" {
		t.Errorf("versions after reopen = %q, %v; want first|third", text, err)
	}
}

// TestFileStoreEndsATornTailBeforeTheNextAppend: a torn line an earlier crash
// left at the file's end is closed off before the next recorded Put, so that
// Put is not lost on the reopen.
func TestFileStoreEndsATornTailBeforeTheNextAppend(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	first := versionLine(t, Version{Bead: "gt-1", Text: "first", Actor: "furiosa", At: testStart})
	data := append(append([]byte{}, first...), `{"bead":"gt-1","text":"sec`...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v; want nil", err)
	}

	store := NewFileStore(path)
	recorded, err := store.Put("gt-1", "second", "furiosa", testStart.Add(time.Minute))
	if err != nil || !recorded {
		t.Fatalf("Put after a torn tail = %v, %v; want recorded", recorded, err)
	}

	reopened := NewFileStore(path)
	skipped, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load error = %v; want nil", err)
	}
	if skipped != 1 {
		t.Errorf("Load skipped = %d; want 1 (the torn tail)", skipped)
	}
	if text, err := versionsText(reopened, "gt-1"); err != nil || text != "first|second" {
		t.Errorf("versions after reopen = %q, %v; want first|second", text, err)
	}
}

// TestFileStorePersistsTaggedFieldNames: a stored line names its fields as the
// schema documents, so a Go field rename cannot change the file's format.
func TestFileStorePersistsTaggedFieldNames(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	store := NewFileStore(path)
	if _, err := store.Put("gt-1", "one", "furiosa", testStart); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error = %v; want nil", err)
	}
	line := strings.TrimSuffix(string(data), "\n")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("the stored line is not a JSON object: %v", err)
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "actor,at,bead,hash,text" {
		t.Errorf("stored field names = %s; want actor,at,bead,hash,text", got)
	}
}

// TestFileStoreKeepsTheSkippedCountOfAnImplicitLoad: a load a read triggers
// keeps its skipped count, so a caller can report a damaged file without
// loading it explicitly.
func TestFileStoreKeepsTheSkippedCountOfAnImplicitLoad(t *testing.T) {
	t.Parallel()
	path := storePath(t)
	good := versionLine(t, Version{Bead: "gt-1", Text: "one", Actor: "furiosa", At: testStart})
	data := append(append([]byte{}, good...), "not a version\n"...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v; want nil", err)
	}

	store := NewFileStore(path)
	if got := store.Skipped(); got != 0 {
		t.Errorf("Skipped before a load = %d; want 0", got)
	}
	if _, err := store.Versions("gt-1"); err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if got := store.Skipped(); got != 1 {
		t.Errorf("Skipped after an implicit load = %d; want 1", got)
	}
}

// TestStoresAgree: one table of puts and reads gives the same answer from the
// in-memory store and the file store.
func TestStoresAgree(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		new  func(t *testing.T) Store
	}{
		{name: "memory", new: func(*testing.T) Store { return NewMemoryStore() }},
		{name: "file", new: func(t *testing.T) Store { return NewFileStore(storePath(t)) }},
	}
	steps := []struct {
		name    string
		run     func(Store) (string, error)
		want    string
		wantErr error
	}{
		{
			name: "put first text",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("gt-1", "one", "furiosa", testStart)
				return fmt.Sprint(recorded), err
			},
			want: "true",
		},
		{
			name: "put identical text later",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("gt-1", "one", "furiosa", testStart.Add(time.Minute))
				return fmt.Sprint(recorded), err
			},
			want: "false",
		},
		{
			name: "put changed text",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("gt-1", "two", "furiosa", testStart.Add(2*time.Minute))
				return fmt.Sprint(recorded), err
			},
			want: "true",
		},
		{
			name: "versions",
			run: func(s Store) (string, error) {
				return versionsText(s, "gt-1")
			},
			want: "one|two",
		},
		{
			name: "as of between",
			run: func(s Store) (string, error) {
				return asOfText(s, "gt-1", testStart.Add(90*time.Second))
			},
			want: "one",
		},
		{
			name: "as of after last",
			run: func(s Store) (string, error) {
				return asOfText(s, "gt-1", testStart.Add(time.Hour))
			},
			want: "two",
		},
		{
			name: "as of before first",
			run: func(s Store) (string, error) {
				return asOfText(s, "gt-1", testStart.Add(-time.Second))
			},
			wantErr: ErrNotFound,
		},
		{
			name: "as of unknown bead",
			run: func(s Store) (string, error) {
				return asOfText(s, "gt-none", testStart)
			},
			wantErr: ErrNotFound,
		},
		{
			name: "put out of order",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("gt-1", "three", "furiosa", testStart.Add(time.Minute))
				return fmt.Sprint(recorded), err
			},
			want:    "false",
			wantErr: ErrOutOfOrder,
		},
		{
			name: "put empty bead",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("", "text", "furiosa", testStart)
				return fmt.Sprint(recorded), err
			},
			want:    "false",
			wantErr: ErrEmptyBead,
		},
		{
			name: "put zero time",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("gt-1", "text", "furiosa", time.Time{})
				return fmt.Sprint(recorded), err
			},
			want:    "false",
			wantErr: ErrZeroTime,
		},
		{
			name: "put second bead",
			run: func(s Store) (string, error) {
				recorded, err := s.Put("gt-2", "other", "furiosa", testStart)
				return fmt.Sprint(recorded), err
			},
			want: "true",
		},
		{
			name: "versions of second bead",
			run: func(s Store) (string, error) {
				return versionsText(s, "gt-2")
			},
			want: "other",
		},
		{
			name: "versions of unknown bead",
			run: func(s Store) (string, error) {
				return versionsText(s, "gt-none")
			},
		},
		{
			name: "versions of empty bead",
			run: func(s Store) (string, error) {
				return versionsText(s, "")
			},
			wantErr: ErrEmptyBead,
		},
		{
			name: "as of empty bead",
			run: func(s Store) (string, error) {
				return asOfText(s, "", testStart)
			},
			wantErr: ErrEmptyBead,
		},
	}
	for _, store := range stores {
		t.Run(store.name, func(t *testing.T) {
			s := store.new(t)
			for _, step := range steps {
				got, err := step.run(s)
				if !errors.Is(err, step.wantErr) {
					t.Fatalf("%s: error = %v; want %v", step.name, err, step.wantErr)
				}
				if got != step.want {
					t.Errorf("%s = %q; want %q", step.name, got, step.want)
				}
			}
		})
	}
}

// versionsText joins a bead's version texts, oldest first, for comparison.
func versionsText(s Store, bead string) (string, error) {
	versions, err := s.Versions(bead)
	if err != nil {
		return "", err
	}
	texts := make([]string, len(versions))
	for i, v := range versions {
		texts[i] = v.Text
	}
	return strings.Join(texts, "|"), nil
}

// asOfText returns the text in force at t.
func asOfText(s Store, bead string, t time.Time) (string, error) {
	v, err := s.AsOf(bead, t)
	if err != nil {
		return "", err
	}
	return v.Text, nil
}

// fakeFile stands in for an open file and fails the step a test names.
type fakeFile struct {
	readErr  error
	writeN   int // bytes Write reports; 0 writes the whole slice
	writeErr error
	closeErr error
}

func (f *fakeFile) Read([]byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return 0, io.EOF
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.writeN > 0 {
		return f.writeN, nil
	}
	return len(p), nil
}

func (f *fakeFile) Close() error {
	return f.closeErr
}

var _ versionFile = (*fakeFile)(nil)
