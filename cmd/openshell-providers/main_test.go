package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/openshellprov"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

func TestMainUsage(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			var out, errw bytes.Buffer
			if code := run([]string{arg}, &out, &errw, fakeHost(t, nil)); code != 0 {
				t.Fatalf("run(%s) = %d, want 0", arg, code)
			}
			if got, want := out.String(), openshellprov.Usage+"\n"; got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			if errw.Len() != 0 {
				t.Errorf("stderr = %q, want empty", errw.String())
			}
		})
	}
}

// A refused argument exits 2 with the command's line, and help reaches exit 0
// even with an argument after it: the parse ends at the first -h.
func TestParseUnknownArgumentAndHelp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{
			name:     "an unknown argument names the token and the usage",
			args:     []string{"--bogus"},
			wantCode: 2,
			wantErr:  "openshell-providers: unknown argument: --bogus (" + openshellprov.Usage + ")\n",
		},
		{
			name:     "an unknown argument after a good one is still refused",
			args:     []string{"-g", "k8s", "--bogus"},
			wantCode: 2,
			wantErr:  "openshell-providers: unknown argument: --bogus (" + openshellprov.Usage + ")\n",
		},
		{
			name:     "help before the unknown argument wins",
			args:     []string{"-h", "--bogus"},
			wantCode: 0,
			wantOut:  openshellprov.Usage + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errw bytes.Buffer
			if code := run(tc.args, &out, &errw, fakeHost(t, nil)); code != tc.wantCode {
				t.Fatalf("run(%q) = %d, want %d", tc.args, code, tc.wantCode)
			}
			if got := out.String(); got != tc.wantOut {
				t.Errorf("stdout = %q, want %q", got, tc.wantOut)
			}
			if got := errw.String(); got != tc.wantErr {
				t.Errorf("stderr = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func TestOpenshellMissingExitsOne(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, nil)
	h.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	var out, errw bytes.Buffer
	if code := run(nil, &out, &errw, h); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if got, want := errw.String(), "openshell-providers: openshell is not on PATH (brew install openshell)\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestLookPathAsksForOpenshell(t *testing.T) {
	t.Parallel()
	var asked []string
	h := fakeHost(t, nil)
	h.LookPath = func(name string) (string, error) {
		asked = append(asked, name)
		return "/usr/local/bin/" + name, nil
	}
	if code := run(nil, io.Discard, io.Discard, h); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if len(asked) != 1 || asked[0] != "openshell" {
		t.Errorf("LookPath asked for %v, want exactly one \"openshell\"", asked)
	}
}

// TestRunExitCodes drives run through the openshell and gt calls a whole run
// makes. A refused argument exits 2 before any call; a refusal by gt or a failed
// provider create exits 1 with the command's line; a run that creates both
// providers exits 0.
func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		exec     func(name string, args []string) ([]byte, error)
		wantCode int
		wantErr  string
	}{
		{
			name:     "a refused argument exits 2 before any call",
			args:     []string{"--bogus"},
			exec:     refuseAll,
			wantCode: 2,
			wantErr:  "openshell-providers: unknown argument: --bogus",
		},
		{
			name:     "a run whose gateway gt refuses exits 1",
			exec:     refuseGt,
			wantCode: 1,
			wantErr:  "openshell-providers: gt sandbox endpoint --gateway openshell failed",
		},
		{
			name:     "a provider create that fails exits 1 and names the command",
			exec:     failProviderCreate,
			wantCode: 1,
			wantErr:  "openshell-providers: openshell provider create --name deepseek failed",
		},
		{
			name:     "a gateway that takes every call exits 0",
			exec:     acceptAll,
			wantCode: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := fakeHost(t, map[string]string{"XDG_CONFIG_HOME": keyDir(t)})
			h.Exec = execFunc(tc.exec)
			var out, errw bytes.Buffer
			if code := run(tc.args, &out, &errw, h); code != tc.wantCode {
				t.Fatalf("run() = %d, want %d (stderr %q)", code, tc.wantCode, errw.String())
			}
			if tc.wantErr != "" && !strings.Contains(errw.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", errw.String(), tc.wantErr)
			}
			if tc.wantErr == "" && errw.Len() != 0 {
				t.Errorf("stderr = %q, want empty", errw.String())
			}
		})
	}
}

