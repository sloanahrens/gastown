package tiersweep

import (
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/buildflags"
)

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
// integration helper's ldflags and the sweep's own environment, into the log
// directory, and every go test environment names that binary.
func TestSweepBuildsGTOnceAndExportsIt(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, taggedRootRepo(), fake, "", "")
	s.Integration()

	bin := filepath.Join(s.Opts.LogDir, gtBinName)
	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
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
