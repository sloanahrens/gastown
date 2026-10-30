package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sweeprunner"
	"github.com/steveyegge/gastown/internal/tiersweep"
)

// testSHA is the commit every case tests: 40 lower-case hex digits.
const testSHA = "4f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39"

// harness is one run wired to fakes: the output and work directories are real
// temp dirs, so the result is written and read back by the production code,
// while the clone, the sweep and the clock are scripted.
type harness struct {
	deps      Deps
	out       string
	work      string
	cloneAt   []string
	sweepAt   []string
	overrides map[string]string
	stdout    bytes.Buffer
	stderr    bytes.Buffer
}

// newHarness wires a run: clone and sweep do what the case scripts, and every
// other seam writes and reads a real t.TempDir().
func newHarness(t *testing.T, clone func(repo, sha, work string) error, sweep func(args []string, overrides map[string]string, stdout io.Writer) int) *harness {
	t.Helper()
	h := &harness{out: t.TempDir(), work: t.TempDir()}
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	calls := 0
	h.deps = Deps{
		Clone: func(repo, sha, work string) error {
			h.cloneAt = []string{repo, sha, work}
			if clone == nil {
				return nil
			}
			return clone(repo, sha, work)
		},
		Sweep: func(args []string, overrides map[string]string, stdout, _ io.Writer) int {
			h.sweepAt = args
			h.overrides = overrides
			if sweep == nil {
				return 0
			}
			return sweep(args, overrides, stdout)
		},
		WriteResult: sweeprunner.Write,
		MkdirTemp:   func(string, string) (string, error) { return h.work, nil },
		MkdirAll:    os.MkdirAll,
		// The work directory is the case's own temp dir: removing it would take
		// the rest of the case's files with it.
		RemoveAll: func(string) error { return nil },
		Environ:   []string{},
		Now: func() time.Time {
			calls++
			return start.Add(time.Duration(calls) * time.Second)
		},
	}
	return h
}

// run runs one command line against the harness and returns the exit code.
func (h *harness) run(args ...string) int {
	return Run(args, &h.stdout, &h.stderr, h.deps)
}

// resultFileExists reports whether the run left a result in the output directory.
func (h *harness) resultFileExists(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(h.out, sweeprunner.ResultFileName))
	return err == nil
}

// TestRunWritesResultForAPassingSweep: a passing sweep leaves a result.json that
// reads back with both tiers GREEN, the rig and sha the run was given, and the
// run exits 0.
func TestRunWritesResultForAPassingSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout io.Writer) int {
		logDir := overrides[tiersweep.EnvLogDir]
		fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=66 failed=0 skipped=0 (logs %s) in 2m0s\n", logDir)
		fmt.Fprintf(stdout, "tier-sweep: race GREEN passed=3 failed=0 skipped=0 (logs %s) in 30s\n", logDir)
		return 0
	})
	code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", "integration,race", "--jobs", "4",
		"--out", h.out, "--repo", "/repo.git")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, h.stderr.String())
	}
	r, err := sweeprunner.Read(h.out, "gastown", testSHA)
	if err != nil {
		t.Fatalf("read the result: %v", err)
	}
	if r.Rig != "gastown" || r.SHA != testSHA {
		t.Errorf("result names rig %q sha %q, want gastown %s", r.Rig, r.SHA, testSHA)
	}
	if !r.Finished.After(r.Started) {
		t.Errorf("finished %s is not after started %s", r.Finished, r.Started)
	}
	verdicts, err := sweeprunner.ToTierResults(r)
	if err != nil {
		t.Fatalf("convert the result: %v", err)
	}
	for _, tier := range []string{tiersweep.TierIntegration, tiersweep.TierRace} {
		if v := verdicts[tier]; v.Verdict != "GREEN" {
			t.Errorf("tier %s = %+v, want GREEN", tier, v)
		}
	}
	if want := []string{"/repo.git", testSHA, h.work}; !equal(h.cloneAt, want) {
		t.Errorf("clone ran with %q, want %q", h.cloneAt, want)
	}
	if want := []string{"--root", h.work, "integration", "race"}; !equal(h.sweepAt, want) {
		t.Errorf("sweep ran with %q, want %q", h.sweepAt, want)
	}
	if got := h.overrides[tiersweep.EnvLogDir]; got != filepath.Join(h.out, "logs") {
		t.Errorf("sweep log directory = %q, want %q", got, filepath.Join(h.out, "logs"))
	}
	if got := h.overrides[tiersweep.EnvTested]; got != testSHA {
		t.Errorf("sweep tested = %q, want %q", got, testSHA)
	}
	if got := h.overrides[envJobs]; got != "4" {
		t.Errorf("sweep jobs = %q, want 4", got)
	}
	// The summary lines are echoed to the process's stdout, which is what the
	// container's log shows a reader.
	for _, tier := range []string{"integration", "race"} {
		if !strings.Contains(h.stdout.String(), "tier-sweep: "+tier+" GREEN") {
			t.Errorf("stdout = %q, want the %s summary line", h.stdout.String(), tier)
		}
	}
}

