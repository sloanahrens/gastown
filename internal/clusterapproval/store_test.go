package clusterapproval

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deployercred"
)

const (
	// brokerUID and deployerGID are the ids the runner passes: the broker's
	// user, which writes the record, and the deployer's group, which reads it.
	brokerUID   = 1001
	deployerGID = 2002
	// recordDir is the directory the broker writes records into.
	recordDir = "/srv/gt/approvals"
)

// recordPath is the record a good call reads.
func recordPath() string { return filepath.Join(recordDir, goodID+".json") }

// fakeReadFS is the in-memory deployercred.ReadFS the tests read through: it
// reports every path it was given as the broker's, in that path's mode, and
// fails the one call whose path is errPath. dirs and files show that an id
// ReadRecord refuses reached the filesystem not at all.
type fakeReadFS struct {
	modes               map[string]fs.FileMode
	data                map[string][]byte
	errPath             string
	openDirErr, openErr error
	dirs, files         int
}

var _ deployercred.ReadFS = (*fakeReadFS)(nil)

func newFakeReadFS() *fakeReadFS {
	return &fakeReadFS{modes: map[string]fs.FileMode{}, data: map[string][]byte{}}
}

func (f *fakeReadFS) putDir(name string, mode fs.FileMode) {
	f.modes[filepath.Clean(name)] = fs.ModeDir | mode
}

func (f *fakeReadFS) putFile(name string, mode fs.FileMode, data string) {
	name = filepath.Clean(name)
	f.modes[name] = mode
	f.data[name] = []byte(data)
}

func (f *fakeReadFS) Lstat(name string) (deployercred.Info, error) {
	mode, ok := f.modes[filepath.Clean(name)]
	if !ok {
		return deployercred.Info{}, fs.ErrNotExist
	}
	return deployercred.Info{Mode: mode, Owner: brokerUID, Group: deployerGID}, nil
}

func (f *fakeReadFS) OpenDir(name string) (deployercred.Dir, error) {
	f.dirs++
	name = filepath.Clean(name)
	if name == f.errPath && f.openDirErr != nil {
		return nil, f.openDirErr
	}
	if _, ok := f.modes[name]; !ok {
		return nil, fs.ErrNotExist
	}
	return fakeDir{f, name}, nil
}

type fakeDir struct {
	fs   *fakeReadFS
	path string
}

func (d fakeDir) Stat() (deployercred.Info, error) { return d.fs.Lstat(d.path) }

func (d fakeDir) Close() error { return nil }

func (d fakeDir) Open(name string) (deployercred.ReadFile, error) {
	d.fs.files++
	full := filepath.Join(d.path, name)
	if full == d.fs.errPath && d.fs.openErr != nil {
		return nil, d.fs.openErr
	}
	if _, ok := d.fs.modes[full]; !ok {
		return nil, fs.ErrNotExist
	}
	return &fakeFile{fs: d.fs, name: full, r: bytes.NewReader(d.fs.data[full])}, nil
}

type fakeFile struct {
	fs   *fakeReadFS
	name string
	r    *bytes.Reader
}

func (f *fakeFile) Stat() (deployercred.Info, error) { return f.fs.Lstat(f.name) }

func (f *fakeFile) Close() error { return nil }

func (f *fakeFile) Read(p []byte) (int, error) { return f.r.Read(p) }

func TestClusterApprovalStoreReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		settle func(*fakeReadFS)
		wantNo bool
		want   string
	}{
		{"a 0750 directory held by the broker's uid and group, with a 0640 record", func(f *fakeReadFS) {
			f.putDir(recordDir, 0o750)
			f.putFile(recordPath(), 0o640, recordText(goodRecord()))
		}, false, ""},
		{"a missing record", func(f *fakeReadFS) { f.putDir(recordDir, 0o750) }, true, ""},
		{"a missing directory", func(*fakeReadFS) {}, true, ""},
		{"a directory that cannot be opened", func(f *fakeReadFS) {
			f.putDir(recordDir, 0o750)
			f.errPath, f.openDirErr = recordDir, fs.ErrPermission
		}, false, "could not open the approval directory"},
		{"a record that cannot be opened", func(f *fakeReadFS) {
			f.putDir(recordDir, 0o750)
			f.errPath, f.openErr = recordPath(), fs.ErrPermission
		}, false, "could not open the approval record"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeReadFS()
			tt.settle(fsys)
			f, err := OpenRecord(fsys, recordDir, goodID)
			switch {
			case tt.wantNo:
				if !errors.Is(err, ErrNoApproval) {
					t.Fatalf("OpenRecord on a record that is not there returned %v, want ErrNoApproval", err)
				}
			case tt.want != "":
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("OpenRecord returned %v, want a refusal stating %q", err, tt.want)
				}
				if errors.Is(err, ErrNoApproval) {
					t.Fatalf("the refusal %v reads as a missing record, and a refused open is not one", err)
				}
			default:
				if err != nil {
					t.Fatalf("OpenRecord refused a good record: %v", err)
				}
				data, err := io.ReadAll(f)
				if err != nil || string(data) != recordText(goodRecord()) {
					t.Fatalf("the opened record read back %q, %v, want %q", data, err, recordText(goodRecord()))
				}
				if fsys.dirs != 1 || fsys.files != 1 {
					t.Fatalf("OpenRecord made %d OpenDir calls and %d Open calls, want the directory and the record opened once", fsys.dirs, fsys.files)
				}
			}
		})
	}
}

func TestClusterApprovalStoreID(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, id string }{
		{"an empty id", ""},
		{"eleven characters", goodID[:11]},
		{"thirteen characters", goodID + "0"},
		{"upper-case", strings.ToUpper(goodID)},
		{"a non-hex character", "0123456789ag"},
		{"a path separator", "01234567/9ab"},
		{"a parent traversal", "../../../etc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeReadFS()
			fsys.putDir(recordDir, 0o750)
			fsys.putFile(recordPath(), 0o640, recordText(goodRecord()))
			got, err := OpenRecord(fsys, recordDir, tt.id)
			if err == nil {
				t.Fatalf("OpenRecord accepted the id %q and returned %v", tt.id, got)
			}
			if !strings.Contains(err.Error(), "not twelve lower-case hex characters") {
				t.Fatalf("the refusal %q does not state the id rule", err)
			}
			if fsys.dirs != 0 || fsys.files != 0 {
				t.Fatalf("a refused id reached the filesystem: %d OpenDir calls and %d Open calls, want none", fsys.dirs, fsys.files)
			}
		})
	}
}
