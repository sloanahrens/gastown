package tiersweep

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/buildflags"
)

// binFromBuild is the -o path of a recorded go build call.
func binFromBuild(t *testing.T, c tierCall) string {
	t.Helper()
	for i, a := range c.argv {
		if a == "-o" && i+1 < len(c.argv) {
			return c.argv[i+1]
		}
	}
	t.Fatalf("go build argv = %v, want one naming an -o path", c.argv)
	return ""
}

// buildGTLines is the stderr lines that report the gt build.
func buildGTLines(stderr string) []string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "build gt") {
			lines = append(lines, l)
		}
	}
	return lines
}

// underDir reports whether path is dir or under it.
func underDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// gtTestEnvs is the environment of each go test command the integration tier
// ran, in order. A go test runs through gt slot run, so its argv starts with gt.
func gtTestEnvs(fake *fakeTier) [][]string {
	var envs [][]string
	for _, c := range fake.calls {
		if c.argv[0] == "gt" {
			envs = append(envs, c.env)
		}
	}
	return envs
}

// builds lists the go build calls the integration tier made.
func builds(fake *fakeTier) []tierCall {
	var calls []tierCall
	for _, c := range fake.calls {
		if c.argv[0] == "go" && c.argv[1] == "build" {
			calls = append(calls, c)
		}
	}
	return calls
}

// hasEnv reports whether env holds the exact entry name=value.
func hasEnv(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

// anyEnvNamed reports whether env holds any entry for name.
func anyEnvNamed(env []string, name string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, name+"=") {
			return true
		}
	}
	return false
}

// taggedRootRepo is a root module with one integration-tagged package, a.
func taggedRootRepo() *fakeRepo {
	repo := baseRepo()
	repo.lists[". *_test.go"] = []string{"a_test.go"}
	repo.files["a_test.go"] = taggedTest
	return repo
}

// TestSweepBuildsGTOnceAndExportsIt: a run builds ./cmd/gt once with the
// integration helper's ldflags and the sweep's own environment, outside the
// kept log directory, and every go test environment names that binary.
func TestSweepBuildsGTOnceAndExportsIt(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, taggedRootRepo(), fake, "", "")
	s.Integration()

	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	bin := binFromBuild(t, got[0])
	wantArgv := []string{"go", "build", "-ldflags", buildflags.LDFlags, "-o", bin, "./cmd/gt"}
	if !reflect.DeepEqual(got[0].argv, wantArgv) {
		t.Errorf("go build argv = %v, want %v", got[0].argv, wantArgv)
	}
	if got[0].dir != "." {
		t.Errorf("go build dir = %q, want the root module", got[0].dir)
	}
	if !hasEnv(got[0].env, "PATH=/fake/bin") {
		t.Errorf("go build environment = %v, want it to carry the sweep's own PATH", got[0].env)
	}
	envs := gtTestEnvs(fake)
	if len(envs) == 0 {
		t.Fatal("no go test ran, so the export is not checked")
	}
	for i, env := range envs {
		if !hasEnv(env, EnvGTBin+"="+bin) {
			t.Errorf("go test %d environment does not name %s=%s", i, EnvGTBin, bin)
		}
	}
}

