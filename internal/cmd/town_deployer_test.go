package cmd

import (
	"bytes"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/deployercred"
)

const deployerTestDir = "/home/example/.gt/deployer"

// deployerEntry is one path of the fake filesystem: its mode (ModeDir set for a
// directory), the uid that owns it, and its bytes.
type deployerEntry struct {
	mode  fs.FileMode
	owner int
	data  []byte
}

// fakeDeployerFS is the in-memory filesystem deployercred.Store is tested
// through. A path it does not hold does not exist, and MkdirAll records the
// mode it is given without modelling a umask.
type fakeDeployerFS struct {
	uid     int
	entries map[string]deployerEntry
}

func newFakeDeployerFS(uid int) *fakeDeployerFS {
	return &fakeDeployerFS{uid: uid, entries: map[string]deployerEntry{}}
}

func (f *fakeDeployerFS) Lstat(name string) (deployercred.Info, error) {
	e, ok := f.entries[filepath.Clean(name)]
	if !ok {
		return deployercred.Info{}, fs.ErrNotExist
	}
	return deployercred.Info{Mode: e.mode, Owner: e.owner}, nil
}

func (f *fakeDeployerFS) MkdirAll(dir string, perm fs.FileMode) error {
	f.entries[filepath.Clean(dir)] = deployerEntry{mode: fs.ModeDir | perm, owner: f.uid}
	return nil
}

func (f *fakeDeployerFS) CreateExclusive(name string, perm fs.FileMode) (deployercred.File, error) {
	name = filepath.Clean(name)
	if _, ok := f.entries[name]; ok {
		return nil, fs.ErrExist
	}
	f.entries[name] = deployerEntry{mode: perm, owner: f.uid}
	return &fakeDeployerFile{fsys: f, name: name}, nil
}

func (f *fakeDeployerFS) Chmod(name string, mode fs.FileMode) error {
	name = filepath.Clean(name)
	e := f.entries[name]
	e.mode = mode
	f.entries[name] = e
	return nil
}

func (f *fakeDeployerFS) Rename(oldpath, newpath string) error {
	oldpath, newpath = filepath.Clean(oldpath), filepath.Clean(newpath)
	e, ok := f.entries[oldpath]
	if !ok {
		return fs.ErrNotExist
	}
	delete(f.entries, oldpath)
	f.entries[newpath] = e
	return nil
}

func (f *fakeDeployerFS) Remove(name string) error {
	name = filepath.Clean(name)
	if _, ok := f.entries[name]; !ok {
		return fs.ErrNotExist
	}
	delete(f.entries, name)
	return nil
}

// fakeDeployerFile writes into the fake filesystem's entry for its own path.
type fakeDeployerFile struct {
	fsys *fakeDeployerFS
	name string
}

func (f *fakeDeployerFile) Write(p []byte) (int, error) {
	e := f.fsys.entries[f.name]
	e.data = append(e.data, p...)
	f.fsys.entries[f.name] = e
	return len(p), nil
}

func (f *fakeDeployerFile) Sync() error  { return nil }
func (f *fakeDeployerFile) Close() error { return nil }

// failingDeployerWriter refuses every stdout write.
type failingDeployerWriter struct{}

func (failingDeployerWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// deployerUser reads a fixed user, so a test never consults the host's own.
func deployerUser(name, uid string) func() (*user.User, error) {
	return func() (*user.User, error) { return &user.User{Username: name, Uid: uid}, nil }
}

// newCSREnv is the command a test runs: a fixed deployer user on uid 1000,
// outside a pod, with the fake filesystem and out.
func newCSREnv(fsys deployercred.FS, out io.Writer) townDeployerCSREnv {
	return townDeployerCSREnv{
		deployerEnv: deployerEnv{
			command: "csr",
			asUser:  "deployer",
			getenv:  func(string) string { return "" },
			current: deployerUser("deployer", "1000"),
			fs:      fsys,
			write: func(string, []byte, fs.FileMode) error {
				return errors.New("wrote a file without --out-csr")
			},
			out: out,
		},
		name: "gt-deployer-apply",
		days: 30,
		dir:  deployerTestDir,
	}
}

// hasDeployerKey reports whether the fake filesystem holds any .key file.
func hasDeployerKey(fsys *fakeDeployerFS) bool {
	for name := range fsys.entries {
		if strings.HasSuffix(name, ".key") {
			return true
		}
	}
	return false
}

// parseDeployerCSR reads the request a test captured as the YAML the API server
// takes.
func parseDeployerCSR(t *testing.T, b []byte) (kind, name string) {
	t.Helper()
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the signing request did not parse as YAML: %v", err)
	}
	return doc.Kind, doc.Metadata.Name
}

