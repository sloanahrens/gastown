package deployercred_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/deployercred"
)

const (
	storeUID = 1000
	storeDir = "/home/example/.gt/deployer"
)

// storeEntry is one path of the fake filesystem: its mode (ModeDir set for a
// directory), the uid that owns it, and its bytes.
type storeEntry struct {
	mode  fs.FileMode
	owner int
	data  []byte
}

type storeRename struct{ oldpath, newpath string }

// fakeStoreFS is the in-memory filesystem Store is tested through. A test
// injects a failure by operation or by path, and Store's outcome is read back
// as the paths that exist.
type fakeStoreFS struct {
	entries map[string]storeEntry
	renames []storeRename
	// created lists the paths an exclusive create made, so a test can show a
	// failed write left none of them behind.
	created []string
	// fail keys an error by "<op>:<path>"; failOp keys one by "<op>"; failAt
	// fails the nth (1-based) call on "<op>:<path>" and failNth the nth call of
	// an operation.
	fail    map[string]error
	failOp  map[string]error
	failAt  map[string]int
	failNth map[string]int
	calls   map[string]int
	opN     map[string]int
	// owner is the uid of a path the fake creates.
	owner int
	// umask is applied by the create, as a host's umask is; Chmod is not
	// subject to it.
	umask fs.FileMode
	// createMode and createOwner override what an exclusive create records, so
	// a test can model a filesystem that dropped a mode bit or changed the owner.
	createMode  *fs.FileMode
	createOwner *int
	// ignoreChmod makes Chmod succeed without changing the mode.
	ignoreChmod bool
	// collide makes the first exclusive create find its name taken, as another
	// process that won the race would leave it.
	collide  bool
	collided bool
	// unreadableTemp makes every path an exclusive create made fail its Lstat,
	// so the read-back of the temporary file fails without the test naming it.
	unreadableTemp bool
}

func newFakeStoreFS(owner int) *fakeStoreFS {
	return &fakeStoreFS{
		entries: map[string]storeEntry{},
		fail:    map[string]error{},
		failOp:  map[string]error{},
		failAt:  map[string]int{},
		failNth: map[string]int{},
		calls:   map[string]int{},
		opN:     map[string]int{},
		owner:   owner,
	}
}

// put adds a path: ModeDir for a directory, ModeSymlink for a link.
func (f *fakeStoreFS) put(name string, mode fs.FileMode, owner int, data []byte) {
	f.entries[filepath.Clean(name)] = storeEntry{mode: mode, owner: owner, data: append([]byte(nil), data...)}
}

func (f *fakeStoreFS) putDir(name string, mode fs.FileMode, owner int) {
	f.put(name, fs.ModeDir|mode, owner, nil)
}

// before is the one place a fake call consults the injected failures.
func (f *fakeStoreFS) before(op, path string) error {
	key := op + ":" + path
	f.calls[key]++
	f.opN[op]++
	if n := f.failAt[key]; n != 0 && f.calls[key] == n {
		return fmt.Errorf("the injected %s failure", op)
	}
	if n := f.failNth[op]; n != 0 && f.opN[op] == n {
		return fmt.Errorf("the injected %s failure", op)
	}
	if err := f.failOp[op]; err != nil {
		return err
	}
	return f.fail[key]
}

func (f *fakeStoreFS) Lstat(name string) (deployercred.Info, error) {
	name = filepath.Clean(name)
	if err := f.before("lstat", name); err != nil {
		return deployercred.Info{}, err
	}
	if f.unreadableTemp && slices.Contains(f.created, name) {
		return deployercred.Info{}, fs.ErrPermission
	}
	e, ok := f.entries[name]
	if !ok {
		return deployercred.Info{}, fs.ErrNotExist
	}
	return deployercred.Info{Mode: e.mode, Owner: e.owner}, nil
}