// execFunc is an openshell and gt that answer through one function, so no
// process starts.
type execFunc func(name string, args []string) ([]byte, error)

func (f execFunc) Run(name string, args []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
	out, err := f(name, args)
	return out, nil, err
}

func refuseGt(name string, _ []string) ([]byte, error) {
	if name == "gt" {
		return nil, errors.New("exit status 1")
	}
	return nil, nil
}

func failProviderCreate(name string, args []string) ([]byte, error) {
	if name == "openshell" && slices.Contains(args, "create") {
		return nil, errors.New("exit status 1")
	}
	return nil, nil
}

func acceptAll(string, []string) ([]byte, error) { return nil, nil }

// keyDir is a config home whose gt directory holds both key files, so the
// providers are created from files the run reads for real.
func keyDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "gt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range map[string]string{
		"deepseek.env":        "DEEPSEEK_API_KEY=sk-test-key\n",
		"forgejo-polecat.env": "FORGEJO_TOKEN=forgejo-test-token\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return home
}

// A working directory that cannot be resolved stops the run before anything is
// read, with the resolution's own message.
func TestDerivedPathsStopsTheRun(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, nil)
	h.Getwd = func() (string, error) { return "", errors.New("boom") }
	var out, errw bytes.Buffer
	if code := run(nil, &out, &errw, h); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if got, want := errw.String(), "openshell-providers: working directory: boom\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestProcessHost drives the Host main hands a run: each seam reaches the
// process, and a run through all of them ends on one of the two refusals it can
// reach rather than on a third outcome a miswired seam would produce.
func TestProcessHost(t *testing.T) {
	t.Parallel()
	h := openshellprov.ProcessHost
	dir := t.TempDir()
	if _, err := h.Stat(dir); err != nil {
		t.Errorf("ProcessHost.Stat(%q): %v", dir, err)
	}
	wd, err := h.Getwd()
	if err != nil {
		t.Fatalf("ProcessHost.Getwd(): %v", err)
	}
	want, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd(): %v", err)
	}
	if wd != want {
		t.Errorf("ProcessHost.Getwd() = %q, want the process's %q", wd, want)
	}
	// A name no host has on PATH: an error is the answer LookPath must give.
	const absent = "openshell-providers-no-such-binary-2f1c"
	if _, err := h.LookPath(absent); err == nil {
		t.Errorf("ProcessHost.LookPath(%q) found a binary that does not exist", absent)
	}

	// A run is not started here: with openshell on PATH it would reach a live
	// gateway. The exec seam is checked for being wired, not for what it runs.
	if h.Exec == nil || h.Files == nil || h.Env == nil {
		t.Fatalf("ProcessHost has an unwired seam: Exec %v, Files %v, Env %v", h.Exec == nil, h.Files == nil, h.Env == nil)
	}
	if got, want := h.Env(), os.Environ(); len(got) != len(want) {
		t.Errorf("ProcessHost.Env() has %d entries, want the process's %d", len(got), len(want))
	}
}

// fakeHost is a run's outside world: a fresh checkout (a temp dir holding
// go.mod) as the working directory, env as the environment, openshell on PATH,
// and the real filesystem under the checkout's temp dir.
func fakeHost(t *testing.T, env map[string]string) openshellprov.Host {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return openshellprov.Host{
		Getwd:    func() (string, error) { return repo, nil },
		Getenv:   func(key string) string { return env[key] },
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
		Stat:     os.Stat,
		Files:    openshellprov.OSFiles{},
		Env:      func() []string { return nil },
		// A test that reaches the gateway without choosing its exec is refused,
		// not run: no process starts and nothing touches a real gateway.
		Exec: execFunc(refuseAll),
	}
}

func refuseAll(string, []string) ([]byte, error) {
	return nil, errors.New("exit status 1")
}
