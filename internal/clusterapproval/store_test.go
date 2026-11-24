package clusterapproval

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	modes                        map[string]fs.FileMode
	data                         map[string][]byte
	ids                          map[string][2]int
	errPath                      string
	openDirErr, openErr, readErr error
	lstatErr, dirStatErr         error
	dirs, files, reads           int
}

var _ deployercred.ReadFS = (*fakeReadFS)(nil)

func newFakeReadFS() *fakeReadFS {
	return &fakeReadFS{modes: map[string]fs.FileMode{}, data: map[string][]byte{}, ids: map[string][2]int{}}
}

func (f *fakeReadFS) putDir(name string, mode fs.FileMode) {
	f.modes[filepath.Clean(name)] = fs.ModeDir | mode
}

func (f *fakeReadFS) putFile(name string, mode fs.FileMode, data string) {
	name = filepath.Clean(name)
	f.modes[name] = mode
	f.data[name] = []byte(data)
}

// putLink makes name a symlink, which every reader must refuse.
func (f *fakeReadFS) putLink(name string) {
	f.modes[filepath.Clean(name)] = fs.ModeSymlink | 0o777
}

// setIDs overrides the owner and group name reports, so a test can name a path
// held by another uid or gid, or one whose owner or group the platform does not
// report (-1).
func (f *fakeReadFS) setIDs(name string, owner, group int) {
	f.ids[filepath.Clean(name)] = [2]int{owner, group}
}

func (f *fakeReadFS) Lstat(name string) (deployercred.Info, error) {
	name = filepath.Clean(name)
	if name == f.errPath && f.lstatErr != nil {
		return deployercred.Info{}, f.lstatErr
	}
	mode, ok := f.modes[name]
	if !ok {
		return deployercred.Info{}, fs.ErrNotExist
	}
	info := deployercred.Info{Mode: mode, Owner: brokerUID, Group: deployerGID}
	if id, ok := f.ids[name]; ok {
		info.Owner, info.Group = id[0], id[1]
	}
	return info, nil
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

func (d fakeDir) Stat() (deployercred.Info, error) {
	if d.path == d.fs.errPath && d.fs.dirStatErr != nil {
		return deployercred.Info{}, d.fs.dirStatErr
	}
	return d.fs.Lstat(d.path)
}

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

func (f *fakeFile) Read(p []byte) (int, error) {
	f.fs.reads++
	if f.name == f.fs.errPath && f.fs.readErr != nil {
		return 0, f.fs.readErr
	}
	return f.r.Read(p)
}

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
			f, err := OpenRecord(fsys, brokerUID, deployerGID, recordDir, goodID)
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
			got, err := OpenRecord(fsys, brokerUID, deployerGID, recordDir, tt.id)
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

func TestClusterApprovalStoreReadsRecord(t *testing.T) {
	t.Parallel()
	approved := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	want := Record{Act: ActApplyPlan, Cluster: ClusterControl, PlanSHA256: goodSHA, ProposalID: goodID, ApprovedAt: approved, ExpiresAt: approved.Add(goodWindow)}
	tests := []struct {
		name   string
		settle func(*fakeReadFS)
		wantNo bool
		want   string
	}{
		{"a good record", func(f *fakeReadFS) {
			f.putDir(recordDir, 0o750)
			f.putFile(recordPath(), 0o640, recordText(goodRecord()))
		}, false, ""},
		{"a missing record", func(f *fakeReadFS) { f.putDir(recordDir, 0o750) }, true, ""},
		{"a record that cannot be read", func(f *fakeReadFS) {
			f.putDir(recordDir, 0o750)
			f.putFile(recordPath(), 0o640, recordText(goodRecord()))
			f.errPath, f.readErr = recordPath(), fs.ErrPermission
		}, false, "could not read the approval record"},
		{"a record larger than 4096 bytes", func(f *fakeReadFS) {
			f.putDir(recordDir, 0o750)
			f.putFile(recordPath(), 0o640, recordText(goodRecord())+strings.Repeat(" ", maxRecordBytes))
		}, false, "larger than 4096 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeReadFS()
			tt.settle(fsys)
			got, err := ReadRecord(fsys, brokerUID, deployerGID, recordDir, goodID)
			switch {
			case tt.wantNo:
				if !errors.Is(err, ErrNoApproval) || got != (Record{}) {
					t.Fatalf("ReadRecord on a record that is not there returned %+v, %v, want ErrNoApproval and no record", got, err)
				}
			case tt.want != "":
				if err == nil || !strings.Contains(err.Error(), tt.want) || errors.Is(err, ErrNoApproval) {
					t.Fatalf("ReadRecord returned %v, want a refusal stating %q that is not ErrNoApproval", err, tt.want)
				}
			default:
				if err != nil || got != want {
					t.Fatalf("ReadRecord read %+v, %v, want %+v", got, err, want)
				}
			}
		})
	}
}