func TestTownDeployerCSR(t *testing.T) {
	t.Parallel()

	t.Run("writes the key and prints the request", func(t *testing.T) {
		t.Parallel()
		fsys := newFakeDeployerFS(1000)
		var out bytes.Buffer
		if err := runTownDeployerCSRIn(newCSREnv(fsys, &out)); err != nil {
			t.Fatalf("runTownDeployerCSRIn = %v", err)
		}

		keyPath := filepath.Join(deployerTestDir, "gt-deployer-apply.key")
		key, ok := fsys.entries[keyPath]
		if !ok {
			t.Fatalf("the key was not written to %s", keyPath)
		}
		if key.mode != 0o600 {
			t.Errorf("the key holds mode %04o, want 0600", key.mode)
		}
		if key.owner != 1000 {
			t.Errorf("the key is owned by uid %d, want 1000", key.owner)
		}
		block, _ := pem.Decode(key.data)
		if block == nil || block.Type != "PRIVATE KEY" {
			t.Fatalf("the stored bytes are not a private key PEM: %q", key.data)
		}

		kind, name := parseDeployerCSR(t, out.Bytes())
		if kind != "CertificateSigningRequest" {
			t.Errorf("the printed kind is %q, want CertificateSigningRequest", kind)
		}
		if name != "gt-deployer-apply" {
			t.Errorf("the printed request names %q, want gt-deployer-apply", name)
		}
		if strings.Contains(out.String(), "PRIVATE KEY") {
			t.Error("the printed request carries a private key block")
		}
		if bytes.Contains(out.Bytes(), key.data) {
			t.Error("the key bytes reached standard output")
		}
	})

	t.Run("writes the request to --out-csr", func(t *testing.T) {
		t.Parallel()
		fsys := newFakeDeployerFS(1000)
		var out bytes.Buffer
		env := newCSREnv(fsys, &out)
		env.outCSR = "/home/example/apply.csr.yaml"
		var writtenName string
		var written []byte
		env.write = func(name string, data []byte, perm fs.FileMode) error {
			writtenName, written = name, append([]byte(nil), data...)
			if perm != 0o644 {
				t.Errorf("the request file holds mode %04o, want 0644", perm)
			}
			return nil
		}
		if err := runTownDeployerCSRIn(env); err != nil {
			t.Fatalf("runTownDeployerCSRIn = %v", err)
		}
		if writtenName != env.outCSR {
			t.Errorf("the request went to %q, want %q", writtenName, env.outCSR)
		}
		kind, name := parseDeployerCSR(t, written)
		if kind != "CertificateSigningRequest" || name != "gt-deployer-apply" {
			t.Errorf("the written request is %s %q, want CertificateSigningRequest gt-deployer-apply", kind, name)
		}
		if out.Len() != 0 {
			t.Errorf("standard output holds %q, and --out-csr was given", out.String())
		}
		key := fsys.entries[filepath.Join(deployerTestDir, "gt-deployer-apply.key")]
		if bytes.Contains(written, key.data) {
			t.Error("the key bytes reached the request file")
		}
	})
}

