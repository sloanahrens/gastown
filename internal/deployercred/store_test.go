package deployercred_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/deployercred"
)

const (
	storeUID = 1000
	storeDir = "/home/example/.gt/deployer"
	storeCA  = "-----BEGIN CERTIFICATE-----\nY2E=\n-----END CERTIFICATE-----\n"
)

// storeEntry is one path of the fake filesystem: its mode (ModeDir set for a
// directory), the uid that owns it, and its bytes.
type storeEntry struct {
	mode  fs.FileMode
	owner int
	data  []byte
}

type storeWrite struct {
	name string
	mode fs.FileMode
}

type storeRename struct{ oldpath, newpath string }

// fakeStoreFS is the in-memory filesystem Store is tested through. It records
// every write and rename, so a refusal can be shown to write nothing, and fails
// the call a test names.
type fakeStoreFS struct {
	entries map[string]storeEntry
	writes  []storeWrite
	renames []storeRename
	// fail keys an error by "<op>:<path>"; a rename is keyed by its old path.
	fail map[string]error
	// lstatAt fails the nth (1-based) Lstat of a path, for a branch that reads
	// one path twice.
	lstatAt map[string]int
	lstatN  map[string]int
	// owner is the uid of a path the fake creates.
	owner int
	// umask is applied by WriteFile, as a host's umask is; Chmod is not subject
	// to it.
	umask fs.FileMode
	// ignoreChmod makes Chmod succeed without changing the mode.
	ignoreChmod bool
}