// TestRunReadsTheContainerEnvironment: with no --rig, --sha, --tiers or --jobs on
// the command line, the run takes the values the rendered container sets.
func TestRunReadsTheContainerEnvironment(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout io.Writer) int {
		fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
		return 0
	})
	h.deps.Environ = []string{
		sweeprunner.EnvRig + "=gastown",
		sweeprunner.EnvSHA + "=" + testSHA,
		sweeprunner.EnvTiers + "=integration",
		sweeprunner.EnvJobs + "=8",
	}
	if code := h.run("--out", h.out, "--repo", "/repo.git"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, h.stderr.String())
	}
	r, err := sweeprunner.Read(h.out, "gastown", testSHA)
	if err != nil {
		t.Fatalf("read the result: %v", err)
	}
	if r.Rig != "gastown" || r.SHA != testSHA {
		t.Errorf("result names rig %q sha %q, want gastown %s", r.Rig, r.SHA, testSHA)
	}
	if want := []string{"--root", h.work, "integration"}; !equal(h.sweepAt, want) {
		t.Errorf("sweep ran with %q, want %q", h.sweepAt, want)
	}
	if got := h.overrides[envJobs]; got != "8" {
		t.Errorf("sweep jobs = %q, want 8", got)
	}
}

// TestRunReportsRedAndStillExitsZero: the run's exit code speaks for whether a
// result exists, not for the verdicts in it. A failing tier and a clone that
// never ran both leave a result the daemon can read, and both exit 0.
func TestRunReportsRedAndStillExitsZero(t *testing.T) {
	t.Parallel()

	t.Run("a failing tier names its failed package in the result", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout io.Writer) int {
			fmt.Fprintf(stdout, "tier-sweep: integration RED passed=65 failed=1 skipped=4 failed: ./internal/daemon (logs %s) in 28m50s\n",
				overrides[tiersweep.EnvLogDir])
			return 1
		})
		if code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", "integration", "--out", h.out, "--repo", "/repo.git"); code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, h.stderr.String())
		}
		r, err := sweeprunner.Read(h.out, "gastown", testSHA)
		if err != nil {
			t.Fatalf("read the result: %v", err)
		}
		verdicts, err := sweeprunner.ToTierResults(r)
		if err != nil {
			t.Fatalf("convert the result: %v", err)
		}
		got := verdicts[tiersweep.TierIntegration]
		if got.Verdict != "RED" || got.Failed != 1 || !equal(got.FailedNames, []string{"./internal/daemon"}) {
			t.Errorf("integration = %+v, want RED with ./internal/daemon failed", got)
		}
	})

	t.Run("a failed clone is a RED result with the git error", func(t *testing.T) {
		t.Parallel()
		const gitErr = "fatal: '/repo.git' does not appear to be a git repository\n" +
			"fatal: Could not read from remote repository."
		h := newHarness(t, func(string, string, string) error { return errors.New(gitErr) },
			func([]string, map[string]string, io.Writer) int {
				t.Error("the sweep ran after the clone failed")
				return 0
			})
		if code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", "integration,race", "--out", h.out, "--repo", "/repo.git"); code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, h.stderr.String())
		}
		r, err := sweeprunner.Read(h.out, "gastown", testSHA)
		if err != nil {
			t.Fatalf("read the result: %v", err)
		}
		if len(r.Tiers) != 2 {
			t.Fatalf("result holds %d tiers, want 2", len(r.Tiers))
		}
		verdicts, err := sweeprunner.ToTierResults(r)
		if err != nil {
			t.Fatalf("convert the result: %v", err)
		}
		for _, tier := range []string{tiersweep.TierIntegration, tiersweep.TierRace} {
			if got := verdicts[tier]; got.Verdict != "RED" {
				t.Errorf("tier %s = %+v, want RED", tier, got)
			}
		}
		if !strings.Contains(r.Tiers[0].Summary, "fatal: '/repo.git' does not appear to be a git repository") {
			t.Errorf("summary %q does not hold the git error", r.Tiers[0].Summary)
		}
		// The error is two lines; the summary the daemon parses is one.
		if strings.ContainsAny(r.Tiers[0].Summary, "\n\r") {
			t.Errorf("summary %q holds a line break", r.Tiers[0].Summary)
		}
	})

	t.Run("a tier the sweep printed no line for is RED", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, _ map[string]string, stdout io.Writer) int {
			// A line too short to be a summary line, and nothing for race: the
			// sweep was cut before it reached the tier.
			fmt.Fprintln(stdout, "tier-sweep: race")
			return 0
		})
		if code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", "integration,race", "--out", h.out, "--repo", "/repo.git"); code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, h.stderr.String())
		}
		r, err := sweeprunner.Read(h.out, "gastown", testSHA)
		if err != nil {
			t.Fatalf("read the result: %v", err)
		}
		verdicts, err := sweeprunner.ToTierResults(r)
		if err != nil {
			t.Fatalf("convert the result: %v", err)
		}
		for _, tier := range []string{tiersweep.TierIntegration, tiersweep.TierRace} {
			got := verdicts[tier]
			if got.Verdict != "RED" || len(got.FailedNames) == 0 || !strings.Contains(strings.Join(got.FailedNames, " "), "no summary line") {
				t.Errorf("tier %s = %+v, want RED naming the missing summary line", tier, got)
			}
		}
	})
}