func TestTownDeployerRefuses(t *testing.T) {
	t.Parallel()

	storeRefused := func(fsys *fakeDeployerFS) {
		fsys.entries[deployerTestDir] = deployerEntry{mode: fs.ModeDir | 0o755, owner: 1000}
	}

	cases := []struct {
		name string
		// edit adjusts the command's environment; fsys is the filesystem the
		// fake Store would write into.
		edit func(env *townDeployerCSREnv, fsys *fakeDeployerFS)
		want string
		// afterStore says the refusal comes after Store wrote the key: the
		// request output failed, and the key it signs is already on disk.
		afterStore bool
	}{
		{
			name: "inside a pod",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.getenv = func(string) string { return "10.0.0.1" }
			},
			want: "deployer user only",
		},
		{
			name: "another running user",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.current = deployerUser("alice", "1001")
			},
			want: "not --as-user",
		},
		{
			name: "no --as-user",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.asUser = ""
			},
			want: "needs --as-user",
		},
		{
			name: "the user cannot be read",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.current = func() (*user.User, error) { return nil, errors.New("no user") }
			},
			want: "reading the user",
		},
		{
			name: "a uid that is not a number",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.current = deployerUser("deployer", "root")
			},
			want: "is not a number",
		},
		{
			name: "days below one",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) { env.days = 0 },
			want: "--days 0",
		},
		{
			name: "days above the bound",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) { env.days = 31 },
			want: "--days 31",
		},
		{
			name: "an unknown identity",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) { env.name = "gt-deployer-root" },
			want: "the deployer identity",
		},
		{
			name: "policy beyond its one day",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.name = "gt-deployer-policy"
				env.days = 2
			},
			want: "lives at most",
		},
		{
			name: "the directory fails the store's check",
			edit: func(_ *townDeployerCSREnv, fsys *fakeDeployerFS) { storeRefused(fsys) },
			want: "writing the gt-deployer-apply key",
		},
		{
			name: "the request file cannot be written",
			edit: func(env *townDeployerCSREnv, _ *fakeDeployerFS) {
				env.outCSR = "/home/example/apply.csr.yaml"
				env.write = func(string, []byte, fs.FileMode) error { return errors.New("disk full") }
			},
			want:       "writing the gt-deployer-apply signing request to",
			afterStore: true,
		},
		{
			name:       "standard output refuses the write",
			edit:       func(env *townDeployerCSREnv, _ *fakeDeployerFS) { env.out = failingDeployerWriter{} },
			want:       "printing the gt-deployer-apply signing request",
			afterStore: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeDeployerFS(1000)
			env := newCSREnv(fsys, &bytes.Buffer{})
			tc.edit(&env, fsys)
			err := runTownDeployerCSRIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownDeployerCSRIn = %v, want a refusal naming %q", err, tc.want)
			}
			if got := hasDeployerKey(fsys); got != tc.afterStore {
				t.Errorf("a key was written = %v, want %v: %v", got, tc.afterStore, fsys.entries)
			}
		})
	}
}

// TestTownDeployerCSRFlagReadsRefuseAnUndefinedFlag keeps every flag read in
// runTownDeployerCSR covered, so a flag that stops being registered fails here.
func TestTownDeployerCSRFlagReadsRefuseAnUndefinedFlag(t *testing.T) {
	t.Parallel()
	flags := []struct {
		name  string
		asInt bool
	}{
		{name: "as-user"},
		{name: "name"},
		{name: "days", asInt: true},
		{name: "dir"},
		{name: "out-csr"},
	}
	for i := range flags {
		defined := flags[:i]
		want := "reading --" + flags[i].name
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, f := range defined {
				if f.asInt {
					cmd.Flags().Int(f.name, 0, "")
				} else {
					cmd.Flags().String(f.name, "", "")
				}
			}
			err := runTownDeployerCSR(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("runTownDeployerCSR = %v, want a refusal naming %q", err, want)
			}
		})
	}
}

// TestTownDeployerCSRFlagsRegistered proves the command the binary runs holds
// every flag the run function reads.
func TestTownDeployerCSRFlagsRegistered(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"as-user", "name", "days", "dir", "out-csr"} {
		if townDeployerCSRCmd.Flags().Lookup(name) == nil {
			t.Errorf("gt town deployer csr has no --%s flag", name)
		}
	}
	found, _, err := townCmd.Find([]string{"deployer"})
	if err != nil || found != townDeployerCmd {
		t.Errorf("gt town Find(deployer) = %v, %v; want the deployer command", found, err)
	}
}
