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

// dockerPolicy is a stored Docker policy: a header that names the Docker route
// and the three endpoints the render rewrites, plus one it leaves alone.
const dockerPolicy = `# Sandbox policy. Dolt on :3307 and Forgejo on :3000 are reached through Docker Desktop's host alias.
network_policies:
  forgejo:
    endpoints:
      - host: host.docker.internal
        port: 3000
  dolt:
    endpoints:
      - host: host.docker.internal
        port: 3307
  tracker:
    endpoints:
      - host: host.docker.internal
        port: 7433
  other:
    endpoints:
      - host: host.docker.internal
        port: 9999
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

// runFrom runs the command over the checkout from wd and returns its exit code,
// what it wrote to stdout and its whole error stream.
func (c checkout) runFrom(t *testing.T, wd string, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(args, &out, &errw, c.host(wd))
	return code, out.String(), errw.String()
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
		{
			name: "a tracker host with a space exits 2 and names the flag",
			c:    present, wd: subdir, args: []string{"--tracker-host", "track er.example"},
			wantCode: 2,
			wantErr:  "render-policy: --tracker-host must be a DNS name, got 'track er.example'",
		},
		{
			name: "the tracker host is checked after the forgejo host",
			c:    present, wd: subdir, args: []string{"--forgejo-host", "bad host", "--tracker-host", "bad host"},
			wantCode: 2,
			wantErr:  "render-policy: --forgejo-host must be a DNS name, got 'bad host'",
		},
		{
			name: "a tracker flag with no value exits 2",
			c:    present, wd: subdir, args: []string{"--tracker-host"},
			wantCode: 2,
			wantErr:  "render-policy: --tracker-host needs a value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, outText, errText := tc.c.runFrom(t, tc.wd, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tc.wantCode, errText)
			}
			if outText != "" {
				t.Errorf("a refused run wrote to stdout: %q", outText)
			}
			if got := strings.TrimSpace(errText); got != tc.wantErr {
				t.Errorf("stderr = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

// goldenRender is dockerPolicy rendered with the defaults: the header names the
// route, each of the three endpoints is renamed and pinned, the one on port 9999
// is left alone.
const goldenRender = `# Sandbox policy. Dolt on :3307 and Forgejo on :3000 are reached through in-cluster Services.
network_policies:
  forgejo:
    endpoints:
      - host: control-plane.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 3000
  dolt:
    endpoints:
      - host: control-plane.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 3307
  tracker:
    endpoints:
      - host: tracker.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 7433
  other:
    endpoints:
      - host: host.docker.internal
        port: 9999
