package polecatimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// fakeListFS records the writer's calls and answers the failure a case names. Names are the ones the
// writer makes, relative to the claim root.
type fakeListFS struct {
	mkdirErr  error
	removeErr error
	writeErr  error
	renameErr error
	mkdirs    []string
	removes   []string
	writes    map[string]string
	renames   [][2]string
}

func newFakeListFS() *fakeListFS {
	return &fakeListFS{writes: map[string]string{}}
}

func (f *fakeListFS) MkdirAll(name string, _ os.FileMode) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.mkdirs = append(f.mkdirs, name)
	return nil
}

func (f *fakeListFS) Remove(name string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removes = append(f.removes, name)
	return nil
}

func (f *fakeListFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes[name] = string(data)
	return nil
}

func (f *fakeListFS) Rename(oldpath, newpath string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	f.renames = append(f.renames, [2]string{oldpath, newpath})
	return nil
}

// TestWriteTrimListWritesTheEncodedListIntoTheClaim pins the file the trim container reads: the list
// lands at the path the builder names, as the JSON array the reader decodes, and it arrives by a
// rename so a Job never reads a list that is half written.
func TestWriteTrimListWritesTheEncodedListIntoTheClaim(t *testing.T) {
	t.Parallel()
	f := newFakeListFS()
	paths := []string{"gocache/ab/cdef", "gomodcache/mod/x"}
	if err := WriteTrimList(paths, f); err != nil {
		t.Fatalf("WriteTrimList: %v", err)
	}

	dest := cachevol.TrimClaimListPath
	tmp := dest + ".tmp"
	if want := []string{cachevol.TrimClaimListDir}; !reflect.DeepEqual(f.mkdirs, want) {
		t.Errorf("mkdirs = %v, want %v", f.mkdirs, want)
	}
	if want := []string{tmp}; !reflect.DeepEqual(f.removes, want) {
		t.Errorf("removes = %v, want the stale temporary cleared: %v", f.removes, want)
	}
	data, ok := f.writes[tmp]
	if !ok || len(f.writes) != 1 {
		t.Fatalf("writes = %v, want the list beside its destination as %s", f.writes, tmp)
	}
	var got []string
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("the written list is not a JSON array of paths: %v", err)
	}
	if !reflect.DeepEqual(got, paths) {
		t.Errorf("the written list is %v, want %v", got, paths)
	}
	if want := [][2]string{{tmp, dest}}; !reflect.DeepEqual(f.renames, want) {
		t.Errorf("renames = %v, want %v: the list moves into place whole", f.renames, want)
	}
}

// TestWriteTrimListRefusesAListTheContainerWouldRefuse pins the guard that keeps the writer and the
// reader on one contract: a list with nothing to remove or a path the claim walk could not give is
// refused whole, and nothing is written.
func TestWriteTrimListRefusesAListTheContainerWouldRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths []string
	}{
		{"no path", nil},
		{"the claim root itself", []string{"."}},
		{"an absolute path", []string{"/etc/passwd"}},
		{"a parent element", []string{"gocache/../../escape"}},
		{"an unclean path", []string{"gocache//one"}},
		{"one bad path beside good ones", []string{"gocache/fine", "../escape"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeListFS()
			if err := WriteTrimList(tc.paths, f); err == nil {
				t.Fatalf("WriteTrimList(%v) wrote a list the container would refuse", tc.paths)
			}
			if len(f.mkdirs) != 0 || len(f.removes) != 0 || len(f.writes) != 0 || len(f.renames) != 0 {
				t.Errorf("the writer touched the claim for a refused list: mkdirs %v, removes %v, writes %v, renames %v", f.mkdirs, f.removes, f.writes, f.renames)
			}
		})
	}
}

// TestWriteTrimListReportsEachFailedStep pins the fail-closed errors: a directory it cannot create,
// a stale temporary it cannot clear, a file it cannot write, or a rename it cannot make leaves no
// list in place, and the error names the step.
func TestWriteTrimListReportsEachFailedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		breakFS   func(*fakeListFS)
		wantErr   string
		wantWrote bool
		wantMoved bool
	}{
		{"the directory cannot be created", func(f *fakeListFS) { f.mkdirErr = errors.New("read-only file system") }, "creating the trim list directory", false, false},
		{"the stale temporary cannot be cleared", func(f *fakeListFS) { f.removeErr = errors.New("permission denied") }, "clearing the stale trim list", false, false},
		{"the list cannot be written", func(f *fakeListFS) { f.writeErr = errors.New("no space left on device") }, "writing the trim list", false, false},
		{"the list cannot be moved into place", func(f *fakeListFS) { f.renameErr = errors.New("permission denied") }, "moving the trim list into place", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeListFS()
			tc.breakFS(f)
			err := WriteTrimList([]string{"gocache/one"}, f)
			if err == nil {
				t.Fatal("WriteTrimList reported success for a list it did not write")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
			if _, ok := f.writes[cachevol.TrimClaimListPath]; ok {
				t.Errorf("the list was written at its destination despite the failure")
			}
			if got := len(f.writes) > 0; got != tc.wantWrote {
				t.Errorf("wrote the temporary list = %v, want %v", got, tc.wantWrote)
			}
			if got := len(f.renames) > 0; got != tc.wantMoved {
				t.Errorf("moved the list into place = %v, want %v", got, tc.wantMoved)
			}
		})
	}
}

// TestTrimListWriteRefusesAClaimWhoseDirectoryLeavesIt runs the writer's production call on a real
// claim whose list directory is a symlink to a directory outside the claim. The write must be refused
// and nothing may land outside the claim (the shared claim is writable by the pod that runs the
// writer, so a planted link is the case to hold).
func TestTrimListWriteRefusesAClaimWhoseDirectoryLeavesIt(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	claim := filepath.Join(base, "claim")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{claim, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(claim, cachevol.TrimClaimListDir)); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := trimListWriteMain(strings.NewReader(`["gocache/ab/one"]`), []string{"--root", claim}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("trim-list-write exit = %d, want 3 for a claim whose list directory leaves it (stderr %q)", code, stderr.String())
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the writer put %d entries outside the claim, want none", len(entries))
	}
}

// TestTrimListWriteDoesNotWriteThroughAStaleTemporary runs the writer's production call on a claim
// that holds a symlink at the temporary's name, pointing at a file outside the claim. The writer must
// replace the link rather than write through it, so the file it points at keeps its contents and the
// list lands at its destination.
func TestTrimListWriteDoesNotWriteThroughAStaleTemporary(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	claim := filepath.Join(base, "claim")
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(filepath.Join(claim, cachevol.TrimClaimListDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(claim, filepath.FromSlash(cachevol.TrimClaimListPath)+".tmp")
	if err := os.Symlink(victim, tmp); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := trimListWriteMain(strings.NewReader(`["gocache/ab/one"]`), []string{"--root", claim}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("trim-list-write exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Errorf("the file outside the claim is %q, want it untouched", got)
	}
	list, err := os.ReadFile(filepath.Join(claim, filepath.FromSlash(cachevol.TrimClaimListPath)))
	if err != nil {
		t.Fatalf("the list is not at its destination: %v", err)
	}
	if string(list) != `["gocache/ab/one"]` {
		t.Errorf("the list at its destination is %q, want the encoded list", list)
	}
}