// TestBuildGTIsNotInTheLogDirectoryAndIsRemoved: the binary handed to the tests
// is outside the kept log directory — under a temporary directory a host sweep
// removes, or under the work tree a runner gave — the build log is inside it,
// and the directory holding the binary is gone when the tier returns.
func TestBuildGTIsNotInTheLogDirectoryAndIsRemoved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// runner gives the sweep a work tree, as the container runner does.
		runner bool
	}{
		{name: "a host sweep builds under a temporary directory"},
		{name: "a runner's work tree holds the binary", runner: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeTier{head: "abc123"}
			s, _ := newIntegration(t, taggedRootRepo(), fake, "", "")
			var work string
			if tc.runner {
				work = t.TempDir()
				s.Opts.WorkDir = work
			}
			s.Integration()

			got := builds(fake)
			if len(got) != 1 {
				t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
			}
			bin := binFromBuild(t, got[0])
			if underDir(bin, s.Opts.LogDir) {
				t.Errorf("binary %s is under the kept log directory %s", bin, s.Opts.LogDir)
			}
			if tc.runner && !underDir(bin, work) {
				t.Errorf("binary %s is not under the work tree %s the runner gave", bin, work)
			}
			if _, err := os.Stat(filepath.Dir(bin)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("build directory %s is still there after the sweep: %v", filepath.Dir(bin), err)
			}
			logPath := filepath.Join(s.Opts.LogDir, buildLogName)
			if _, err := os.Stat(logPath); err != nil {
				t.Errorf("build log %s is not in the log directory: %v", logPath, err)
			}
		})
	}
}

// TestBuildGTRefusesAWorkTreeItCannotMake: a work tree whose gt-bin directory
// cannot be made runs no build, leaves TIER_SWEEP_GT_BIN unset, and the tier
// still runs GREEN as it does after a failed build. A regular file where the
// work tree should be makes the mkdir fail.
func TestBuildGTRefusesAWorkTreeItCannotMake(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.Opts.WorkDir = file
	line, red := s.Integration()

	if got := builds(fake); len(got) != 0 {
		t.Errorf("go build calls = %v, want none where the directory cannot be made", got)
	}
	if lines := buildGTLines(stderr.String()); len(lines) != 1 {
		t.Errorf("build gt stderr lines = %d, want 1: %q", len(lines), stderr.String())
	}
	for i, env := range gtTestEnvs(fake) {
		if anyEnvNamed(env, EnvGTBin) {
			t.Errorf("go test %d environment names %s with no build", i, EnvGTBin)
		}
	}
	if red || !strings.Contains(line, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Errorf("line = %q red = %v, want the tier GREEN with its package passed", line, red)
	}
}

// TestBuildGTTemporaryDirectoryFailure: a host sweep that cannot make its
// temporary directory runs no build, reports it once, leaves TIER_SWEEP_GT_BIN
// unset, and the tier still runs GREEN.
func TestBuildGTTemporaryDirectoryFailure(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	s.MkdirTemp = func(string, string) (string, error) { return "", errors.New("no temporary directory") }
	line, red := s.Integration()

	if got := builds(fake); len(got) != 0 {
		t.Errorf("go build calls = %v, want none with no directory to build in", got)
	}
	lines := buildGTLines(stderr.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "no temporary directory") {
		t.Errorf("build gt stderr lines = %q, want one naming the failure", lines)
	}
	for i, env := range gtTestEnvs(fake) {
		if anyEnvNamed(env, EnvGTBin) {
			t.Errorf("go test %d environment names %s with no build", i, EnvGTBin)
		}
	}
	if red || !strings.Contains(line, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Errorf("line = %q red = %v, want the tier GREEN with its package passed", line, red)
	}
}

// TestBuildGTRemovalFailure: a build directory that cannot be removed is one
// stderr line and changes no verdict, as a log directory that cannot be
// removed is.
func TestBuildGTRemovalFailure(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	s.Opts.WorkDir = t.TempDir()
	var removed []string
	s.RemoveAll = func(path string) error {
		removed = append(removed, path)
		return errors.New("busy")
	}
	line, red := s.Integration()

	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	dir := filepath.Dir(binFromBuild(t, got[0]))
	if len(removed) != 1 || removed[0] != dir {
		t.Errorf("removals = %v, want just the build directory %s", removed, dir)
	}
	if !strings.Contains(stderr.String(), "remove "+dir+": busy") {
		t.Errorf("stderr = %q, want one line naming the failed removal of %s", stderr.String(), dir)
	}
	if red || !strings.Contains(line, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Errorf("line = %q red = %v, want the tier GREEN with its package passed", line, red)
	}
}

