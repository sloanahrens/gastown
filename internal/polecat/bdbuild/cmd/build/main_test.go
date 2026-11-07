package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

const (
	hostCommit  = "3b90ab0c9d8e7f6a5b4c3d2e1f00112233445566"
	hostBuildID = "3b90ab0c"
	otherCommit = "9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// fakeInstalled answers the installed bd's version report with info and err,
// and counts the reads.
type fakeInstalled struct {
	info  beads.VersionInfo
	err   error
	reads int
}

func (f *fakeInstalled) Version() (beads.VersionInfo, error) {
	f.reads++
	return f.info, f.err
}

// fakeCompile records the options of each compile and answers with out and err.
type fakeCompile struct {
	out  string
	err  error
	opts []bdbuild.Options
}

func (f *fakeCompile) compile(opt bdbuild.Options) (string, error) {
	f.opts = append(f.opts, opt)
	return f.out, f.err
}

func TestRunRequiresTheCheckoutAndTheOutput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"-src", "/beads"},
		{"-out", "/out/bd"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, &fakeInstalled{}, (&fakeCompile{}).compile); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "-src and -out are required") {
			t.Errorf("run(%v) stderr = %q, want the missing flags named", args, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%v) wrote %q on stdout; the label value is the only stdout", args, stdout.String())
		}
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-nope"}, &stdout, &stderr, &fakeInstalled{}, (&fakeCompile{}).compile); code != 2 {
		t.Errorf("run(-nope) = %d, want 2", code)
	}
}

func TestRunBuildsTheInstalledBDsCommitAndStampsItsBuildID(t *testing.T) {
	t.Parallel()
	installed := &fakeInstalled{info: beads.VersionInfo{Version: "1.2.2", BuildID: hostBuildID, Commit: hostCommit}}
	compile := &fakeCompile{out: hostCommit}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-src", "/beads", "-out", "/out/bd"}, &stdout, &stderr, installed, compile.compile); code != 0 {
		t.Fatalf("run = %d, want 0; stderr %q", code, stderr.String())
	}
	if installed.reads != 1 {
		t.Errorf("installed bd read %d times, want once: Commit and the build stamp share one report", installed.reads)
	}
	if len(compile.opts) != 1 {
		t.Fatalf("compile ran %d times, want once", len(compile.opts))
	}
	opt := compile.opts[0]
	if opt.Src != "/beads" || opt.Out != "/out/bd" {
		t.Errorf("compile got Src %q Out %q, want the flags passed through", opt.Src, opt.Out)
	}
	if opt.Commit != hostCommit {
		t.Errorf("compile Commit = %q, want the installed bd's commit %q", opt.Commit, hostCommit)
	}
	if opt.InstalledCommit != hostCommit || opt.InstalledBuildID != hostBuildID {
		t.Errorf("compile stamp = (%q, %q), want the installed bd's report (%q, %q)", opt.InstalledBuildID, opt.InstalledCommit, hostBuildID, hostCommit)
	}
	if stdout.String() != hostCommit+"\n" {
		t.Errorf("stdout = %q, want the full commit alone", stdout.String())
	}
}

func TestRunNamedCommitStillStampsFromTheInstalledBD(t *testing.T) {
	t.Parallel()
	installed := &fakeInstalled{info: beads.VersionInfo{Version: "1.2.2", BuildID: hostBuildID, Commit: hostCommit}}
	compile := &fakeCompile{out: otherCommit}
	var stdout, stderr bytes.Buffer
	args := []string{"-src", "/beads", "-out", "/out/bd", "-commit", otherCommit}
	if code := run(args, &stdout, &stderr, installed, compile.compile); code != 0 {
		t.Fatalf("run = %d, want 0; stderr %q", code, stderr.String())
	}
	if len(compile.opts) != 1 {
		t.Fatalf("compile ran %d times, want once", len(compile.opts))
	}
	opt := compile.opts[0]
	if opt.Commit != otherCommit {
		t.Errorf("compile Commit = %q, want the named commit %q", opt.Commit, otherCommit)
	}
	// The installed report still reaches Compile, which decides whether it names
	// the commit being built; the override does not hide it from Compile.
	if opt.InstalledCommit != hostCommit || opt.InstalledBuildID != hostBuildID {
		t.Errorf("compile stamp = (%q, %q), want the installed report (%q, %q)", opt.InstalledBuildID, opt.InstalledCommit, hostBuildID, hostCommit)
	}
	if stdout.String() != otherCommit+"\n" {
		t.Errorf("stdout = %q, want the compiled commit alone", stdout.String())
	}
}

