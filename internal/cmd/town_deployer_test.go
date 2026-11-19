package cmd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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

const (
	kubeconfigTestCAPath   = "/home/example/ca.crt"
	kubeconfigTestCertPath = "/home/example/gt-deployer-apply.crt"
	kubeconfigTestServer   = "https://127.0.0.1:64459"
)

// deployerTestCertificate builds a self-signed certificate PEM for the test,
// so a CA or a signed certificate the command reads is a real one.
func deployerTestCertificate(t *testing.T, commonName string) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate the test certificate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31-1, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("could not create the test certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// paths returns the fake filesystem's paths in a stable order, so a test can
// show a refusal wrote nothing.
func (f *fakeDeployerFS) paths() string {
	names := make([]string, 0, len(f.entries))
	for name := range f.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

// newKubeconfigEnv is the kubeconfig command a test runs: the fixed deployer
// user outside a pod, on the fake filesystem, reading the files it is given.
func newKubeconfigEnv(fsys *fakeDeployerFS, files map[string][]byte) townDeployerKubeconfigEnv {
	return townDeployerKubeconfigEnv{
		deployerEnv: deployerEnv{
			command: "kubeconfig",
			asUser:  "deployer",
			getenv:  func(string) string { return "" },
			current: deployerUser("deployer", "1000"),
			fs:      fsys,
			readFile: func(path string) ([]byte, error) {
				data, ok := files[path]
				if !ok {
					return nil, fs.ErrNotExist
				}
				return data, nil
			},
			write: func(string, []byte, fs.FileMode) error {
				return errors.New("kubeconfig wrote a file outside the store")
			},
			out: io.Discard,
		},
		name:   "gt-deployer-apply",
		dir:    deployerTestDir,
		server: kubeconfigTestServer,
		ca:     kubeconfigTestCAPath,
		cert:   kubeconfigTestCertPath,
	}
}

// newKubeconfigFiles is the --ca and --cert input a test's environment reads.
func newKubeconfigFiles(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		kubeconfigTestCAPath:   deployerTestCertificate(t, "gt deployer test CA"),
		kubeconfigTestCertPath: deployerTestCertificate(t, "gt-deployer-apply"),
	}
}

// kubeconfigDoc is the stored kubeconfig as kubectl's schema reads it, decoded
// independently of the package's own struct.
type kubeconfigDoc struct {
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

func TestTownDeployerKubeconfigWrites(t *testing.T) {
	t.Parallel()

	t.Run("stores the certificate and names the key by path", func(t *testing.T) {
		t.Parallel()
		fsys := newFakeDeployerFS(1000)
		fsys.entries[deployerTestDir] = deployerEntry{mode: fs.ModeDir | 0o700, owner: 1000}
		keyPath := filepath.Join(deployerTestDir, "gt-deployer-apply.key")
		fsys.entries[keyPath] = deployerEntry{mode: 0o600, owner: 1000, data: []byte("the key bytes")}
		files := newKubeconfigFiles(t)

		if err := runTownDeployerKubeconfigIn(newKubeconfigEnv(fsys, files)); err != nil {
			t.Fatalf("runTownDeployerKubeconfigIn = %v", err)
		}

		certPath := filepath.Join(deployerTestDir, "gt-deployer-apply.crt")
		stored, ok := fsys.entries[certPath]
		if !ok {
			t.Fatalf("the certificate was not stored at %s", certPath)
		}
		if stored.mode != 0o600 || stored.owner != 1000 {
			t.Errorf("the certificate holds mode %04o owned by uid %d, want 0600 owned by 1000", stored.mode, stored.owner)
		}
		if !bytes.Equal(stored.data, files[kubeconfigTestCertPath]) {
			t.Error("the stored certificate is not the bytes --cert named")
		}

		kubeconfigPath := filepath.Join(deployerTestDir, "gt-deployer-apply.kubeconfig")
		kube, ok := fsys.entries[kubeconfigPath]
		if !ok {
			t.Fatalf("the kubeconfig was not stored at %s", kubeconfigPath)
		}
		if kube.mode != 0o600 || kube.owner != 1000 {
			t.Errorf("the kubeconfig holds mode %04o owned by uid %d, want 0600 owned by 1000", kube.mode, kube.owner)
		}

		var doc kubeconfigDoc
		if err := yaml.Unmarshal(kube.data, &doc); err != nil {
			t.Fatalf("the stored kubeconfig does not parse: %v", err)
		}
		if doc.APIVersion != "v1" || doc.Kind != "Config" {
			t.Fatalf("the kubeconfig is %s %s, want v1 Config", doc.APIVersion, doc.Kind)
		}
		if len(doc.Users) != 1 {
			t.Fatalf("the kubeconfig holds %d users, want one", len(doc.Users))
		}
		user := doc.Users[0].User
		if user.ClientKey != keyPath {
			t.Errorf("the user references the key at %q, want %s", user.ClientKey, keyPath)
		}
		if user.ClientCertificate != certPath {
			t.Errorf("the user references the certificate at %q, want %s", user.ClientCertificate, certPath)
		}
		if user.ClientKeyData != "" || user.ClientCertificateData != "" {
			t.Error("the kubeconfig embeds the key or the certificate instead of naming its path")
		}
		if doc.CurrentContext != "gt-deployer-apply" {
			t.Errorf("the current context is %q, want gt-deployer-apply", doc.CurrentContext)
		}
		if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != kubeconfigTestServer {
			t.Fatalf("the cluster is %+v, want %s", doc.Clusters, kubeconfigTestServer)
		}
		ca, err := base64.StdEncoding.DecodeString(doc.Clusters[0].Cluster.CertificateAuthorityData)
		if err != nil || !bytes.Equal(ca, files[kubeconfigTestCAPath]) {
			t.Errorf("the embedded CA is not the --ca certificate: %v", err)
		}
	})

	t.Run("refuses the admin context name and writes nothing", func(t *testing.T) {
		t.Parallel()
		fsys := newFakeDeployerFS(1000)
		fsys.entries[deployerTestDir] = deployerEntry{mode: fs.ModeDir | 0o700, owner: 1000}
		env := newKubeconfigEnv(fsys, newKubeconfigFiles(t))
		env.name = "colima-gt-control"
		before := fsys.paths()

		err := runTownDeployerKubeconfigIn(env)
		if err == nil || !strings.Contains(err.Error(), "admin context") {
			t.Fatalf("runTownDeployerKubeconfigIn = %v, want a refusal naming the admin context", err)
		}
		if after := fsys.paths(); after != before {
			t.Errorf("the refused run wrote files: %s", after)
		}
	})
}

func TestTownDeployerKubeconfigRefuses(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name string
		// edit adjusts the environment, the files --ca and --cert are read
		// from, or the filesystem before the command runs.
		edit func(env *townDeployerKubeconfigEnv, files map[string][]byte, fsys *fakeDeployerFS)
		want string
	}
	cases := []testCase{
		{
			name: "inside a pod",
			edit: func(env *townDeployerKubeconfigEnv, _ map[string][]byte, _ *fakeDeployerFS) {
				env.getenv = func(string) string { return "10.0.0.1" }
			},
			want: "deployer user only",
		},
		{
			name: "another running user",
			edit: func(env *townDeployerKubeconfigEnv, _ map[string][]byte, _ *fakeDeployerFS) {
				env.current = deployerUser("alice", "1001")
			},
			want: "not --as-user",
		},
		{
			name: "no --as-user",
			edit: func(env *townDeployerKubeconfigEnv, _ map[string][]byte, _ *fakeDeployerFS) {
				env.asUser = ""
			},
			want: "needs --as-user",
		},
		{
			name: "--ca cannot be read",
			edit: func(_ *townDeployerKubeconfigEnv, files map[string][]byte, _ *fakeDeployerFS) {
				delete(files, kubeconfigTestCAPath)
			},
			want: "reading --ca " + kubeconfigTestCAPath,
		},
		{
			name: "--cert cannot be read",
			edit: func(_ *townDeployerKubeconfigEnv, files map[string][]byte, _ *fakeDeployerFS) {
				delete(files, kubeconfigTestCertPath)
			},
			want: "reading --cert " + kubeconfigTestCertPath,
		},
		{
			name: "--cert holds no certificate",
			edit: func(_ *townDeployerKubeconfigEnv, files map[string][]byte, _ *fakeDeployerFS) {
				files[kubeconfigTestCertPath] = []byte("not a certificate")
			},
			want: "holds no CERTIFICATE PEM block",
		},
		{
			name: "--cert holds a block that does not parse",
			edit: func(_ *townDeployerKubeconfigEnv, files map[string][]byte, _ *fakeDeployerFS) {
				files[kubeconfigTestCertPath] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})
			},
			want: "CERTIFICATE block that does not parse",
		},
		{
			name: "the CA does not parse",
			edit: func(_ *townDeployerKubeconfigEnv, files map[string][]byte, _ *fakeDeployerFS) {
				files[kubeconfigTestCAPath] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})
			},
			want: "building the gt-deployer-apply kubeconfig",
		},
		{
			name: "a server that is not loopback",
			edit: func(env *townDeployerKubeconfigEnv, _ map[string][]byte, _ *fakeDeployerFS) {
				env.server = "https://10.0.0.1:6443"
			},
			want: "not a loopback address",
		},
		{
			name: "the directory fails the store's check",
			edit: func(_ *townDeployerKubeconfigEnv, _ map[string][]byte, fsys *fakeDeployerFS) {
				fsys.entries[deployerTestDir] = deployerEntry{mode: fs.ModeDir | 0o755, owner: 1000}
			},
			want: "writing the gt-deployer-apply certificate and kubeconfig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeDeployerFS(1000)
			fsys.entries[deployerTestDir] = deployerEntry{mode: fs.ModeDir | 0o700, owner: 1000}
			files := newKubeconfigFiles(t)
			env := newKubeconfigEnv(fsys, files)
			tc.edit(&env, files, fsys)
			before := fsys.paths()

			err := runTownDeployerKubeconfigIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownDeployerKubeconfigIn = %v, want a refusal naming %q", err, tc.want)
			}
			if after := fsys.paths(); after != before {
				t.Errorf("the refused run wrote files: %s", after)
			}
		})
	}
}