// TestClusterApprovalStoreRefuses drives every refusal through ReadRecord, the
// production call site. Each row changes one thing about a good directory and
// record: it is refused before any byte is read, and the refusal names the path
// and the failing check, never the record.
func TestClusterApprovalStoreRefuses(t *testing.T) {
	t.Parallel()
	text := recordText(goodRecord())
	tests := []struct {
		name   string
		settle func(*fakeReadFS)
		path   string
		want   string
	}{
		{"a symlink at the directory", func(f *fakeReadFS) { f.putLink(recordDir) }, recordDir, "is a symlink"},
		{"a symlink on an ancestor", func(f *fakeReadFS) { f.putLink("/srv") }, "/srv", "is a symlink"},
		{"a 0755 directory", func(f *fakeReadFS) { f.putDir(recordDir, 0o755) }, recordDir, "has mode drwxr-xr-x, not drwxr-x---"},
		{"a 0700 directory", func(f *fakeReadFS) { f.putDir(recordDir, 0o700) }, recordDir, "has mode drwx------, not drwxr-x---"},
		{"a directory owned by another uid", func(f *fakeReadFS) { f.setIDs(recordDir, brokerUID+1, deployerGID) }, recordDir, "is owned by uid"},
		{"a directory in another group", func(f *fakeReadFS) { f.setIDs(recordDir, brokerUID, deployerGID+1) }, recordDir, "is in group"},
		{"a directory whose owner is not reported", func(f *fakeReadFS) { f.setIDs(recordDir, -1, deployerGID) }, recordDir, "owner of the approval directory"},
		{"a directory whose group is not reported", func(f *fakeReadFS) { f.setIDs(recordDir, brokerUID, -1) }, recordDir, "group of the approval directory"},
		{"a record owned by another uid", func(f *fakeReadFS) { f.setIDs(recordPath(), brokerUID+1, deployerGID) }, recordPath(), "is owned by uid"},
		{"a record in another group", func(f *fakeReadFS) { f.setIDs(recordPath(), brokerUID, deployerGID+1) }, recordPath(), "is in group"},
		{"a record whose owner is not reported", func(f *fakeReadFS) { f.setIDs(recordPath(), -1, deployerGID) }, recordPath(), "owner of the approval record"},
		{"a record whose group is not reported", func(f *fakeReadFS) { f.setIDs(recordPath(), brokerUID, -1) }, recordPath(), "group of the approval record"},
		{"an ancestor that cannot be read", func(f *fakeReadFS) { f.errPath, f.lstatErr = "/srv", fs.ErrPermission }, "/srv", "could not read /srv"},
		{"a directory handle that cannot be read", func(f *fakeReadFS) { f.errPath, f.dirStatErr = recordDir, fs.ErrPermission }, recordDir, "could not read the approval directory"},
		{"a 0644 record", func(f *fakeReadFS) { f.putFile(recordPath(), 0o644, text) }, recordPath(), "has mode -rw-r--r--, not -rw-r-----"},
		{"a 0600 record", func(f *fakeReadFS) { f.putFile(recordPath(), 0o600, text) }, recordPath(), "has mode -rw-------, not -rw-r-----"},
		{"a record that is a symlink", func(f *fakeReadFS) { f.putLink(recordPath()) }, recordPath(), "has mode Lrwxrwxrwx, not -rw-r-----"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeReadFS()
			fsys.putDir(recordDir, 0o750)
			fsys.putFile(recordPath(), 0o640, text)
			tt.settle(fsys)
			got, err := ReadRecord(fsys, brokerUID, deployerGID, recordDir, goodID)
			if err == nil {
				t.Fatalf("ReadRecord accepted the record and returned %+v", got)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.want) || !strings.Contains(msg, tt.path) {
				t.Fatalf("the refusal %q does not name the path %s and the check %q", msg, tt.path, tt.want)
			}
			if errors.Is(err, ErrNoApproval) {
				t.Fatalf("the refusal %q reads as a missing record", msg)
			}
			if got != (Record{}) {
				t.Fatalf("a refused read returned a record: %+v", got)
			}
			if fsys.reads != 0 {
				t.Fatalf("a refused record was read %d times, want no read", fsys.reads)
			}
			if strings.Contains(msg, goodSHA) {
				t.Fatalf("the refusal %q carries record content", msg)
			}
		})
	}
}