// TestRunBuildsGTUnderTheRootItWasGiven: a run started with --root, as the
// container runner starts it, builds gt under that work tree.
func TestRunBuildsGTUnderTheRootItWasGiven(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	work := t.TempDir()
	logDir := t.TempDir()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	code := Run([]string{"--root", work, TierIntegration}, io.Discard, io.Discard, Deps{
		CgoEnv: testCgoEnv,
		Host: Host{
			Env: func(name string) string {
				if name == EnvLogDir {
					return logDir
				}
				return ""
			},
			MkdirAll: func(string, fs.FileMode) error { return nil },
		},
		Exec:    fake.exec,
		Src:     taggedRootRepo(),
		Environ: []string{"PATH=/fake/bin"},
		Now:     func() time.Time { return at },
	})
	if code != 0 {
		t.Fatalf("Run exited %d, want a GREEN run", code)
	}
	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	bin := binFromBuild(t, got[0])
	if !underDir(bin, work) {
		t.Errorf("go build -o %s, want the binary under --root %s", bin, work)
	}
	if underDir(bin, logDir) {
		t.Errorf("go build -o %s, want it outside the log directory %s", bin, logDir)
	}
}

// TestRunLeavesARelativeRootOutOfTheBinaryPath: a relative --root does not name
// the binary's directory. TIER_SWEEP_GT_BIN reaches each go test, which would
// resolve a relative path from its own package directory, so the run builds
// under a temporary directory instead.
func TestRunLeavesARelativeRootOutOfTheBinaryPath(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	logDir := t.TempDir()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	code := Run([]string{"--root", "worktree", TierIntegration}, io.Discard, io.Discard, Deps{
		CgoEnv: testCgoEnv,
		Host: Host{
			Env: func(name string) string {
				if name == EnvLogDir {
					return logDir
				}
				return ""
			},
			MkdirAll: func(string, fs.FileMode) error { return nil },
		},
		Exec:    fake.exec,
		Src:     taggedRootRepo(),
		Environ: []string{"PATH=/fake/bin"},
		Now:     func() time.Time { return at },
	})
	if code != 0 {
		t.Fatalf("Run exited %d, want a GREEN run", code)
	}
	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	bin := binFromBuild(t, got[0])
	if !filepath.IsAbs(bin) {
		t.Errorf("go build -o %s, want an absolute path a go test can use", bin)
	}
	if strings.HasPrefix(bin, "worktree"+string(filepath.Separator)) {
		t.Errorf("go build -o %s, want the binary outside the relative --root", bin)
	}
}

// TestSweepBuildsNoGTWithoutRootPackages: a plan whose packages all live in a
// nested module runs no build and exports nothing, since no helper in them
// would have built gt.
func TestSweepBuildsNoGTWithoutRootPackages(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	setTracker(repo)
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, repo, fake, "", "")
	line, _ := s.Integration()

	if got := builds(fake); len(got) != 0 {
		t.Errorf("go build calls = %v, want none for a plan with no root package", got)
	}
	for i, env := range gtTestEnvs(fake) {
		if anyEnvNamed(env, EnvGTBin) {
			t.Errorf("go test %d names %s with nothing in the root to use it", i, EnvGTBin)
		}
	}
	if !strings.Contains(line, "integration GREEN passed=1") {
		t.Errorf("summary = %q, want the nested module's package counted", line)
	}
}

// TestGTBuildCarriesTheICUCgoEnv: whatever icu4c cgo entries the build needs
// on this host reach the go build, so the sweep's binary is the one the
// integration helper would have made.
func TestGTBuildCarriesTheICUCgoEnv(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, taggedRootRepo(), fake, "", "")
	s.CgoEnv = func([]string) []string { return []string{"CGO_CPPFLAGS=-I/icu/include"} }
	s.Integration()

	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	if !hasEnv(got[0].env, "CGO_CPPFLAGS=-I/icu/include") {
		t.Errorf("go build environment = %v, want the icu4c cgo entries on it", got[0].env)
	}
	if !hasEnv(got[0].env, "PATH=/fake/bin") {
		t.Errorf("go build environment = %v, want the sweep's environment kept", got[0].env)
	}
}

