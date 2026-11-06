package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// fakeTarget records what the wrapper asked of it and answers from its fields.
type fakeTarget struct {
	launched  bool
	call      gtwrap.Call
	exit      int
	launchErr error

	doctored  bool
	diag      gtwrap.Diagnosis
	doctorErr error
}

func (f *fakeTarget) Launch(call gtwrap.Call) (int, error) {
	f.launched, f.call = true, call
	return f.exit, f.launchErr
}

func (f *fakeTarget) Doctor() (gtwrap.Diagnosis, error) {
	f.doctored = true
	return f.diag, f.doctorErr
}

// harness is one run's process: a temp town with a marker, a temp HOME and the
// fake target.
type harness struct {
	surface   surface
	fake      *fakeTarget
	town      string
	home      string
	builtTown string
	targetSet bool
	stdout    *bytes.Buffer
	stderr    *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	town := t.TempDir()
	writeTownMarker(t, town)
	h := &harness{
		fake:   &fakeTarget{},
		town:   town,
		home:   t.TempDir(),
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	h.surface = surface{
		getenv: func(name string) string {
			if name == "HOME" {
				return h.home
			}
			return ""
		},
		environ:  func() []string { return []string{"GT_TOWN_ROOT=" + h.town} },
		getwd:    func() (string, error) { return h.town, nil },
		readFile: os.ReadFile,
		stat:     os.Stat,
		stdout:   h.stdout,
		stderr:   h.stderr,
		newDocker: func(town string) target {
			h.builtTown, h.targetSet = town, true
			return h.fake
		},
	}
	return h
}

// run runs the wrapper with argv, whose first entry is the installed name.
func (h *harness) run(argv ...string) int {
	return run(argv, h.surface)
}

func (h *harness) writeConfig(t *testing.T, contents string) {
	t.Helper()
	dir := filepath.Join(h.home, ".config", "gt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wrap.json"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write the config file: %v", err)
	}
}

// writeTownMarker makes dir a town root.
func writeTownMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(townMarker)), 0o755); err != nil {
		t.Fatalf("create the town marker directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, townMarker), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write the town marker: %v", err)
	}
}

func TestRunPicksTheToolFromTheInvokedName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"gt", "bd", "om", "hm"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if exit := h.run(name, "status", "--line"); exit != 0 {
				t.Fatalf("run(%q) = %d, want 0; stderr: %s", name, exit, h.stderr)
			}
			if !h.fake.launched {
				t.Fatal("the wrapper launched nothing")
			}
			want := []string{name, "status", "--line"}
			if !slices.Equal(h.fake.call.Argv, want) {
				t.Fatalf("launched %q, want %q", h.fake.call.Argv, want)
			}
			if h.fake.call.Dir != h.town {
				t.Fatalf("launched in %q, want the caller's directory %q", h.fake.call.Dir, h.town)
			}
			if h.builtTown != h.town {
				t.Fatalf("built the target for town %q, want %q", h.builtTown, h.town)
			}
		})
	}
}

func TestRunPassesTheToolExitCodeThrough(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.exit = 7
	if exit := h.run("gt", "status"); exit != 7 {
		t.Fatalf("run = %d, want the tool's 7", exit)
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("a passed-through status wrote %q to stderr", h.stderr)
	}
}

func TestRunRefusesANameItWasNotInstalledAs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("/usr/local/bin/bash", "-c", "true"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "gt, bd, om and hm") {
		t.Fatalf("stderr %q does not name the four tools", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("the wrapper launched a tool under a name it was not installed as")
	}
}

func TestRunReportsATargetThatCannotLaunch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.launchErr = errors.New("gtwrap: Docker is down")
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "Docker is down") {
		t.Fatalf("stderr %q does not carry the target's error", h.stderr)
	}
}

func TestRunRefusesAnUnknownTargetInTheConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, `{"target":"kubectl"}`)
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	for _, want := range []string{"kubectl", "docker"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", h.stderr, want)
		}
	}
	if h.targetSet || h.fake.launched {
		t.Fatal("a target was built for a name the config file gave that the wrapper does not know")
	}
}

func TestRunRefusesAConfigThatDoesNotParse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, "{")
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "parse the target config file") {
		t.Fatalf("stderr %q does not name the parse failure", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("the wrapper launched with a config file it could not parse")
	}
}

func TestRunRefusesAConfigThatNamesNoTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, "{}")
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "names no target") {
		t.Fatalf("stderr %q does not say the file names no target", h.stderr)
	}
}

func TestRunRefusesAConfigItCannotRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "read the target config file") {
		t.Fatalf("stderr %q does not name the read failure", h.stderr)
	}
}

func TestRunRefusesWithoutHOME(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getenv = func(name string) string {
		if name == "GT_TOWN_ROOT" {
			return h.town
		}
		return ""
	}
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "HOME") {
		t.Fatalf("stderr %q does not name HOME", h.stderr)
	}
}

func TestRunUsesTheTownGT_TOWN_ROOTNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
	h.surface.getenv = func(name string) string {
		switch name {
		case "HOME":
			return h.home
		case "GT_TOWN_ROOT":
			return h.town
		}
		return ""
	}
	if exit := h.run("gt", "status"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if h.builtTown != h.town {
		t.Fatalf("built the target for town %q, want %q", h.builtTown, h.town)
	}
}

func TestRunRefusesAGTTownRootWithoutTheMarker(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	notATown := t.TempDir()
	h.surface.getenv = func(name string) string {
		switch name {
		case "HOME":
			return h.home
		case "GT_TOWN_ROOT":
			return notATown
		}
		return ""
	}
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), townMarker) {
		t.Fatalf("stderr %q does not name the missing marker", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("the wrapper launched with GT_TOWN_ROOT naming no town")
	}
}

func TestRunFindsTheTownAboveTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sub := filepath.Join(h.town, "rig", "polecats", "bullet")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("create the working directory: %v", err)
	}
	h.surface.getwd = func() (string, error) { return sub, nil }
	if exit := h.run("gt", "status"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if h.builtTown != h.town {
		t.Fatalf("built the target for town %q, want the town above the working directory %q", h.builtTown, h.town)
	}
}

func TestRunFallsBackToTheHomeTown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	homeTown := filepath.Join(h.home, "gt")
	writeTownMarker(t, homeTown)
	h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
	if exit := h.run("gt", "status"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if h.builtTown != homeTown {
		t.Fatalf("built the target for town %q, want the home town %q", h.builtTown, homeTown)
	}
}

func TestRunRefusesWhenNoTownIsFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "no town root") {
		t.Fatalf("stderr %q does not say no town root was found", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("the wrapper launched with no town root")
	}
}

func TestRunRefusesWhenTheWorkingDirectoryCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return "", errors.New("getwd: no such file or directory") }
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "working directory") {
		t.Fatalf("stderr %q does not name the working directory", h.stderr)
	}
	if h.fake.launched {
		t.Fatal("the wrapper launched without a working directory to find the town from")
	}
}

func TestRunRefusesWhenTheGTTownRootMarkerCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getenv = func(name string) string {
		switch name {
		case "HOME":
			return h.home
		case "GT_TOWN_ROOT":
			return h.town
		}
		return ""
	}
	h.surface.stat = func(string) (os.FileInfo, error) { return nil, fs.ErrPermission }
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "GT_TOWN_ROOT") {
		t.Fatalf("stderr %q does not name GT_TOWN_ROOT", h.stderr)
	}
}

func TestRunRefusesWhenAMarkerAboveTheWorkingDirectoryCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cwd := t.TempDir()
	h.surface.getwd = func() (string, error) { return cwd, nil }
	h.surface.stat = func(string) (os.FileInfo, error) { return nil, fs.ErrPermission }
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), cwd) {
		t.Fatalf("stderr %q does not name the directory whose marker could not be read", h.stderr)
	}
}

func TestRunRefusesWhenTheHomeTownMarkerCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
	homeTown := filepath.Join(h.home, "gt")
	h.surface.stat = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(homeTown, townMarker) {
			return nil, fs.ErrPermission
		}
		return nil, fs.ErrNotExist
	}
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), homeTown) {
		t.Fatalf("stderr %q does not name the home town whose marker could not be read", h.stderr)
	}
}

func TestDoctorReportsEachRung(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		diag gtwrap.Diagnosis
		exit int
	}{
		{
			name: "exec",
			diag: gtwrap.Diagnosis{
				Rung:      gtwrap.RungExec,
				Container: "gt-daemon",
				Image:     "gt-daemon:7f3e9a1",
				Reason:    "the daemon container gt-daemon is running",
			},
			exit: 0,
		},
		{
			name: "run",
			diag: gtwrap.Diagnosis{
				Rung:      gtwrap.RungRun,
				Container: "gt-daemon",
				Image:     "gt-daemon:7f3e9a1",
				Reason:    "the daemon container gt-daemon is not running, so a throwaway container of the image runs the tool",
			},
			exit: 0,
		},
		{
			name: "down",
			diag: gtwrap.Diagnosis{
				Rung:      gtwrap.RungDown,
				Container: "gt-daemon",
				Image:     "gt-daemon:7f3e9a1",
				Reason:    "Docker is down: docker info did not answer",
			},
			exit: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.fake.diag = tt.diag
			if exit := h.run("gt", doctorFlag); exit != tt.exit {
				t.Fatalf("run(%s) = %d, want %d; stderr: %s", doctorFlag, exit, tt.exit, h.stderr)
			}
			if !h.fake.doctored {
				t.Fatal("the wrapper never asked the target's doctor")
			}
			if h.fake.launched {
				t.Fatal("--doctor launched the tool")
			}
			want := "rung: " + tt.diag.Rung.String() +
				"\ncontainer: " + tt.diag.Container +
				"\nimage: " + tt.diag.Image +
				"\nreason: " + tt.diag.Reason + "\n"
			if got := h.stdout.String(); got != want {
				t.Fatalf("report = %q, want %q", got, want)
			}
		})
	}
}

func TestDoctorRefusesWhenTheDiagnosisCannotBeMade(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.doctorErr = errors.New("gtwrap: read the daemon compose file: no such file")
	if exit := h.run("gt", doctorFlag); exit != 1 {
		t.Fatalf("run(%s) = %d, want 1", doctorFlag, exit)
	}
	if !strings.Contains(h.stderr.String(), "compose file") {
		t.Fatalf("stderr %q does not carry the diagnosis failure", h.stderr)
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("a failed doctor wrote a report: %q", h.stdout)
	}
}

// TestRunSendsALaterDoctorFlagToTheTool: --doctor is the wrapper's flag only as
// the first argument, so a tool argument of the same spelling is not swallowed.
func TestRunSendsALaterDoctorFlagToTheTool(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt", "status", doctorFlag); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.fake.launched {
		t.Fatal("a later --doctor did not reach the tool")
	}
	want := []string{"gt", "status", doctorFlag}
	if !slices.Equal(h.fake.call.Argv, want) {
		t.Fatalf("launched %q, want %q", h.fake.call.Argv, want)
	}
}