func (f *fakeStoreFS) MkdirAll(name string, perm fs.FileMode) error {
	name = filepath.Clean(name)
	if err := f.before("mkdirall", name); err != nil {
		return err
	}
	var missing []string
	for p := name; ; p = filepath.Dir(p) {
		if e, ok := f.entries[p]; ok {
			if !e.mode.IsDir() {
				return errors.New("the path is not a directory")
			}
			break
		}
		missing = append(missing, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		f.entries[missing[i]] = storeEntry{mode: fs.ModeDir | perm&^f.umask, owner: f.owner}
	}
	return nil
}

func (f *fakeStoreFS) CreateExclusive(name string, perm fs.FileMode) (deployercred.File, error) {
	name = filepath.Clean(name)
	if err := f.before("createexclusive", name); err != nil {
		return nil, err
	}
	if f.collide && !f.collided {
		f.collided = true
		f.entries[name] = storeEntry{mode: perm, owner: f.owner}
		return nil, fs.ErrExist
	}
	if _, ok := f.entries[name]; ok {
		return nil, fs.ErrExist
	}
	mode := perm &^ f.umask
	if f.createMode != nil {
		mode = *f.createMode
	}
	owner := f.owner
	if f.createOwner != nil {
		owner = *f.createOwner
	}
	f.entries[name] = storeEntry{mode: mode, owner: owner}
	f.created = append(f.created, name)
	return &fakeFile{fsys: f, name: name}, nil
}

func (f *fakeStoreFS) Chmod(name string, mode fs.FileMode) error {
	name = filepath.Clean(name)
	if err := f.before("chmod", name); err != nil {
		return err
	}
	e, ok := f.entries[name]
	if !ok {
		return fs.ErrNotExist
	}
	if !f.ignoreChmod {
		e.mode = mode
		f.entries[name] = e
	}
	return nil
}

func (f *fakeStoreFS) Rename(oldpath, newpath string) error {
	oldp, newp := filepath.Clean(oldpath), filepath.Clean(newpath)
	if err := f.before("rename", oldp); err != nil {
		return err
	}
	e, ok := f.entries[oldp]
	if !ok {
		return fs.ErrNotExist
	}
	delete(f.entries, oldp)
	f.entries[newp] = e
	f.renames = append(f.renames, storeRename{oldpath: oldp, newpath: newp})
	return nil
}

func (f *fakeStoreFS) Remove(name string) error {
	name = filepath.Clean(name)
	err := f.before("remove", name)
	if errors.Is(err, fs.ErrNotExist) {
		// Another process removed it first; the path is gone either way.
		delete(f.entries, name)
		return err
	}
	if err != nil {
		return err
	}
	if _, ok := f.entries[name]; !ok {
		return fs.ErrNotExist
	}
	delete(f.entries, name)
	return nil
}

// fakeFile is the writer an exclusive create returns.
type fakeFile struct {
	fsys   *fakeStoreFS
	name   string
	closed bool
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.closed {
		return 0, errors.New("the injected write to a closed file")
	}
	if err := f.fsys.before("write", f.name); err != nil {
		return 0, err
	}
	e := f.fsys.entries[f.name]
	e.data = append(e.data, p...)
	f.fsys.entries[f.name] = e
	return len(p), nil
}

func (f *fakeFile) Sync() error {
	return f.fsys.before("sync", f.name)
}

func (f *fakeFile) Close() error {
	err := f.fsys.before("close", f.name)
	f.closed = true
	return err
}

// snapshot copies the fake's entries, so a test can show a refusal changed none.
func (f *fakeStoreFS) snapshot() map[string]storeEntry {
	out := make(map[string]storeEntry, len(f.entries))
	for name, e := range f.entries {
		out[name] = storeEntry{mode: e.mode, owner: e.owner, data: append([]byte(nil), e.data...)}
	}
	return out
}

// requireNothingWritten fails when Store created a file or changed a path on a
// call that refused before writing.
func requireNothingWritten(t *testing.T, f *fakeStoreFS, before map[string]storeEntry) {
	t.Helper()
	if len(f.created) != 0 {
		t.Fatalf("the refusal created %d files, want none", len(f.created))
	}
	after := f.snapshot()
	if len(after) != len(before) {
		t.Fatalf("the refusal left %d paths where %d were", len(after), len(before))
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok || got.mode != want.mode || got.owner != want.owner || !bytes.Equal(got.data, want.data) {
			t.Fatalf("the refusal changed %s: %+v, want %+v", name, got, want)
		}
	}
}

// requireNoLeftovers fails when a path Store created is still in the fake.
func requireNoLeftovers(t *testing.T, f *fakeStoreFS) {
	t.Helper()
	for _, name := range f.created {
		if _, ok := f.entries[name]; ok {
			t.Fatalf("the temporary file %s was left behind", name)
		}
	}
}

// goodStoreDir is an existing 0700 directory owned by storeUID, with a real
// parent, so a refusal case starts from a directory Store would accept.
func goodStoreDir() *fakeStoreFS {
	f := newFakeStoreFS(storeUID)
	f.putDir(storeDir, 0o700, storeUID)
	f.putDir("/home/example/.gt", 0o700, storeUID)
	f.putDir("/home/example", 0o700, storeUID)
	return f
}

func keyFiles() map[string][]byte {
	return map[string][]byte{"gt-deployer-apply.key": []byte("the key bytes")}
}

func uid(u int) *int { return &u }

// TestDeployerCredDiskLstat drives the host wrapper's error branch, which the
// in-memory fake never reaches: Disk.Lstat must return the host's error, not an
// Info that reads as an existing path.
func TestDeployerCredDiskLstat(t *testing.T) {
	t.Parallel()
	_, err := deployercred.Disk{}.Lstat(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Disk.Lstat of an absent path returned %v, want fs.ErrNotExist", err)
	}
}

func TestDeployerCredStoreRefusals(t *testing.T) {
	t.Parallel()
	target := filepath.Join(storeDir, "gt-deployer-apply.key")

	cases := []struct {
		name  string
		fs    *fakeStoreFS
		uid   int
		dir   string
		files map[string][]byte
		// dirties marks a case whose call creates a path before the refusal, so
		// only the error, the missing target and the absent leftovers are
		// asserted.
		dirties bool
		wantErr string
	}{
		{name: "a nil filesystem", fs: nil, uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "no filesystem"},
		{name: "a negative uid", fs: goodStoreDir(), uid: -1, dir: storeDir, files: keyFiles(), wantErr: "negative uid"},
		{name: "an empty directory", fs: goodStoreDir(), uid: storeUID, dir: "", files: keyFiles(), wantErr: "is empty"},
		{name: "a relative directory", fs: goodStoreDir(), uid: storeUID, dir: "deployer", files: keyFiles(), wantErr: "not an absolute path"},
		{name: "an unclean directory", fs: goodStoreDir(), uid: storeUID, dir: storeDir + "/.", files: keyFiles(), wantErr: "not a clean path"},
		{name: "no files", fs: goodStoreDir(), uid: storeUID, dir: storeDir, files: map[string][]byte{}, wantErr: "no files to store"},
		{name: "a name in a subdirectory", fs: goodStoreDir(), uid: storeUID, dir: storeDir, files: map[string][]byte{"sub/key": []byte("k")}, wantErr: "not one plain file name"},
		{name: "the parent name", fs: goodStoreDir(), uid: storeUID, dir: storeDir, files: map[string][]byte{"..": []byte("k")}, wantErr: "not one plain file name"},

		{name: "a symlinked directory", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(storeDir, fs.ModeSymlink, storeUID, nil)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is a symlink"},
		{name: "a symlinked parent", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put("/home/example", fs.ModeSymlink, storeUID, nil)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is a symlink"},
		{name: "a parent that is not a directory", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put("/home/example", 0o600, storeUID, []byte("a file"))
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is not a directory"},
		{name: "a directory in mode 0755", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(storeDir, 0o755, storeUID)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "not 0700"},
		{name: "a directory owned by another uid", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(storeDir, 0o700, storeUID+1)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "owned by uid"},
		{name: "a directory whose owner is not reported", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(storeDir, 0o700, -1)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is not reported"},
		{name: "a directory that is a regular file", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(storeDir, 0o600, storeUID, []byte("a file"))
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is not a directory"},
		{name: "an unreadable chain", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["lstat:"+storeDir] = fs.ErrPermission
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "could not read"},
		{name: "a directory that cannot be created", fs: func() *fakeStoreFS {
			f := newFakeStoreFS(storeUID)
			f.fail["mkdirall:"+storeDir] = errors.New("the injected mkdir failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "could not create the deployer directory"},
		{name: "a created directory that cannot be read back", fs: func() *fakeStoreFS {
			f := newFakeStoreFS(storeUID)
			f.failAt["lstat:"+storeDir] = 2
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirties: true, wantErr: "could not read the deployer directory"},

		{name: "an unreadable existing target", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["lstat:"+target] = fs.ErrPermission
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "could not read the existing credential"},
		{name: "an existing target that is a symlink", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, fs.ModeSymlink, storeUID, nil)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is a symlink"},
		{name: "an existing target that is a directory", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(target, 0o700, storeUID)
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "not a regular file"},
		{name: "an existing target in mode 0644", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o644, storeUID, []byte("old"))
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "not 0600"},
		{name: "an existing target owned by another uid", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o600, storeUID+1, []byte("old"))
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "owned by uid"},
		{name: "an existing target whose owner is not reported", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o600, -1, []byte("old"))
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "is not reported"},

		{name: "a failed exclusive create", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.failOp["createexclusive"] = fs.ErrPermission
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "could not create a temporary file"},
		{name: "a failed chmod", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.failOp["chmod"] = errors.New("the injected chmod failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirties: true, wantErr: "could not set the credential"},
		{name: "a failed write", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.failOp["write"] = errors.New("the injected write failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirties: true, wantErr: "could not write the credential"},
		{name: "a failed sync", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.failOp["sync"] = errors.New("the injected sync failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirties: true, wantErr: "could not write the credential"},
		{name: "a failed close", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.failOp["close"] = errors.New("the injected close failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirties: true, wantErr: "could not close the credential"},
		{name: "a failed rename", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.failOp["rename"] = errors.New("the injected rename failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirties: true, wantErr: "could not move the credential"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fsys deployercred.FS
			if tc.fs != nil {
				fsys = tc.fs
			}
			var before map[string]storeEntry
			if tc.fs != nil {
				before = tc.fs.snapshot()
			}
			err := deployercred.Store(fsys, tc.uid, tc.dir, tc.files)
			if err == nil {
				t.Fatalf("Store accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("the error %q does not say %q", err, tc.wantErr)
			}
			if tc.fs == nil {
				return
			}
			if !tc.dirties {
				requireNothingWritten(t, tc.fs, before)
				return
			}
			if _, ok := tc.fs.entries[target]; ok {
				t.Fatalf("the refusal left a file at %s", target)
			}
			requireNoLeftovers(t, tc.fs)
		})
	}
}

func TestDeployerCredStoreWrites(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{
		"gt-deployer-apply.key":        []byte("the key bytes"),
		"gt-deployer-apply.kubeconfig": []byte("the kubeconfig bytes"),
	}

	f := newFakeStoreFS(storeUID)
	f.putDir(storeDir, 0o700, storeUID)
	if err := deployercred.Store(f, storeUID, storeDir, files); err != nil {
		t.Fatalf("Store refused a good directory: %v", err)
	}
	for name, data := range files {
		target := filepath.Join(storeDir, name)
		e, ok := f.entries[target]
		if !ok {
			t.Fatalf("the credential %s was not written", target)
		}
		if e.mode != 0o600 || e.owner != storeUID {
			t.Fatalf("the credential %s is mode %04o owned by uid %d, want 0600 owned by %d", target, e.mode.Perm(), e.owner, storeUID)
		}
		if !bytes.Equal(e.data, data) {
			t.Fatalf("the credential %s holds %q, want %q", target, e.data, data)
		}
		renamed := false
		for _, r := range f.renames {
			if r.newpath == target {
				renamed = true
			}
		}
		if !renamed {
			t.Fatalf("the credential %s was never renamed into place", target)
		}
	}
	requireNoLeftovers(t, f)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	if err := deployercred.Check(f, storeUID, storeDir, names); err != nil {
		t.Fatalf("Check refused the pair Store just wrote: %v", err)
	}

	t.Run("a missing directory is created 0700", func(t *testing.T) {
		f := newFakeStoreFS(storeUID)
		if err := deployercred.Store(f, storeUID, storeDir, map[string][]byte{"key": []byte("k")}); err != nil {
			t.Fatalf("Store refused a directory it can create: %v", err)
		}
		e, ok := f.entries[storeDir]
		if !ok || e.mode != fs.ModeDir|0o700 || e.owner != storeUID {
			t.Fatalf("the created directory is %+v, want a 0700 directory owned by %d", e, storeUID)
		}
	})

	t.Run("an existing wider file is refused", func(t *testing.T) {
		f := newFakeStoreFS(storeUID)
		f.putDir(storeDir, 0o700, storeUID)
		target := filepath.Join(storeDir, "key")
		f.put(target, 0o644, storeUID, []byte("old bytes"))
		before := f.snapshot()
		err := deployercred.Store(f, storeUID, storeDir, map[string][]byte{"key": []byte("new bytes")})
		if err == nil {
			t.Fatal("Store accepted an existing file in mode 0644")
		}
		if !strings.Contains(err.Error(), "not 0600") {
			t.Fatalf("the error %q does not name the mode", err)
		}
		requireNothingWritten(t, f, before)
		if string(f.entries[target].data) != "old bytes" {
			t.Fatalf("the refusal changed the existing file to %q", f.entries[target].data)
		}
	})
}

// TestDeployerCredStoreOrder drives the order a credential is written in: the
// temporary file is verified before the rename, so a failure at any step leaves
// the target as it found it and no temporary file behind.
func TestDeployerCredStoreOrder(t *testing.T) {
	t.Parallel()

	// goodExistingDir is a directory holding a good credential of uid's, so each
	// failure case can show the target still holds the bytes it had.
	goodExistingDir := func(t *testing.T) (*fakeStoreFS, string) {
		t.Helper()
		f := goodStoreDir()
		target := filepath.Join(storeDir, "gt-deployer-apply.key")
		f.put(target, 0o600, storeUID, []byte("old bytes"))
		return f, target
	}

	failures := []struct {
		name   string
		setup  func(*fakeStoreFS)
		wantEr string
	}{
		{name: "a temporary file with the wrong mode", setup: func(f *fakeStoreFS) {
			f.umask = 0o777
			f.ignoreChmod = true
		}, wantEr: "holds mode 0000, not 0600"},
		{name: "a temporary file with another owner", setup: func(f *fakeStoreFS) {
			f.createOwner = uid(storeUID + 1)
		}, wantEr: "owned by uid 1001, not 1000"},
		{name: "a temporary file whose owner is not reported", setup: func(f *fakeStoreFS) {
			f.createOwner = uid(-1)
		}, wantEr: "is not reported"},
		{name: "a failed write", setup: func(f *fakeStoreFS) {
			f.failOp["write"] = errors.New("the injected write failure")
		}, wantEr: "could not write the credential"},
		{name: "a temporary file name that already exists", setup: func(f *fakeStoreFS) {
			f.collide = true
		}, wantEr: "could not create a temporary file"},
	}

	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			f, target := goodExistingDir(t)
			tc.setup(f)
			err := deployercred.Store(f, storeUID, storeDir, keyFiles())
			if err == nil {
				t.Fatalf("Store accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantEr) {
				t.Fatalf("the error %q does not say %q", err, tc.wantEr)
			}
			if got := string(f.entries[target].data); got != "old bytes" {
				t.Fatalf("the target holds %q, want the old bytes", got)
			}
			requireNoLeftovers(t, f)
		})
	}

	t.Run("a file another writer left in the way is not removed", func(t *testing.T) {
		f := goodStoreDir()
		f.collide = true
		if err := deployercred.Store(f, storeUID, storeDir, keyFiles()); err == nil {
			t.Fatal("Store accepted a temporary name another writer held")
		}
		if len(f.created) != 0 {
			t.Fatalf("Store recorded %d files of its own, want none", len(f.created))
		}
		extra := 0
		for name, e := range f.entries {
			switch name {
			case storeDir, "/home/example/.gt", "/home/example":
				continue
			}
			extra++
			if len(e.data) != 0 {
				t.Fatalf("the colliding file %s holds %q, want no bytes", name, e.data)
			}
		}
		if extra != 1 {
			t.Fatalf("the refusal left %d paths beyond the directory, want the one other writer's file", extra)
		}
	})

	t.Run("a temporary file that cannot be read back", func(t *testing.T) {
		f, target := goodExistingDir(t)
		f.unreadableTemp = true
		err := deployercred.Store(f, storeUID, storeDir, keyFiles())
		if err == nil {
			t.Fatal("Store accepted a temporary file it could not read back")
		}
		if !strings.Contains(err.Error(), "could not read back the temporary file") {
			t.Fatalf("the error %q does not name the read-back", err)
		}
		if got := string(f.entries[target].data); got != "old bytes" {
			t.Fatalf("the target holds %q, want the old bytes", got)
		}
		requireNoLeftovers(t, f)
	})

	t.Run("a close that fails beside a failed write is reported", func(t *testing.T) {
		f, target := goodExistingDir(t)
		f.failOp["write"] = errors.New("the injected write failure")
		f.failOp["close"] = errors.New("the injected close failure")
		err := deployercred.Store(f, storeUID, storeDir, keyFiles())
		if err == nil {
			t.Fatal("Store accepted a write that failed")
		}
		if !strings.Contains(err.Error(), "could not write the credential") {
			t.Fatalf("the error %q does not name the write", err)
		}
		if !strings.Contains(err.Error(), "could not be closed") {
			t.Fatalf("the error %q does not report the close it could not make", err)
		}
		if got := string(f.entries[target].data); got != "old bytes" {
			t.Fatalf("the target holds %q, want the old bytes", got)
		}
		requireNoLeftovers(t, f)
	})

	t.Run("a temporary file already gone is not reported as a leftover", func(t *testing.T) {
		f, target := goodExistingDir(t)
		f.failOp["write"] = errors.New("the injected write failure")
		f.failOp["remove"] = fs.ErrNotExist
		err := deployercred.Store(f, storeUID, storeDir, keyFiles())
		if err == nil {
			t.Fatal("Store accepted a write that failed")
		}
		if !strings.Contains(err.Error(), "could not write the credential") {
			t.Fatalf("the error %q does not name the write", err)
		}
		if strings.Contains(err.Error(), "could not be removed") {
			t.Fatalf("the error %q reports a temporary file that is already gone", err)
		}
		if got := string(f.entries[target].data); got != "old bytes" {
			t.Fatalf("the target holds %q, want the old bytes", got)
		}
		requireNoLeftovers(t, f)
	})

	t.Run("a temporary file that cannot be removed is reported", func(t *testing.T) {
		f, target := goodExistingDir(t)
		f.failOp["write"] = errors.New("the injected write failure")
		f.failOp["remove"] = errors.New("the injected remove failure")
		err := deployercred.Store(f, storeUID, storeDir, keyFiles())
		if err == nil {
			t.Fatal("Store accepted a write that failed")
		}
		if got := string(f.entries[target].data); got != "old bytes" {
			t.Fatalf("the target holds %q, want the old bytes", got)
		}
		if !strings.Contains(err.Error(), "could not write the credential") {
			t.Fatalf("the error %q does not name the write", err)
		}
		if !strings.Contains(err.Error(), "could not be removed and may hold the credential") {
			t.Fatalf("the error %q does not report the temporary file it could not remove", err)
		}
	})
}

func TestDeployerCredStoreHalfPair(t *testing.T) {
	t.Parallel()
	first := "gt-deployer-apply.crt"
	second := "gt-deployer-apply.key"
	files := map[string][]byte{first: []byte("the certificate bytes"), second: []byte("the key bytes")}

	f := goodStoreDir()
	// The first file is written whole; the second fails, so Store leaves the
	// pair half-written.
	f.failNth["write"] = 2
	err := deployercred.Store(f, storeUID, storeDir, files)
	if err == nil {
		t.Fatal("Store accepted a write that failed on the second file")
	}
	if !strings.Contains(err.Error(), "could not write the credential") {
		t.Fatalf("the error %q does not name the write", err)
	}

	got, ok := f.entries[filepath.Join(storeDir, first)]
	if !ok {
		t.Fatalf("the first file %s was removed, and a failure on the second leaves the files already renamed", first)
	}
	if got.mode != 0o600 || got.owner != storeUID || string(got.data) != string(files[first]) {
		t.Fatalf("the first file is %+v, want 0600 owned by %d holding its bytes", got, storeUID)
	}
	if _, ok := f.entries[filepath.Join(storeDir, second)]; ok {
		t.Fatalf("the second file %s was written", second)
	}
	requireNoLeftovers(t, f)

	if err := deployercred.Check(f, storeUID, storeDir, []string{first, second}); err == nil {
		t.Fatal("Check accepted the half-written pair")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("the error %q does not name the missing file", err)
	}
}

func TestDeployerCredStoreCheck(t *testing.T) {
	t.Parallel()
	name := "gt-deployer-apply.key"
	target := filepath.Join(storeDir, name)
	names := []string{name}

	cases := []struct {
		name    string
		fs      *fakeStoreFS
		uid     int
		dir     string
		names   []string
		wantErr string
	}{
		{name: "a nil filesystem", fs: nil, uid: storeUID, dir: storeDir, names: names, wantErr: "no filesystem"},
		{name: "a negative uid", fs: goodStoreDir(), uid: -1, dir: storeDir, names: names, wantErr: "negative uid"},
		{name: "an empty directory", fs: goodStoreDir(), uid: storeUID, dir: "", names: names, wantErr: "is empty"},
		{name: "a relative directory", fs: goodStoreDir(), uid: storeUID, dir: "deployer", names: names, wantErr: "not an absolute path"},
		{name: "no names", fs: goodStoreDir(), uid: storeUID, dir: storeDir, names: nil, wantErr: "no files to check"},
		{name: "a name in a subdirectory", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o600, storeUID, []byte("k"))
			return f
		}(), uid: storeUID, dir: storeDir, names: []string{"sub/key"}, wantErr: "not one plain file name"},
		{name: "a missing directory", fs: newFakeStoreFS(storeUID), uid: storeUID, dir: storeDir, names: names, wantErr: "does not exist"},
		{name: "a symlinked directory", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(storeDir, fs.ModeSymlink, storeUID, nil)
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "is a symlink"},
		{name: "a directory in mode 0755", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(storeDir, 0o755, storeUID)
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "not 0700"},
		{name: "a directory owned by another uid", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(storeDir, 0o700, storeUID+1)
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "owned by uid"},
		{name: "an unreadable chain", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["lstat:"+storeDir] = fs.ErrPermission
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "could not read"},
		{name: "a missing file", fs: goodStoreDir(), uid: storeUID, dir: storeDir, names: names, wantErr: "does not exist"},
		{name: "an unreadable file", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["lstat:"+target] = fs.ErrPermission
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "could not read the credential"},
		{name: "a file that is a symlink", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, fs.ModeSymlink, storeUID, nil)
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "is a symlink"},
		{name: "a file that is a directory", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.putDir(target, 0o600, storeUID)
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "not a regular file"},
		{name: "a file in mode 0644", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o644, storeUID, []byte("k"))
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "not 0600"},
		{name: "a file owned by another uid", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o600, storeUID+1, []byte("k"))
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "owned by uid"},
		{name: "a file whose owner is not reported", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(target, 0o600, -1, []byte("k"))
			return f
		}(), uid: storeUID, dir: storeDir, names: names, wantErr: "is not reported"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fsys deployercred.FS
			if tc.fs != nil {
				fsys = tc.fs
			}
			err := deployercred.Check(fsys, tc.uid, tc.dir, tc.names)
			if err == nil {
				t.Fatalf("Check accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("the error %q does not say %q", err, tc.wantErr)
			}
		})
	}

	t.Run("a good file passes", func(t *testing.T) {
		f := goodStoreDir()
		f.put(target, 0o600, storeUID, []byte("the key bytes"))
		if err := deployercred.Check(f, storeUID, storeDir, names); err != nil {
			t.Fatalf("Check refused a 0600 credential of uid's: %v", err)
		}
	})
}

// testCAPEM is a self-signed CA certificate, so the kubeconfig cases carry a CA
// that parses as one.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate the test CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gt deployer test CA"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31-1, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("could not create the test CA certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// kubeDoc is the kubeconfig as kubectl's schema reads it, decoded independently
// of the package's own struct.
type kubeDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Clusters   []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificate     string `yaml:"client-certificate"`
			ClientKey             string `yaml:"client-key"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	CurrentContext string `yaml:"current-context"`
}

func TestDeployerCredStoreKubeconfig(t *testing.T) {
	t.Parallel()
	caPEM := testCAPEM(t)
	certPath := filepath.Join(storeDir, "gt-deployer-apply.crt")
	keyPath := filepath.Join(storeDir, "gt-deployer-apply.key")
	const contextName = "gt-deployer-apply"

	for _, server := range []string{"https://127.0.0.1:6443", "https://localhost:6443", "https://[::1]:6443"} {
		t.Run("a loopback server at "+server, func(t *testing.T) {
			out, err := deployercred.Kubeconfig(server, caPEM, certPath, keyPath, contextName)
			if err != nil {
				t.Fatalf("Kubeconfig refused a loopback server: %v", err)
			}
			var doc kubeDoc
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatalf("the kubeconfig does not parse: %v", err)
			}
			if doc.APIVersion != "v1" || doc.Kind != "Config" {
				t.Fatalf("the kubeconfig is %s %s, want v1 Config", doc.APIVersion, doc.Kind)
			}
			if len(doc.Clusters) != 1 || len(doc.Users) != 1 || len(doc.Contexts) != 1 {
				t.Fatalf("the kubeconfig holds %d clusters, %d users and %d contexts, want one of each", len(doc.Clusters), len(doc.Users), len(doc.Contexts))
			}
			if got := doc.Clusters[0]; got.Name != contextName || got.Cluster.Server != server {
				t.Fatalf("the cluster is %+v, want %q at %s", got, contextName, server)
			}
			ca, err := base64.StdEncoding.DecodeString(doc.Clusters[0].Cluster.CertificateAuthorityData)
			if err != nil || !bytes.Equal(ca, caPEM) {
				t.Fatalf("the CA data decodes to %q, %v; want the certificate", ca, err)
			}
			user := doc.Users[0]
			if user.Name != contextName {
				t.Fatalf("the user is named %q, want %q", user.Name, contextName)
			}
			if user.User.ClientCertificate != certPath || user.User.ClientKey != keyPath {
				t.Fatalf("the user references %q and %q, want %s and %s", user.User.ClientCertificate, user.User.ClientKey, certPath, keyPath)
			}
			if user.User.ClientCertificateData != "" || user.User.ClientKeyData != "" {
				t.Fatalf("the user embeds the certificate or key: %q, %q", user.User.ClientCertificateData, user.User.ClientKeyData)
			}
			ctx := doc.Contexts[0]
			if ctx.Name != contextName || ctx.Context.Cluster != contextName || ctx.Context.User != contextName {
				t.Fatalf("the context is %+v, want %q naming its own cluster and user", ctx, contextName)
			}
			if doc.CurrentContext != contextName {
				t.Fatalf("the current context is %q, want %q", doc.CurrentContext, contextName)
			}
		})
	}

	t.Run("a .kube/config target is refused by Store", func(t *testing.T) {
		f := newFakeStoreFS(storeUID)
		f.putDir("/home/example/.kube", 0o700, storeUID)
		before := f.snapshot()
		err := deployercred.Store(f, storeUID, "/home/example/.kube", map[string][]byte{"config": []byte("the admin config")})
		if err == nil {
			t.Fatal("Store wrote a .kube/config")
		}
		if !strings.Contains(err.Error(), ".kube/config") {
			t.Fatalf("the error %q does not name .kube/config", err)
		}
		requireNothingWritten(t, f, before)
	})

	t.Run("refusals", func(t *testing.T) {
		type args struct {
			server string
			ca     []byte
			cert   string
			key    string
			ctx    string
		}
		good := args{server: "https://127.0.0.1:6443", ca: caPEM, cert: certPath, key: keyPath, ctx: contextName}
		cases := []struct {
			name    string
			mutate  func(*args)
			wantErr string
		}{
			{name: "the admin context name", mutate: func(a *args) { a.ctx = "colima-gt-control" }, wantErr: "admin context"},
			{name: "an empty context name", mutate: func(a *args) { a.ctx = "" }, wantErr: "context name is empty"},
			{name: "an http server", mutate: func(a *args) { a.server = "http://127.0.0.1:6443" }, wantErr: "not an https URL"},
			{name: "a server that does not parse", mutate: func(a *args) { a.server = "127.0.0.1:6443" }, wantErr: "does not parse"},
			{name: "a server with user info", mutate: func(a *args) { a.server = "https://deployer:secret@127.0.0.1:6443" }, wantErr: "carries user info"},
			{name: "a remote address", mutate: func(a *args) { a.server = "https://10.1.2.3:6443" }, wantErr: "not a loopback address"},
			{name: "a remote host name", mutate: func(a *args) { a.server = "https://control.example.com:6443" }, wantErr: "not a loopback address"},
			{name: "an empty CA", mutate: func(a *args) { a.ca = nil }, wantErr: "CA is empty"},
			{name: "a CA that is not a certificate", mutate: func(a *args) {
				a.ca = []byte("-----BEGIN PRIVATE KEY-----\nY2E=\n-----END PRIVATE KEY-----\n")
			}, wantErr: "no CERTIFICATE PEM block"},
			{name: "a CA CERTIFICATE block that does not parse", mutate: func(a *args) {
				a.ca = []byte("-----BEGIN CERTIFICATE-----\nY2E=\n-----END CERTIFICATE-----\n")
			}, wantErr: "does not parse as a certificate"},
			{name: "an empty certificate path", mutate: func(a *args) { a.cert = "" }, wantErr: "certificate path is empty"},
			{name: "a relative certificate path", mutate: func(a *args) { a.cert = "cert.pem" }, wantErr: "certificate path"},
			{name: "an empty key path", mutate: func(a *args) { a.key = "" }, wantErr: "key path is empty"},
			{name: "a relative key path", mutate: func(a *args) { a.key = "key.pem" }, wantErr: "key path"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				args := good
				tc.mutate(&args)
				out, err := deployercred.Kubeconfig(args.server, args.ca, args.cert, args.key, args.ctx)
				if err == nil {
					t.Fatalf("Kubeconfig accepted %s", tc.name)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("the error %q does not say %q", err, tc.wantErr)
				}
				if out != nil {
					t.Fatalf("the refusal returned a kubeconfig: %q", out)
				}
			})
		}
	})
}