// TestGTBuildIsBounded: the build runs through the bounded seam with
// buildTimeout, and a build the deadline stopped is one stderr line that names
// the deadline, leaves the variable unset, and does not make the tier RED.
func TestGTBuildIsBounded(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	var deadlines []time.Duration
	s.BuildExec = func(dir string, env []string, argv []string, out io.Writer, timeout time.Duration) int {
		deadlines = append(deadlines, timeout)
		fake.exec(dir, env, argv, out)
		return buildKilledCode
	}
	line, red := s.Integration()

	if len(deadlines) != 1 || deadlines[0] != buildTimeout {
		t.Fatalf("build deadlines = %v, want one of %v", deadlines, buildTimeout)
	}
	wantLine := "did not finish in " + buildTimeout.String()
	var named []string
	for _, l := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(l, "build gt") {
			named = append(named, l)
		}
	}
	if len(named) != 1 || !strings.Contains(named[0], wantLine) {
		t.Errorf("build stderr lines = %q, want one naming %q", named, wantLine)
	}
	for i, env := range gtTestEnvs(fake) {
		if anyEnvNamed(env, EnvGTBin) {
			t.Errorf("go test %d environment names %s after a killed build", i, EnvGTBin)
		}
	}
	if red || !strings.Contains(line, "integration GREEN passed=1") {
		t.Errorf("line = %q red = %v, want the tier GREEN with its package passed", line, red)
	}
}

// TestGTBuildFailureIsNotRed: a failing build prints one stderr line, leaves
// TIER_SWEEP_GT_BIN unset in every go test environment, and the tier still runs
// and reports GREEN as it does without the build.
func TestGTBuildFailureIsNotRed(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123", codes: map[string]int{". ./cmd/gt": 1}}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	line, red := s.Integration()

	var buildLines []string
	for _, l := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(l, "build gt") {
			buildLines = append(buildLines, l)
		}
	}
	if len(buildLines) != 1 {
		t.Errorf("build gt stderr lines = %d, want 1: %q", len(buildLines), stderr.String())
	}
	for i, env := range gtTestEnvs(fake) {
		if anyEnvNamed(env, EnvGTBin) {
			t.Errorf("go test %d environment names %s after a failed build", i, EnvGTBin)
		}
	}
	if len(gtTestEnvs(fake)) == 0 {
		t.Fatal("no go test ran after the failed build")
	}
	if red || !strings.Contains(line, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Errorf("line = %q red = %v, want the tier GREEN with its package passed", line, red)
	}
}

// TestGTBuildFailureKeepsItsOutput: a failed build's output is written to the
// build log in the log directory, and the stderr line names that file instead
// of quoting the output.
func TestGTBuildFailureKeepsItsOutput(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	s.BuildExec = func(dir string, env []string, argv []string, out io.Writer, timeout time.Duration) int {
		fmt.Fprintln(out, "./x.go:3:2: undefined: y")
		return 1
	}
	s.Integration()

	logPath := filepath.Join(s.Opts.LogDir, buildLogName)
	kept, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read build log: %v", err)
	}
	if !strings.Contains(string(kept), "undefined: y") {
		t.Errorf("build log = %q, want the compiler output", kept)
	}
	if strings.Contains(stderr.String(), "undefined: y") {
		t.Errorf("stderr = %q, want the output left out of it", stderr.String())
	}
	if !strings.Contains(stderr.String(), logPath) {
		t.Errorf("stderr = %q, want it to name %s", stderr.String(), logPath)
	}
}