// TestTownDeployerKubeconfigFlagReadsRefuseAnUndefinedFlag keeps every flag
// read in runTownDeployerKubeconfig covered, so a flag that stops being
// registered fails here.
func TestTownDeployerKubeconfigFlagReadsRefuseAnUndefinedFlag(t *testing.T) {
	t.Parallel()
	flags := []string{"as-user", "name", "dir", "server", "ca", "cert"}
	for i := range flags {
		defined := flags[:i]
		want := "reading --" + flags[i]
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, f := range defined {
				cmd.Flags().String(f, "", "")
			}
			err := runTownDeployerKubeconfig(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("runTownDeployerKubeconfig = %v, want a refusal naming %q", err, want)
			}
		})
	}
}

// TestTownDeployerKubeconfigFlagsRegistered proves the command the binary runs
// holds every flag the run function reads.
func TestTownDeployerKubeconfigFlagsRegistered(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"as-user", "name", "dir", "server", "ca", "cert"} {
		if townDeployerKubeconfigCmd.Flags().Lookup(name) == nil {
			t.Errorf("gt town deployer kubeconfig has no --%s flag", name)
		}
	}
	found, _, err := townCmd.Find([]string{"deployer", "kubeconfig"})
	if err != nil || found != townDeployerKubeconfigCmd {
		t.Errorf("gt town Find(deployer kubeconfig) = %v, %v; want the kubeconfig command", found, err)
	}
}