// TestRunRefusesBadArguments: a command line the runner cannot use exits 2 with
// the usage line, runs neither step, and leaves no result behind.
func TestRunRefusesBadArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{"a rig that is a path", []string{"--rig", "../x", "--sha", testSHA}},
		{"a short sha", []string{"--rig", "gastown", "--sha", "abc123"}},
		{"no sha", []string{"--rig", "gastown"}},
		{"a tier the sweep does not offer", []string{"--rig", "gastown", "--sha", testSHA, "--tiers", "integration,nope"}},
		{"a tier named twice", []string{"--rig", "gastown", "--sha", testSHA, "--tiers", "integration,integration"}},
		{"zero jobs", []string{"--rig", "gastown", "--sha", testSHA, "--jobs", "0"}},
		{"more jobs than a spec renders", []string{"--rig", "gastown", "--sha", testSHA, "--jobs", "17"}},
		{"a jobs value that is not a number", []string{"--rig", "gastown", "--sha", testSHA, "--jobs", "many"}},
		{"no rig", []string{"--sha", testSHA}},
		{"an argument that is not a flag", []string{"integration"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t,
				func(string, string, string) error {
					t.Error("the clone ran for a refused command line")
					return nil
				},
				func([]string, map[string]string, io.Writer) int {
					t.Error("the sweep ran for a refused command line")
					return 0
				})
			args := append(append([]string{}, tc.args...), "--out", h.out, "--repo", "/repo.git")
			if code := h.run(args...); code != UsageExit {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, UsageExit, h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), usage) {
				t.Errorf("stderr = %q, want the usage line %q", h.stderr.String(), usage)
			}
			if h.resultFileExists(t) {
				t.Error("a refused command line wrote a result")
			}
		})
	}
}

// TestRunRefusesAFlagWithNoValue: a flag at the end of the command line has no
// value to take, and the runner says so rather than reading it as unnamed.
func TestRunRefusesAFlagWithNoValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	if code := h.run("--rig", "gastown", "--out", h.out, "--repo", "/repo.git", "--sha"); code != UsageExit {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, UsageExit, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), usage) {
		t.Errorf("stderr = %q, want the usage line", h.stderr.String())
	}
}

// TestRunRefusesRelativePaths: the output directory and the repository are
// paths this process resolves, so a relative one would land wherever the
// process happens to start.
func TestRunRefusesRelativePaths(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"the output directory": {"--out", "out", "--repo", "/repo.git"},
		"the repository":       {"--repo", "repo"},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil, nil)
			args := append([]string{"--rig", "gastown", "--sha", testSHA, "--out", h.out}, paths...)
			if code := h.run(args...); code != UsageExit {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, UsageExit, h.stderr.String())
			}
		})
	}
}

// TestRunRefusesABadEnvironment: a value the container sets that the runner
// cannot use is refused before anything runs.
func TestRunRefusesABadEnvironment(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	h.deps.Environ = []string{sweeprunner.EnvJobs + "=many"}
	if code := h.run("--rig", "gastown", "--sha", testSHA, "--out", h.out, "--repo", "/repo.git"); code != UsageExit {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, UsageExit, h.stderr.String())
	}
}

// TestRunFailsWhenItCannotPrepareTheRun: no output directory and no work
// directory are both runs that cannot leave a result, so they exit non-zero
// instead of reading as a sweep of nothing.
func TestRunFailsWhenItCannotPrepareTheRun(t *testing.T) {
	t.Parallel()
	t.Run("the output directory", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, nil)
		h.deps.MkdirAll = func(string, fs.FileMode) error { return errors.New("read-only file system") }
		if code := h.run("--rig", "gastown", "--sha", testSHA, "--out", h.out, "--repo", "/repo.git"); code != FailureExit {
			t.Fatalf("exit code = %d, want %d (stderr %q)", code, FailureExit, h.stderr.String())
		}
		if !strings.Contains(h.stderr.String(), "read-only") {
			t.Errorf("stderr = %q, want it to name the failure", h.stderr.String())
		}
		if h.cloneAt != nil {
			t.Error("the clone ran without an output directory to write the result into")
		}
	})
	t.Run("the work directory", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, nil)
		h.deps.MkdirTemp = func(string, string) (string, error) { return "", errors.New("no space left") }
		if code := h.run("--rig", "gastown", "--sha", testSHA, "--out", h.out, "--repo", "/repo.git"); code != FailureExit {
			t.Fatalf("exit code = %d, want %d (stderr %q)", code, FailureExit, h.stderr.String())
		}
		if !strings.Contains(h.stderr.String(), "no space left") {
			t.Errorf("stderr = %q, want it to name the failure", h.stderr.String())
		}
	})
}

