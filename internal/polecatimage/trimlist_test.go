package polecatimage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// fakeListFS records the writer's calls and answers the failure a case names.
type fakeListFS struct {
	mkdirErr  error
	writeErr  error
	renameErr error
	mkdirs    []string
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
	if err := WriteTrimList("/cache", paths, f); err != nil {
		t.Fatalf("WriteTrimList: %v", err)
	}

	dir := filepath.Join("/cache", cachevol.TrimClaimListDir)
	dest := filepath.Join("/cache", filepath.FromSlash(cachevol.TrimClaimListPath))
	if want := []string{dir}; !reflect.DeepEqual(f.mkdirs, want) {
		t.Errorf("mkdirs = %v, want %v", f.mkdirs, want)
	}
	tmp := dest + ".tmp"
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
			if err := WriteTrimList("/cache", tc.paths, f); err == nil {
				t.Fatalf("WriteTrimList(%v) wrote a list the container would refuse", tc.paths)
			}
			if len(f.mkdirs) != 0 || len(f.writes) != 0 || len(f.renames) != 0 {
				t.Errorf("the writer touched the claim for a refused list: mkdirs %v, writes %v, renames %v", f.mkdirs, f.writes, f.renames)
			}
		})
	}
}

// TestWriteTrimListReportsEachFailedStep pins the fail-closed errors: a directory it cannot create,
// a file it cannot write, or a rename it cannot make leaves no list in place, and the error names
// the step.
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
		{"the list cannot be written", func(f *fakeListFS) { f.writeErr = errors.New("no space left on device") }, "writing the trim list", false, false},
		{"the list cannot be moved into place", func(f *fakeListFS) { f.renameErr = errors.New("permission denied") }, "moving the trim list into place", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeListFS()
			tc.breakFS(f)
			err := WriteTrimList("/cache", []string{"gocache/one"}, f)
			if err == nil {
				t.Fatal("WriteTrimList reported success for a list it did not write")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
			dest := filepath.Join("/cache", filepath.FromSlash(cachevol.TrimClaimListPath))
			if _, ok := f.writes[dest]; ok {
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