func newFakeStoreFS(owner int) *fakeStoreFS {
	return &fakeStoreFS{
		entries: map[string]storeEntry{},
		fail:    map[string]error{},
		lstatAt: map[string]int{},
		lstatN:  map[string]int{},
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

func (f *fakeStoreFS) Lstat(name string) (deployercred.Info, error) {
	name = filepath.Clean(name)
	if err := f.fail["lstat:"+name]; err != nil {
		return deployercred.Info{}, err
	}
	f.lstatN[name]++
	if f.lstatAt[name] == f.lstatN[name] {
		return deployercred.Info{}, errors.New("the injected lstat failure")
	}
	e, ok := f.entries[name]
	if !ok {
		return deployercred.Info{}, fs.ErrNotExist
	}
	return deployercred.Info{Mode: e.mode, Owner: e.owner}, nil
}

func (f *fakeStoreFS) MkdirAll(name string, perm fs.FileMode) error {
	name = filepath.Clean(name)
	if err := f.fail["mkdirall:"+name]; err != nil {
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

func (f *fakeStoreFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	name = filepath.Clean(name)
	if err := f.fail["writefile:"+name]; err != nil {
		return err
	}
	mode := perm &^ f.umask
	f.entries[name] = storeEntry{mode: mode, owner: f.owner, data: append([]byte(nil), data...)}
	f.writes = append(f.writes, storeWrite{name: name, mode: mode})
	return nil
}

func (f *fakeStoreFS) Chmod(name string, mode fs.FileMode) error {
	name = filepath.Clean(name)
	if err := f.fail["chmod:"+name]; err != nil {
		return err
	}
	e, ok := f.entries[name]
	if !ok {
		return fs.ErrNotExist
	}
	if !f.ignoreChmod {
		e.mode = mode
	}
	f.entries[name] = e
	return nil
}

func (f *fakeStoreFS) Rename(oldpath, newpath string) error {
	oldp, newp := filepath.Clean(oldpath), filepath.Clean(newpath)
	if err := f.fail["rename:"+oldp]; err != nil {
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

// tempPath is the temporary name Store writes name under, so a test can put a
// file in its way or fail the call.
func tempPath(dir, name string) string {
	return filepath.Join(dir, "."+name+"."+strconv.Itoa(os.Getpid())+".tmp")
}

// snapshot copies the fake's entries, so a test can show a refusal changed none.
func (f *fakeStoreFS) snapshot() map[string]storeEntry {
	out := make(map[string]storeEntry, len(f.entries))
	for name, e := range f.entries {
		out[name] = storeEntry{mode: e.mode, owner: e.owner, data: append([]byte(nil), e.data...)}
	}
	return out
}

// requireNothingWritten fails when Store wrote or renamed a file, or changed a
// path, on a call that refused before writing.
func requireNothingWritten(t *testing.T, f *fakeStoreFS, before map[string]storeEntry) {
	t.Helper()
	if len(f.writes) != 0 || len(f.renames) != 0 {
		t.Fatalf("the refusal wrote %d files and renamed %d, want no write", len(f.writes), len(f.renames))
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
	temp := tempPath(storeDir, "gt-deployer-apply.key")

	cases := []struct {
		name  string
		fs    *fakeStoreFS
		uid   int
		dir   string
		files map[string][]byte
		// dirty marks a case whose call legitimately creates a directory or
		// writes a file before the refusal, so only the error is asserted.
		dirty   bool
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
			f.lstatAt[storeDir] = 2
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "could not read the deployer directory"},

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

		{name: "a temporary file that already exists", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.put(temp, 0o600, storeUID, []byte("left over"))
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "already exists"},
		{name: "an unreadable temporary path", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["lstat:"+temp] = fs.ErrPermission
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "could not read the temporary path"},
		{name: "a failed write", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["writefile:"+temp] = errors.New("the injected write failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), wantErr: "could not write the credential"},
		{name: "a failed chmod", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["chmod:"+temp] = errors.New("the injected chmod failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "could not set the credential"},
		{name: "a failed rename", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.fail["rename:"+temp] = errors.New("the injected rename failure")
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "could not move the credential"},

		{name: "an unreadable written file", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.lstatAt[target] = 2
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "could not read back the credential"},
		{name: "a written file that lost its mode", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.umask = 0o777
			f.ignoreChmod = true
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "after the write, not 0600"},
		{name: "a written file with another owner", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.owner = storeUID + 1
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "owned by uid"},
		{name: "a written file whose owner is not reported", fs: func() *fakeStoreFS {
			f := goodStoreDir()
			f.owner = -1
			return f
		}(), uid: storeUID, dir: storeDir, files: keyFiles(), dirty: true, wantErr: "is not reported"},
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
			if tc.fs != nil && !tc.dirty {
				requireNothingWritten(t, tc.fs, before)
			}
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
	if len(f.writes) != len(files) || len(f.renames) != len(files) {
		t.Fatalf("%d writes and %d renames for %d files, want one of each per file", len(f.writes), len(f.renames), len(files))
	}
	for _, w := range f.writes {
		if w.mode != 0o600 {
			t.Fatalf("the temporary file %s was created %04o, not 0600", w.name, w.mode)
		}
		if filepath.Dir(w.name) != storeDir || !strings.HasSuffix(w.name, ".tmp") {
			t.Fatalf("the write went to %s, not a temporary name in %s", w.name, storeDir)
		}
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
				if !strings.HasSuffix(r.oldpath, ".tmp") || filepath.Dir(r.oldpath) != storeDir {
					t.Fatalf("the credential %s came from %s, not a temporary name in %s", target, r.oldpath, storeDir)
				}
			}
		}
		if !renamed {
			t.Fatalf("the credential %s was never renamed into place", target)
		}
	}
	for name := range f.entries {
		if strings.HasSuffix(name, ".tmp") {
			t.Fatalf("the temporary file %s was left behind", name)
		}
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
	certPath := filepath.Join(storeDir, "gt-deployer-apply.crt")
	keyPath := filepath.Join(storeDir, "gt-deployer-apply.key")
	const contextName = "gt-deployer-apply"

	for _, server := range []string{"https://127.0.0.1:6443", "https://localhost:6443", "https://[::1]:6443"} {
		t.Run("a loopback server at "+server, func(t *testing.T) {
			out, err := deployercred.Kubeconfig(server, []byte(storeCA), certPath, keyPath, contextName)
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
			if err != nil || string(ca) != storeCA {
				t.Fatalf("the CA data decodes to %q, %v; want %q", ca, err, storeCA)
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
		good := args{server: "https://127.0.0.1:6443", ca: []byte(storeCA), cert: certPath, key: keyPath, ctx: contextName}
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