`

// TestMainDefaultRenderMatchesTable: a run that passes every check writes the
// rendered policy to stdout and nothing to stderr and exits 0: the header names
// the route, each of the three endpoints is renamed and pinned, the one on port
// 9999 is left alone. The default BASE is found from the checkout root of a run
// started below it.
func TestMainDefaultRenderMatchesTable(t *testing.T) {
	t.Parallel()
	c := newCheckout(t)
	c.withPolicy(t)
	code, out, errText := c.runFrom(t, filepath.Join(c.root, "cmd", "render-policy"))
	if code != 0 || errText != "" {
		t.Fatalf("exit code = %d, stderr %q, want 0 and nothing", code, errText)
	}
	if out != goldenRender {
		t.Errorf("stdout =\n%s\nwant\n%s", out, goldenRender)
	}
}

// TestMainOnlyReadsBase: the Docker policy's bytes and mode are the same after a
// run, for a render and for a refusal.
func TestMainOnlyReadsBase(t *testing.T) {
	t.Parallel()
	c := newCheckout(t)
	c.withPolicy(t)
	snapshot := func() (string, os.FileMode) {
		t.Helper()
		data, err := os.ReadFile(c.defaultBase())
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(c.defaultBase())
		if err != nil {
			t.Fatal(err)
		}
		return string(data), info.Mode()
	}
	beforeData, beforeMode := snapshot()
	for _, args := range [][]string{nil, {"--allowed-ip", "1.2.3"}} {
		c.runFrom(t, c.root, args...)
		if data, mode := snapshot(); data != beforeData || mode != beforeMode {
			t.Errorf("the Docker policy changed after a run with %v", args)
		}
	}
}

// TestMainSeparateNamesAndBadInput groups the two sections of the old shell test
// that drive flags: the names land on their own ports, and a value the script
// would not render is refused.
func TestMainSeparateNamesAndBadInput(t *testing.T) {
	t.Parallel()
	t.Run("separate names", separateNamesLandOnTheirPorts)
	t.Run("bad input", rejectedValuesRenderNothing)
}

// separateNamesLandOnTheirPorts: each flag's name lands on its own port and never
// on another, and the pin is the flag's address on all three.
func separateNamesLandOnTheirPorts(t *testing.T) {
	t.Parallel()
	c := newCheckout(t)
	c.withPolicy(t)
	code, out, errText := c.runFrom(t, c.root, "--dolt-host", "dolt.example", "--forgejo-host", "forgejo.example",
		"--tracker-host", "tracker.example", "--allowed-ip", "10.0.0.9")
	if code != 0 || errText != "" {
		t.Fatalf("exit code = %d, stderr %q", code, errText)
	}
	for _, port := range []struct{ port, want string }{{"3307", "dolt.example"}, {"3000", "forgejo.example"}, {"7433", "tracker.example"}} {
		ep := endpointLines(t, out, port.port)
		if !strings.Contains(ep, "host: "+port.want+"\n") {
			t.Errorf("port %s endpoint lacks %s:\n%s", port.port, port.want, ep)
		}
		for _, other := range []string{"dolt.example", "forgejo.example", "tracker.example"} {
			if other != port.want && strings.Contains(ep, other) {
				t.Errorf("port %s endpoint carries %s:\n%s", port.port, other, ep)
			}
		}
	}
	if got := strings.Count(out, `allowed_ips: ["10.0.0.9/32"]`); got != 3 {
		t.Errorf("--allowed-ip is pinned %d times, want 3", got)
	}
}

// endpointLines returns the host, pin and port lines of the endpoint on port.
func endpointLines(t *testing.T, out, port string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "port: "+port && i >= 2 {
			return strings.Join(lines[i-2:i+1], "\n") + "\n"
		}
	}
	t.Fatalf("no endpoint on port %s in:\n%s", port, out)
	return ""
}

// rejectedValuesRenderNothing: each value the script would not render is refused
// with exit 2 and nothing on stdout, and names its flag.
func rejectedValuesRenderNothing(t *testing.T) {
	t.Parallel()
	c := newCheckout(t)
	c.withPolicy(t)
	cases := []struct {
		name string
		args []string
		flag string
	}{
		{"an injection in the address", []string{"--allowed-ip", `1.2.3.4/32"]; evil`}, "--allowed-ip"},
		{"too few octets", []string{"--allowed-ip", "1.2.3"}, "--allowed-ip"},
		{"too many octets", []string{"--allowed-ip", "1.2.3.4.5"}, "--allowed-ip"},
		{"an octet over 255", []string{"--allowed-ip", "256.1.1.1"}, "--allowed-ip"},
		{"a letter in the address", []string{"--allowed-ip", "1.2.3.4x"}, "--allowed-ip"},
		{"a trailing dot (departure 1)", []string{"--allowed-ip", "1.2.3.4."}, "--allowed-ip"},
		{"a dolt host with a space", []string{"--dolt-host", "dolt example"}, "--dolt-host"},
		{"an empty forgejo host", []string{"--forgejo-host", ""}, "--forgejo-host"},
		{"a label starting with a hyphen", []string{"--forgejo-host", "-bad.example"}, "--forgejo-host"},
		{"an empty label", []string{"--dolt-host", "bad..example"}, "--dolt-host"},
		{"a tracker host with a space", []string{"--tracker-host", "track er.example"}, "--tracker-host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, out, errText := c.runFrom(t, c.root, tc.args...)
			if code != 2 || out != "" || !strings.Contains(errText, tc.flag) {
				t.Errorf("exit %d, stdout %q, stderr %q, want exit 2, nothing rendered and %s named", code, out, errText, tc.flag)
			}
		})
	}
}

// TestMainLeavesOtherEndpointsAlone: through the command, an endpoint on another
// port keeps its host and gets no pin, and an unrelated host is untouched.
func TestMainLeavesOtherEndpointsAlone(t *testing.T) {
	t.Parallel()
	c := newCheckout(t)
	path := filepath.Join(c.root, "other.yaml")
	in := "network_policies:\n  x:\n    endpoints:\n      - host: host.docker.internal\n        port: 9999\n      - host: proxy.golang.org\n        port: 443\n      - host: host.docker.internal\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errText := c.runFrom(t, c.root, path)
	if code != 0 || errText != "" || out != in {
		t.Errorf("exit %d, stderr %q, stdout %q, want the input unchanged", code, errText, out)
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
	var out, errw bytes.Buffer
	if code := run(nil, &out, &errw, host); code != 1 || out.String() != "" {
		t.Errorf("exit code = %d, stdout %q, want 1 and nothing (stderr %q)", code, out.String(), errw.String())
	}
	want := "render-policy: no go.mod in /nowhere/deep or any parent, so the default BASE cannot be found"
	if got := strings.TrimSpace(errw.String()); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}
