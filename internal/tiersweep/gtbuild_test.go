package tiersweep

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

// TestSweepBuildsGTOnceAndExportsIt: a run builds ./cmd/gt once with the
// BuiltProperly flag into the log directory, and every go test environment
// names that binary.
func TestSweepBuildsGTOnceAndExportsIt(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	repo.lists[". *_test.go"] = []string{"a_test.go"}
	repo.files["a_test.go"] = taggedTest
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, repo, fake, "", "")
	s.Integration()

	bin := filepath.Join(s.Opts.LogDir, gtBinName)
	got := builds(fake)
	if len(got) != 1 {
		t.Fatalf("go build calls = %d, want 1: %v", len(got), got)
	}
	wantArgv := []string{"go", "build", "-ldflags", gtBuildLDFlags, "-o", bin, "./cmd/gt"}
	if !reflect.DeepEqual(got[0].argv, wantArgv) {
		t.Errorf("go build argv = %v, want %v", got[0].argv, wantArgv)
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

// TestGTBuildFailureIsNotRed: a failing build prints one stderr line, leaves
// TIER_SWEEP_GT_BIN unset in every go test environment, and the tier still runs
// and reports GREEN as it does without the build.
func TestGTBuildFailureIsNotRed(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	repo.lists[". *_test.go"] = []string{"a_test.go"}
	repo.files["a_test.go"] = taggedTest
	fake := &fakeTier{head: "abc123", codes: map[string]int{". ./cmd/gt": 1}}
	s, stderr := newIntegration(t, repo, fake, "", "")
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
