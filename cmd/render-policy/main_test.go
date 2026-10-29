package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/k8spolicy"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The command runs in this test process; the hermetic harness keeps the live
// town's HOME and GT_* environment out of reach.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// dockerPolicy is the shape of deploy/polecat/policy.yaml; this slice only has
// to read the file.
const dockerPolicy = `network_policies:
  sandbox:
    endpoints:
      - host: host.docker.internal
        port: 3307
`

// checkout is a repository-shaped tree a test reads through: a go.mod at its
// root, and withPolicy the Docker policy where the default BASE looks for it.
type checkout struct{ root string }

func newCheckout(t *testing.T) checkout {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/checkout\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return checkout{root: root}
}

func (c checkout) withPolicy(t *testing.T) {
	t.Helper()
	path := filepath.Join(c.root, filepath.FromSlash(k8spolicy.DefaultBasePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(dockerPolicy), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// defaultBase is where the command looks for the policy when the command line
// names none.
func (c checkout) defaultBase() string {
	return filepath.Join(c.root, filepath.FromSlash(k8spolicy.DefaultBasePath))
}

// host reads the checkout through the production seams from wd: a test names a
// subdirectory to prove the default BASE hangs off the repository root.
func (c checkout) host(wd string) k8spolicy.Host {
	return k8spolicy.Host{
		Getwd:    func() (string, error) { return wd, nil },
		Stat:     os.Stat,
		ReadFile: os.ReadFile,
	}
}

// runFrom runs the command over the checkout from wd and returns its exit code
// and its whole error stream. The command has one stream in this slice, so a
// refusal cannot reach stdout; the render slice adds the stream a successful
// run writes.
func (c checkout) runFrom(t *testing.T, wd string, args ...string) (int, string) {
	t.Helper()
	var errw bytes.Buffer
	code := run(args, &errw, c.host(wd))
	return code, errw.String()
}

// TestMainErrors: every refusal, with the exit code and the single line it
// prints. The three order rows pin the script's order of checks: BASE, then the
// pinned address, then the dolt host, then the forgejo host.
func TestMainErrors(t *testing.T) {
	t.Parallel()
	missing := newCheckout(t)
	present := newCheckout(t)
	present.withPolicy(t)
	subdir := filepath.Join(present.root, "cmd", "render-policy")
	namedMissing := filepath.Join(missing.root, "nope.yaml")

	cases := []struct {
		name     string
		c        checkout
		wd       string
		args     []string
		wantCode int
		wantErr  string
	}{
		{
			name: "a missing BASE exits 1 naming the file",
			c:    missing, wd: missing.root,
			wantCode: 1,
			wantErr:  "render-policy: cannot read " + missing.defaultBase(),
		},
		{
			name: "a named BASE that is missing exits 1 naming it",
			c:    missing, wd: missing.root, args: []string{namedMissing},
			wantCode: 1,
			wantErr:  "render-policy: cannot read " + namedMissing,
		},
		{
			name: "a bad address exits 2 naming the flag and the value",
			c:    present, wd: subdir, args: []string{"--allowed-ip", "1.2.3"},
			wantCode: 2,
			wantErr:  "render-policy: --allowed-ip must be an IPv4 address, got '1.2.3'",
		},
		{
			name: "the injection string is refused",
			c:    present, wd: subdir, args: []string{"--allowed-ip", `1.2.3.4/32"]; evil`},
			wantCode: 2,
			wantErr:  `render-policy: --allowed-ip must be an IPv4 address, got '1.2.3.4/32"]; evil'`,
		},
		{
			name: "a bad dolt host exits 2 naming the flag and the value",
			c:    present, wd: subdir, args: []string{"--dolt-host", "dolt example"},
			wantCode: 2,
			wantErr:  "render-policy: --dolt-host must be a DNS name, got 'dolt example'",
		},
		{
			name: "a bad forgejo host exits 2 naming the flag and the value",
			c:    present, wd: subdir, args: []string{"--forgejo-host", "-bad.example"},
			wantCode: 2,
			wantErr:  "render-policy: --forgejo-host must be a DNS name, got '-bad.example'",
		},
		{
			name: "BASE is checked before the address",
			c:    missing, wd: missing.root,
			args:     []string{"--allowed-ip", "1.2.3", "--dolt-host", "bad host", "--forgejo-host", "bad host"},
			wantCode: 1,
			wantErr:  "render-policy: cannot read " + missing.defaultBase(),
		},
		{
			name: "the address is checked before the dolt host",
			c:    present, wd: subdir,
			args:     []string{"--allowed-ip", "1.2.3", "--dolt-host", "bad host", "--forgejo-host", "bad host"},
			wantCode: 2,
			wantErr:  "render-policy: --allowed-ip must be an IPv4 address, got '1.2.3'",
		},
		{
			name: "the dolt host is checked before the forgejo host",
			c:    present, wd: subdir,
			args:     []string{"--dolt-host", "bad host", "--forgejo-host", "bad host"},
			wantCode: 2,
			wantErr:  "render-policy: --dolt-host must be a DNS name, got 'bad host'",
		},
		{
			name: "an unknown option exits 2",
			c:    present, wd: subdir, args: []string{"--bogus"},
			wantCode: 2,
			wantErr:  "render-policy: unknown option --bogus",
		},
		{
			name: "a flag with no value exits 2",
			c:    present, wd: subdir, args: []string{"--allowed-ip"},
			wantCode: 2,
			wantErr:  "render-policy: --allowed-ip needs a value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, errText := tc.c.runFrom(t, tc.wd, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tc.wantCode, errText)
			}
			if got := strings.TrimSpace(errText); got != tc.wantErr {
				t.Errorf("stderr = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

// TestMainStagedRunRefuses: a command line that passes every check must not
// exit 0 as though it had written a policy. The checkout has the policy at its
// root only, so reading it proves the default BASE was resolved from the
// checkout root of a run started below it.
func TestMainStagedRunRefuses(t *testing.T) {
	t.Parallel()
	c := newCheckout(t)
	c.withPolicy(t)
	code, errText := c.runFrom(t, filepath.Join(c.root, "cmd", "render-policy"))
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, errText)
	}
	if got := strings.TrimSpace(errText); got != notImplemented {
		t.Errorf("stderr = %q, want %q", got, notImplemented)
	}
}

// TestMainWithoutARepositoryRoot: outside any checkout the default BASE has no
// anchor, and the command says so rather than reading some other file.
func TestMainWithoutARepositoryRoot(t *testing.T) {
	t.Parallel()
	host := k8spolicy.Host{
		Getwd:    func() (string, error) { return "/nowhere/deep", nil },
		Stat:     func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
		ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
	}
	var errw bytes.Buffer
	if code := run(nil, &errw, host); code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, errw.String())
	}
	want := "render-policy: no go.mod in /nowhere/deep or any parent, so the default BASE cannot be found"
	if got := strings.TrimSpace(errw.String()); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}