// TestRunKeepsTheResultWhenCleanupFails: the work tree is scratch, so a
// removal that fails is one stderr line and the result stands.
func TestRunKeepsTheResultWhenCleanupFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout io.Writer) int {
		fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
		return 0
	})
	h.deps.RemoveAll = func(string) error { return errors.New("device busy") }
	if code := h.run("--rig", "gastown", "--sha", testSHA, "--out", h.out, "--repo", "/repo.git"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "device busy") {
		t.Errorf("stderr = %q, want it to name the removal that failed", h.stderr.String())
	}
	if _, err := sweeprunner.Read(h.out, "gastown", testSHA); err != nil {
		t.Errorf("read the result: %v", err)
	}
}

// TestEnvironWithAppendsTheOverridesInOrder: the environment a sweep's children
// see does not depend on map iteration order.
func TestEnvironWithAppendsTheOverridesInOrder(t *testing.T) {
	t.Parallel()
	got := environWith([]string{"PATH=/bin", "GT_TEST_DOCKER=1"}, map[string]string{"TIER_SWEEP_JOBS": "4", "TIER_SWEEP_LOGDIR": "/out/logs"})
	want := []string{"PATH=/bin", "GT_TEST_DOCKER=1", "TIER_SWEEP_JOBS=4", "TIER_SWEEP_LOGDIR=/out/logs"}
	if !equal(got, want) {
		t.Errorf("environWith = %q, want %q", got, want)
	}
}

// TestParseArgsHoldsTheRenderedSpecRules: every value the runner refuses is one
// sweeprunner.Spec.Validate refuses, so the daemon cannot render a run the
// entrypoint will not start.
func TestParseArgsHoldsTheRenderedSpecRules(t *testing.T) {
	t.Parallel()
	valid := sweeprunner.Spec{
		Rig: "gastown", SHA: testSHA, Image: "gastown-polecat:dev",
		CPUs: 4, MemoryGB: 8, Jobs: 4,
		OutDir: sweeprunner.OutputDir, RepoDir: sweeprunner.RepoMount,
		Context: "colima-gt-sbx", RunID: "run-1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the base spec is not valid: %v", err)
	}
	cases := []struct {
		name  string
		spoil func(*sweeprunner.Spec)
		args  []string
	}{
		{"a rig that is not a segment", func(s *sweeprunner.Spec) { s.Rig = "../x" },
			[]string{"--rig", "../x", "--sha", testSHA}},
		{"a short sha", func(s *sweeprunner.Spec) { s.SHA = "abc123" },
			[]string{"--rig", "gastown", "--sha", "abc123"}},
		{"a tier the sweep does not offer", func(s *sweeprunner.Spec) { s.Tiers = []string{"integration", "nope"} },
			[]string{"--rig", "gastown", "--sha", testSHA, "--tiers", "integration,nope"}},
		{"zero jobs", func(s *sweeprunner.Spec) { s.Jobs = 0 },
			[]string{"--rig", "gastown", "--sha", testSHA, "--jobs", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := valid
			tc.spoil(&bad)
			if err := bad.Validate(); err == nil {
				t.Errorf("sweeprunner.Spec.Validate accepted %+v", bad)
			}
			if _, err := parseArgs(tc.args, nil); err == nil {
				t.Errorf("parseArgs accepted %q", tc.args)
			}
		})
	}
}

// TestRunFailsWhenTheResultCannotBeWritten: a run that cannot leave a result
// says so on stderr and exits non-zero, since the daemon has nothing to read.
func TestRunFailsWhenTheResultCannotBeWritten(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	h.deps.WriteResult = func(string, sweeprunner.Result) error { return errors.New("disk full") }
	if code := h.run("--rig", "gastown", "--sha", testSHA, "--out", h.out, "--repo", "/repo.git"); code != FailureExit {
		t.Fatalf("exit code = %d, want %d", code, FailureExit)
	}
	if !strings.Contains(h.stderr.String(), "disk full") {
		t.Errorf("stderr = %q, want it to name the write failure", h.stderr.String())
	}
}

// equal reports whether two string slices hold the same entries in order.
func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
