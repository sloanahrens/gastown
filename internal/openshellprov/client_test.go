package openshellprov

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// fakeRunner records each call and answers every call with the same scripted
// result, so no process starts.
type fakeRunner struct {
	calls  []string
	stdout []byte
	stderr []byte
	err    error
}

func (f *fakeRunner) Run(name string, args []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	return f.stdout, f.stderr, f.err
}

// An empty command has no program to start, so Run fails before any process
// exists; the runner's failure return is reached without spawning anything.
func TestExecRunnerReturnsAStartFailure(t *testing.T) {
	t.Parallel()
	runner := ExecRunner{Command: func(string, ...string) *exec.Cmd { return &exec.Cmd{} }}
	stdout, stderr, err := runner.Run("openshell", []string{"provider", "list"}, nil, nil)
	if err == nil {
		t.Fatal("Run of an unstartable command = nil error, want the start failure")
	}
	if len(stdout) != 0 || len(stderr) != 0 {
		t.Errorf("output = %q / %q after a start failure, want none", stdout, stderr)
	}
}

// failingWriter refuses every write, so the export's copy-out error is reached.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestClientPutsTheGatewayFlagBeforeTheSubcommand(t *testing.T) {
	t.Parallel()
	calls := []struct {
		name string
		run  func(c Client) error
		sub  string
	}{
		{"ProviderExists", func(c Client) error { c.ProviderExists("deepseek"); return nil }, "provider list"},
		{"ProfileExists", func(c Client) error { _, err := c.ProfileExists("forgejo-polecat"); return err }, "profile list --global"},
		{"ExportProfile", func(c Client) error { return c.ExportProfile("forgejo-polecat", io.Discard) }, "profile export --global forgejo-polecat"},
	}
	for _, tc := range calls {
		t.Run(tc.name+" with a gateway", func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{}
			if err := tc.run(Client{Run: fake, Gateway: "k8s"}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if want := "openshell -g k8s " + tc.sub; len(fake.calls) != 1 || fake.calls[0] != want {
				t.Errorf("calls = %q, want [%q]", fake.calls, want)
			}
		})
		t.Run(tc.name+" without a gateway", func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{}
			if err := tc.run(Client{Run: fake}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if want := "openshell " + tc.sub; len(fake.calls) != 1 || fake.calls[0] != want {
				t.Errorf("calls = %q, want [%q]", fake.calls, want)
			}
		})
	}
}

func TestProviderExistsFirstColumnOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		listing string
		want    bool
	}{
		{name: "the name in the first column", listing: "NAME TYPE\ndeepseek deepseek-claude\n", want: true},
		{name: "a longer name that starts with it", listing: "NAME TYPE\ndeepseek-legacy deepseek-claude\n", want: false},
		{name: "the name in a later column", listing: "NAME TYPE\nother deepseek\n", want: false},
		{name: "a substring of the name", listing: "NAME TYPE\nxdeepseek deepseek-claude\n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{stdout: []byte(tc.listing)}
			if got := (Client{Run: fake}).ProviderExists("deepseek"); got != tc.want {
				t.Errorf("ProviderExists(deepseek) over %q = %v, want %v", tc.listing, got, tc.want)
			}
		})
	}
}

// A failed provider list must read as absent even when its partial output names
// the provider, as the script's `|| true` does.
func TestProviderListFailureReadsAsAbsent(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{stdout: []byte("deepseek deepseek-claude\n"), err: errors.New("gateway down")}
	if (Client{Run: fake}).ProviderExists("deepseek") {
		t.Fatal("ProviderExists(deepseek) = true after a failed list, want false")
	}
}

// A failed profile list is fatal and carries the captured output, so a clash
// later is not what the operator sees. It must never read as absent.
func TestProfileListFailureIsFatalWithOutput(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{stderr: []byte("gateway unreachable\n"), err: errors.New("exit 1")}
	exists, err := (Client{Run: fake}).ProfileExists("forgejo-polecat")
	if err == nil {
		t.Fatalf("ProfileExists after a failed list = (%v, nil), want an error", exists)
	}
	for _, want := range []string{
		"openshell profile list --global failed, so whether profile forgejo-polecat exists is unknown:",
		"gateway unreachable",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if exists {
		t.Error("ProfileExists after a failed list reported the profile as present")
	}
}

func TestProfileExistsReadsTheListing(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{stdout: []byte("NAME SCOPE\nforgejo-polecat global\n")}
	exists, err := (Client{Run: fake}).ProfileExists("forgejo-polecat")
	if err != nil || !exists {
		t.Fatalf("ProfileExists(forgejo-polecat) = (%v, %v), want (true, nil)", exists, err)
	}
}

func TestExportProfileWritesStdoutAndFailsLoudly(t *testing.T) {
	t.Parallel()
	t.Run("success writes the profile to out", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{stdout: []byte("id: forgejo-polecat\n")}
		var out bytes.Buffer
		if err := (Client{Run: fake}).ExportProfile("forgejo-polecat", &out); err != nil {
			t.Fatalf("ExportProfile: %v", err)
		}
		if got := out.String(); got != "id: forgejo-polecat\n" {
			t.Errorf("out = %q, want the profile's stdout", got)
		}
	})
	t.Run("failure names the export and writes nothing", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{stdout: []byte("partial"), stderr: []byte("no such profile\n"), err: errors.New("exit 1")}
		var out bytes.Buffer
		err := (Client{Run: fake}).ExportProfile("forgejo-polecat", &out)
		if err == nil {
			t.Fatal("ExportProfile after a failed export = nil, want an error")
		}
		for _, want := range []string{
			"openshell profile export --global forgejo-polecat failed, so whether it binds the gateway's endpoint is unknown:",
			"no such profile",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
		if out.Len() != 0 {
			t.Errorf("out = %q after a failed export, want nothing written", out.String())
		}
	})
	t.Run("a refused write is an error", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{stdout: []byte("id: forgejo-polecat\n")}
		err := (Client{Run: fake}).ExportProfile("forgejo-polecat", failingWriter{})
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("ExportProfile with a refused write = %v, want the write error", err)
		}
	})
}