// TestGTBuildLogCannotBeCreated: a build whose log cannot be created does not
// run; the sweep reports it once, leaves TIER_SWEEP_GT_BIN unset, and the tier
// still runs GREEN. A directory in the log's place makes the create fail.
func TestGTBuildLogCannotBeCreated(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, taggedRootRepo(), fake, "", "")
	if err := os.Mkdir(filepath.Join(s.Opts.LogDir, buildLogName), 0o755); err != nil {
		t.Fatal(err)
	}
	line, red := s.Integration()

	if got := builds(fake); len(got) != 0 {
		t.Errorf("go build calls = %v, want none when its log cannot be created", got)
	}
	var buildLines []string
	for _, l := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(l, "build gt") {
			buildLines = append(buildLines, l)
		}
	}
	if len(buildLines) != 1 {
		t.Errorf("build gt stderr lines = %d, want 1: %q", len(buildLines), stderr.String())
	}
	for i, env := range gtTestEnvs(fake) {
		if anyEnvNamed(env, EnvGTBin) {
			t.Errorf("go test %d environment names %s with no build", i, EnvGTBin)
		}
	}
	if red || !strings.Contains(line, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Errorf("line = %q red = %v, want the tier GREEN with its package passed", line, red)
	}
}

// TestRunCarriesDepsCgoEnvToTheBuild: the cgo entries a run's Deps answer are
// the ones the go build gets, so a unit test of Run never reaches the real ICU
// lookup, which starts brew on a Mac (gt-0brwb). The environment the entries
// are added to is the run's own.
func TestRunCarriesDepsCgoEnvToTheBuild(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	logDir := t.TempDir()
	code := Run([]string{TierIntegration}, io.Discard, io.Discard, Deps{
		CgoEnv: func(base []string) []string { return append(base[:len(base):len(base)], "CGO_LDFLAGS=-L/icu/lib") },
		Host: Host{
			Env: func(name string) string {
				if name == EnvLogDir {
					return logDir
				}
				return ""
			},
			MkdirAll: func(string, fs.FileMode) error { return nil },
		},
		Exec:    fake.exec,
		Src:     taggedRootRepo(),
		Environ: []string{"PATH=/fake/bin"},
		Now:     func() time.Time { return at },
	})
	if code != 0 {
		t.Fatalf("Run exited %d, want a GREEN run", code)
	}
	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	if !hasEnv(got[0].env, "CGO_LDFLAGS=-L/icu/lib") || !hasEnv(got[0].env, "PATH=/fake/bin") {
		t.Errorf("go build environment = %v, want the Deps entries added to the run's own", got[0].env)
	}
}

// TestUnitSweepHelperAnswersTheCgoEnvItself: the build a Sweep the shared helper
// builds takes its cgo entries from the helper's testCgoEnv, so it never asks
// buildflags.ICUCgoEnv, whether or not the host has brew. The seam is checked by
// identity, without calling the fallback, so the guard is red on a host without
// brew too and starts no brew on a Mac (gt-0brwb).
func TestUnitSweepHelperAnswersTheCgoEnvItself(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, taggedRootRepo(), fake, "", "")
	if reflect.ValueOf(s.cgoEnv()).Pointer() != reflect.ValueOf(testCgoEnv).Pointer() {
		t.Fatal("the build's cgo seam is not the helper's testCgoEnv: a Sweep with no CgoEnv takes buildflags.ICUCgoEnv, which starts brew on a Mac (gt-0brwb)")
	}
	if got := s.cgoEnv()([]string{"PATH=/fake/bin"}); got != nil {
		t.Fatalf("the helper's cgo seam answered %v, want nil: its gt build would take icu4c entries from the host", got)
	}
	s.Integration()
	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	if anyEnvNamed(got[0].env, "CGO_CPPFLAGS") || anyEnvNamed(got[0].env, "CGO_LDFLAGS") {
		t.Errorf("go build environment = %v, want no icu4c entries from the host", got[0].env)
	}
}