func TestRunRefusesAnInstalledBDItCannotVouchFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		installed *fakeInstalled
		want      string
	}{
		{
			name:      "version read fails",
			installed: &fakeInstalled{err: errors.New("bd: not on PATH")},
			want:      "cannot read the installed bd's commit",
		},
		{
			name:      "installed bd reports no commit",
			installed: &fakeInstalled{info: beads.VersionInfo{Version: "1.2.2", BuildID: hostBuildID}},
			want:      "reports no commit",
		},
		{
			name:      "installed bd reports a commit that is not hex",
			installed: &fakeInstalled{info: beads.VersionInfo{Version: "1.2.2", Commit: "not-a-commit"}},
			want:      "not a 7+ character hex commit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			compile := &fakeCompile{}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"-src", "/beads", "-out", "/out/bd"}, &stdout, &stderr, tc.installed, compile.compile); code != 1 {
				t.Errorf("run = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr.String(), tc.want)
			}
			if len(compile.opts) != 0 {
				t.Errorf("compile ran %d times after the refusal, want none", len(compile.opts))
			}
			if stdout.Len() != 0 {
				t.Errorf("wrote %q on stdout after the refusal", stdout.String())
			}
		})
	}
}

func TestInstalledBDReadsWhereItRunsWithTheTargetVariablesStripped(t *testing.T) {
	t.Parallel()
	want := &fakeInstalled{}
	var gotDir string
	var gotEnv []string
	newPlain := func(dir string, env []string) bdbuild.Versioner {
		gotDir, gotEnv = dir, env
		return want
	}
	environ := []string{
		"PATH=/usr/bin",
		"HOME=/home/polecat",
		"BEADS_DIR=/town/other/.beads",
		"BEADS_DOLT_SERVER_DATABASE=other",
		"BD_DB=other",
	}
	if got := installedBD(environ, newPlain); got != want {
		t.Errorf("installedBD returned %v, want the Versioner the constructor built", got)
	}
	if gotDir != "" {
		t.Errorf("constructor dir = %q, want empty: the bd runs where this command runs", gotDir)
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/polecat"} {
		if !slices.Contains(gotEnv, kept) {
			t.Errorf("env %q lacks %q, want the host environment kept", gotEnv, kept)
		}
	}
	for _, dropped := range []string{"BEADS_DIR=/town/other/.beads", "BEADS_DOLT_SERVER_DATABASE=other", "BD_DB=other"} {
		if slices.Contains(gotEnv, dropped) {
			t.Errorf("env still has %q, want the BEADS target variables stripped", dropped)
		}
	}
}

func TestRunRefusesACompileThatFails(t *testing.T) {
	t.Parallel()
	installed := &fakeInstalled{info: beads.VersionInfo{Version: "1.2.2", BuildID: hostBuildID, Commit: hostCommit}}
	compile := &fakeCompile{err: errors.New("cannot build bd at " + hostCommit)}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-src", "/beads", "-out", "/out/bd"}, &stdout, &stderr, installed, compile.compile); code != 1 {
		t.Errorf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot build bd at "+hostCommit) {
		t.Errorf("stderr = %q, want the compile error", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q on stdout after a failed compile; the path is the only stdout", stdout.String())
	}
}