// A record that does not parse is reported without ParseRecord's error, which
// can carry a key or a value from the record.
func TestClusterApprovalStoreErrorsNameNoContent(t *testing.T) {
	t.Parallel()
	const secretKey, secretValue = "secret_token", "s3cr3t-value-that-must-not-leak"
	fields := goodRecord()
	fields[secretKey] = secretValue
	fsys := newFakeReadFS()
	fsys.putDir(recordDir, 0o750)
	fsys.putFile(recordPath(), 0o640, recordText(fields))
	_, err := ReadRecord(fsys, brokerUID, deployerGID, recordDir, goodID)
	if err == nil {
		t.Fatal("ReadRecord accepted a record with an unknown key")
	}
	msg := err.Error()
	if !strings.Contains(msg, recordPath()) || !strings.Contains(msg, "not a valid approval record") {
		t.Fatalf("the refusal %q does not name the path %s and say the record is not valid", msg, recordPath())
	}
	if strings.Contains(msg, secretKey) || strings.Contains(msg, secretValue) {
		t.Fatalf("the refusal %q carries the record's key or value", msg)
	}
}

// swapAfterOpenDir is the real disk, except the first OpenDir runs swap once the directory is open.
type swapAfterOpenDir struct {
	deployercred.Disk
	swap func() error
	ran  bool
}

func (s *swapAfterOpenDir) OpenDir(name string) (deployercred.Dir, error) {
	dir, err := s.Disk.OpenDir(name)
	if err != nil || s.ran {
		return dir, err
	}
	s.ran = true
	if err := s.swap(); err != nil {
		_ = dir.Close() // the swap failed before any read: a close error changes nothing
		return nil, err
	}
	return dir, nil
}

// writeApprovalTree writes <tree>/approvals/<goodID>.json, modes 0750 and 0640,
// owned by the test's uid and gid, and returns the approvals directory.
func writeApprovalTree(t *testing.T, base, tree string, fields map[string]any) string {
	dir := filepath.Join(base, tree, "approvals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the approvals directory: the reader requires 0750, whose owner-execute bit is the directory search bit
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, goodID+".json")
	if err := os.WriteFile(file, []byte(recordText(fields)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestClusterApprovalStoreSwappedAncestor proves ReadRecord reads the record it
// checked when the opened path is swapped for a symlink right after OpenDir. Rows
// swap the approval directory, or the directory above it, for a decoy tree holding
// a second valid record. Mutation proof: a ReadRecord that opened the directory by
// its path again after OpenDir reads the decoy record and fails got != want.
func TestClusterApprovalStoreSwappedAncestor(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, swapped, target string }{
		{"the approval directory", filepath.Join("real", "approvals"), filepath.Join("decoy", "approvals")},
		{"the directory above the approval directory", "real", "decoy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// macOS keeps the temp dir under /var, a symlink that refuseSymlinkChain refuses.
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := writeApprovalTree(t, base, "real", goodRecord())
			writeApprovalTree(t, base, "decoy", with(goodRecord(), map[string]any{"plan_sha256": strings.Repeat("b2", 32)}))

			swapped := filepath.Join(base, tt.swapped)
			fsys := &swapAfterOpenDir{swap: func() error {
				if err := os.Rename(swapped, swapped+".moved"); err != nil {
					return err
				}
				return os.Symlink(filepath.Join(base, tt.target), swapped)
			}}
			got, err := ReadRecord(fsys, os.Getuid(), os.Getgid(), dir, goodID)
			approved := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
			want := Record{Act: ActApplyPlan, Cluster: ClusterControl, PlanSHA256: goodSHA, ProposalID: goodID, ApprovedAt: approved, ExpiresAt: approved.Add(goodWindow)}
			if !fsys.ran || err != nil || got != want {
				t.Fatalf("swap ran %v: ReadRecord read %+v, %v after the swap, want the record it checked: %+v", fsys.ran, got, err, want)
			}
		})
	}
}
