package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
func newHarness(t *testing.T, clone func(repo, sha, work string) error, sweep func(args []string, overrides map[string]string, stdout, stderr io.Writer) int) *harness {
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
		Sweep: func(args []string, overrides map[string]string, stdout, stderr io.Writer) int {
			h.sweepAt = args
			h.overrides = overrides
			if sweep == nil {
				return 0
			}
			return sweep(args, overrides, stdout, stderr)
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
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
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
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
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
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
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
			"fatal: Could not read from remote repository.\x1b[0m"
		h := newHarness(t, func(string, string, string) error { return errors.New(gitErr) },
			func([]string, map[string]string, io.Writer, io.Writer) int {
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
		// The error is two lines and a terminal escape; the summary the daemon
		// parses is one line and holds no control character.
		if strings.ContainsAny(r.Tiers[0].Summary, "\n\r\x1b") {
			t.Errorf("summary %q holds a line break or a control character", r.Tiers[0].Summary)
		}
	})

	t.Run("a tier the sweep printed no line for is RED", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, _ map[string]string, stdout, _ io.Writer) int {
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
				func([]string, map[string]string, io.Writer, io.Writer) int {
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
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
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

// TestCloneWithReportsGitFailures: the clone the result records is the one this
// file builds (safe.directory on the read-only mount, no hard links, a detached
// checkout), and a step that fails is the error the run reports rather than a
// tree the sweep would test at the wrong commit.
func TestCloneWithReportsGitFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fails func(args []string) bool
		out   string
		want  string
		calls int
	}{
		{
			name:  "the clone",
			fails: func(args []string) bool { return args[2] == "clone" },
			out:   "fatal: '/repo.git' does not appear to be a git repository",
			want:  "git clone /repo.git: fatal: '/repo.git' does not appear to be a git repository",
			calls: 1, // the checkout is not attempted on a tree that was never made
		},
		{
			name:  "the checkout",
			fails: func(args []string) bool { return args[0] == "checkout" },
			out:   "fatal: reference is not a tree",
			want:  "git checkout " + testSHA + ": fatal: reference is not a tree",
			calls: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(_ string, args ...string) (int, string) {
				calls = append(calls, args)
				if tc.fails(args) {
					return 128, tc.out
				}
				return 0, ""
			}
			err := cloneWith(run, "/repo.git", testSHA, "/work")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to hold %q", err, tc.want)
			}
			if len(calls) != tc.calls {
				t.Fatalf("git ran %d times, want %d: %q", len(calls), tc.calls, calls)
			}
			if got, want := strings.Join(calls[0], " "), "-c safe.directory=/repo.git clone --no-hardlinks /repo.git /work"; got != want {
				t.Errorf("clone ran %q, want %q", got, want)
			}
			if tc.calls > 1 {
				if got, want := strings.Join(calls[1], " "), "checkout --detach "+testSHA; got != want {
					t.Errorf("checkout ran %q, want %q", got, want)
				}
			}
		})
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

// TestParseArgsHoldsTheRenderedSpecRules: every rule sweeprunner.Spec.Validate
// has over a field the entrypoint receives refuses both the spec and the
// command line, so the daemon cannot render a run the entrypoint will not
// start. The rules over the fields the container never passes (image, limits,
// context, run id) have no flag; those are covered by sweeprunner's own tests.
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
		{"a tier named twice", func(s *sweeprunner.Spec) { s.Tiers = []string{"integration", "integration"} },
			[]string{"--rig", "gastown", "--sha", testSHA, "--tiers", "integration,integration"}},
		{"zero jobs", func(s *sweeprunner.Spec) { s.Jobs = 0 },
			[]string{"--rig", "gastown", "--sha", testSHA, "--jobs", "0"}},
		{"more jobs than a spec renders", func(s *sweeprunner.Spec) { s.Jobs = 17 },
			[]string{"--rig", "gastown", "--sha", testSHA, "--jobs", "17"}},
		{"an output directory that is relative", func(s *sweeprunner.Spec) { s.OutDir = "out" },
			[]string{"--rig", "gastown", "--sha", testSHA, "--out", "out", "--repo", sweeprunner.RepoMount}},
		{"an output directory with a colon", func(s *sweeprunner.Spec) { s.OutDir = "/a:b" },
			[]string{"--rig", "gastown", "--sha", testSHA, "--out", "/a:b", "--repo", sweeprunner.RepoMount}},
		{"a repository that is relative", func(s *sweeprunner.Spec) { s.RepoDir = "repo" },
			[]string{"--rig", "gastown", "--sha", testSHA, "--out", sweeprunner.OutputDir, "--repo", "repo"}},
		{"a repository with a colon", func(s *sweeprunner.Spec) { s.RepoDir = "/a:b" },
			[]string{"--rig", "gastown", "--sha", testSHA, "--out", sweeprunner.OutputDir, "--repo", "/a:b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := valid
			tc.spoil(&bad)
			if err := bad.Validate(); err == nil {
				t.Errorf("sweeprunner.Spec.Validate accepted %+v", bad)
			}
			args := tc.args
			if !slices.Contains(args, "--out") {
				args = append(args, "--out", sweeprunner.OutputDir)
			}
			if !slices.Contains(args, "--repo") {
				args = append(args, "--repo", sweeprunner.RepoMount)
			}
			if _, err := parseArgs(args, nil); err == nil {
				t.Errorf("parseArgs accepted %q", args)
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

// TestRunTakesTheVerdictFromTheSweepExit: the sweep's exit status is the
// verdict and the summary line only supplies its counts. A GREEN line beside a
// non-zero exit is RED, a tier the sweep never reported is RED with the sweep's
// own complaint, and a sweep that produced no exit status at all is a run that
// cannot report a result.
func TestRunTakesTheVerdictFromTheSweepExit(t *testing.T) {
	t.Parallel()

	t.Run("a clean exit keeps the GREEN line", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
			fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=7 failed=0 skipped=0 (logs %s) in 1s\n",
				overrides[tiersweep.EnvLogDir])
			return 0
		})
		verdict, summary := runAndRead(t, h, "integration")
		if verdict.Verdict != "GREEN" {
			t.Errorf("integration = %+v, want GREEN", verdict)
		}
		if strings.Contains(summary, "sweep exited") {
			t.Errorf("summary %q names a contradiction the run did not have", summary)
		}
	})

	t.Run("a non-zero exit turns a GREEN line RED", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
			fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=7 failed=0 skipped=0 (logs %s) in 1s\n",
				overrides[tiersweep.EnvLogDir])
			return 1
		})
		verdict, summary := runAndRead(t, h, "integration")
		if verdict.Verdict != "RED" {
			t.Errorf("integration = %+v, want RED from the sweep's exit status", verdict)
		}
		if !strings.Contains(summary, "sweep exited 1") {
			t.Errorf("summary %q does not record the exit status beside a GREEN line", summary)
		}
		// The counts the sweep printed are still the counts the result holds.
		if verdict.Passed != 7 {
			t.Errorf("passed = %d, want the printed 7", verdict.Passed)
		}
	})

	t.Run("a non-zero exit beside one RED tier leaves the GREEN tier GREEN", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
			fmt.Fprintf(stdout, "tier-sweep: shell GREEN passed=9 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
			fmt.Fprintf(stdout, "tier-sweep: integration RED passed=6 failed=1 skipped=0 failed: pkg/a (logs %s) in 2s\n", overrides[tiersweep.EnvLogDir])
			return 1
		})
		got := runAndReadAll(t, h, "shell", "integration")
		if got["shell"].Result.Verdict != "GREEN" {
			t.Errorf("shell = %+v, want GREEN: the sweep ran it and it passed, the failure is integration's", got["shell"].Result)
		}
		if strings.Contains(got["shell"].Summary, "sweep exited") {
			t.Errorf("the GREEN tier's summary %q names a contradiction it does not have", got["shell"].Summary)
		}
		if got["integration"].Result.Verdict != "RED" || strings.Contains(got["integration"].Summary, "sweep exited") {
			t.Errorf("integration = %+v, summary %q, want the sweep's own RED line unchanged", got["integration"].Result, got["integration"].Summary)
		}
	})

	t.Run("a non-zero exit beside only GREEN lines turns each RED, naming the contradiction", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
			fmt.Fprintf(stdout, "tier-sweep: shell GREEN passed=9 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
			fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=7 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
			return 3
		})
		got := runAndReadAll(t, h, "shell", "integration")
		for _, tier := range []string{"shell", "integration"} {
			if !strings.Contains(got[tier].Summary, " RED ") || !strings.Contains(got[tier].Summary, "sweep exited 3") {
				t.Errorf("%s summary %q, want RED with the exit named: no tier can be blamed", tier, got[tier].Summary)
			}
			if got[tier].Result.Verdict != "RED" {
				t.Errorf("%s = %+v, want RED", tier, got[tier].Result)
			}
		}
	})

	t.Run("a non-zero exit with no line is RED with the sweep's words", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, _ map[string]string, _, stderr io.Writer) int {
			fmt.Fprintln(stderr, "tier-sweep: could not count the integration packages")
			return 1
		})
		verdict, summary := runAndRead(t, h, "integration")
		if verdict.Verdict != "RED" {
			t.Errorf("integration = %+v, want RED", verdict)
		}
		for _, want := range []string{"exit 1", "could not count the integration packages"} {
			if !strings.Contains(summary, want) {
				t.Errorf("summary %q does not hold %q", summary, want)
			}
		}
	})

	t.Run("a sweep that never exited is an error, not a verdict", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func([]string, map[string]string, io.Writer, io.Writer) int {
			return sweepNoExit
		})
		if code := h.run("--rig", "gastown", "--sha", testSHA, "--out", h.out, "--repo", "/repo.git"); code != FailureExit {
			t.Fatalf("exit code = %d, want %d", code, FailureExit)
		}
		if !strings.Contains(h.stderr.String(), "did not exit") {
			t.Errorf("stderr = %q, want it to name the sweep that did not exit", h.stderr.String())
		}
		if h.resultFileExists(t) {
			t.Error("a run whose sweep never exited wrote a result")
		}
	})
}

// runAndRead runs one integration-tier sweep against h and returns its verdict
// and stored summary line.
func runAndRead(t *testing.T, h *harness, tier string) (sweeprunner.TierResult, string) {
	t.Helper()
	if code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", tier, "--out", h.out, "--repo", "/repo.git"); code != 0 {
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
	return verdicts[tier], r.Tiers[0].Summary
}

// tierRead is one tier of a read result: its verdict and the summary line the
// result holds for it.
type tierRead struct {
	Result  sweeprunner.TierResult
	Summary string
}

// runAndReadAll runs the runner for several tiers and returns each tier's
// verdict and summary line.
func runAndReadAll(t *testing.T, h *harness, tiers ...string) map[string]tierRead {
	t.Helper()
	if code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", strings.Join(tiers, ","), "--out", h.out, "--repo", "/repo.git"); code != 0 {
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
	out := map[string]tierRead{}
	for _, tier := range r.Tiers {
		out[tier.Tier] = tierRead{Result: verdicts[tier.Tier], Summary: tier.Summary}
	}
	return out
}

// TestRunSanitisesTheSweepOutput: whatever the sweep prints is data a reader is
// shown and the daemon acts on, so the runner strips what a terminal would
// interpret, caps what would cost memory, and takes the log path from its own
// run directory rather than from the line (gt-dibnm).
func TestRunSanitisesTheSweepOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		line        string
		wantVerdict string
		check       func(t *testing.T, logDir, summary string)
	}{
		{
			name:        "a terminal escape",
			line:        "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs %s) \x1b[31min 1s\x1b[0m\n",
			wantVerdict: "GREEN",
			check: func(t *testing.T, _, summary string) {
				if strings.Contains(summary, "\x1b") {
					t.Errorf("summary %q holds a terminal escape", summary)
				}
			},
		},
		{
			name:        "a NUL byte",
			line:        "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs %s) in 1s\x00\n",
			wantVerdict: "GREEN",
			check: func(t *testing.T, _, summary string) {
				if strings.ContainsRune(summary, 0) {
					t.Errorf("summary %q holds a NUL byte", summary)
				}
			},
		},
		{
			name:        "a line larger than the cap",
			line:        "tier-sweep: integration RED passed=0 failed=1 skipped=0 failed: " + strings.Repeat("x", 1<<20) + " (logs %s) in 1s\n",
			wantVerdict: "RED",
			check: func(t *testing.T, _, summary string) {
				if len(summary) > maxSummaryBytes {
					t.Errorf("summary is %d bytes, want at most %d", len(summary), maxSummaryBytes)
				}
				// A line longer than the capture cannot be kept whole, so the run
				// records what it lost rather than passing the fragment off as a
				// verdict.
				if !strings.Contains(summary, "output truncated:") {
					t.Errorf("summary %q does not record the output the run dropped", summary)
				}
			},
		},
		{
			name:        "invalid UTF-8",
			line:        "tier-sweep: integration RED passed=0 failed=1 skipped=0 failed: \xff\xfe (logs %s) in 1s\n",
			wantVerdict: "RED",
			check: func(t *testing.T, _, summary string) {
				if !utf8.ValidString(summary) {
					t.Errorf("summary %q is not valid UTF-8", summary)
				}
			},
		},
		{
			name:        "a log path the sweep chose",
			line:        "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs ../../etc) in 1s\n",
			wantVerdict: "GREEN",
			check: func(t *testing.T, logDir, summary string) {
				if !strings.Contains(summary, "(logs "+logDir+")") {
					t.Errorf("summary %q does not name the run's own log directory %q", summary, logDir)
				}
				if strings.Contains(summary, "../../etc") {
					t.Errorf("summary %q keeps the path the sweep chose", summary)
				}
			},
		},
		{
			name:        "a line that looks like JSON",
			line:        "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 failed: {\"verdict\":\"RED\"} (logs %s) in 1s\n",
			wantVerdict: "GREEN",
			check: func(t *testing.T, logDir, summary string) {
				// The payload stays the text it was, and the path is still the
				// runner's own: nothing in the line is read as a document or a path.
				if !strings.Contains(summary, "(logs "+logDir+")") {
					t.Errorf("summary %q does not name the run's own log directory", summary)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
				fmt.Fprintf(stdout, tc.line, overrides[tiersweep.EnvLogDir])
				return 0
			})
			verdict, summary := runAndRead(t, h, "integration")
			// A line the runner leaves unsafe fails Read, which is the whole
			// point: runAndRead would have failed the case already.
			if verdict.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want the line's own %q beside a clean exit", verdict.Verdict, tc.wantVerdict)
			}
			tc.check(t, filepath.Join(h.out, "logs"), summary)
		})
	}
}

// TestRunKeepsTheEndOfTheSweepOutput: the verdicts and the sweep's own complaint
// come last, so the runner keeps the end of each stream — a verdict printed
// after megabytes of progress is still read, and a reason is quoted from the
// sweep's last words — and says in the result what it had to drop (gt-dibnm).
func TestRunKeepsTheEndOfTheSweepOutput(t *testing.T) {
	t.Parallel()

	t.Run("a verdict after more than the cap is still found", func(t *testing.T) {
		t.Parallel()
		progress := "go: downloading example.com/x v1.2.3 (100%)\n"
		noise := strings.Repeat(progress, 3*maxCaptureBytes/len(progress)+1)
		var printed int
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
			line := fmt.Sprintf("tier-sweep: integration GREEN passed=7 failed=0 skipped=0 (logs %s) in 1s\n",
				overrides[tiersweep.EnvLogDir])
			io.WriteString(stdout, noise)
			io.WriteString(stdout, line)
			printed = len(noise) + len(line)
			return 0
		})
		verdict, summary := runAndRead(t, h, "integration")
		if verdict.Verdict != "GREEN" {
			t.Errorf("integration = %+v, want the GREEN line printed after %d bytes of progress", verdict, len(noise))
		}
		want := fmt.Sprintf("output truncated: %d bytes dropped from the head", printed-maxCaptureBytes)
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not hold %q", summary, want)
		}
	})

	t.Run("output under the cap is kept whole", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
			fmt.Fprintln(stdout, "tier-sweep: integration: 66 packages")
			fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=66 failed=0 skipped=0 (logs %s) in 2m0s\n",
				overrides[tiersweep.EnvLogDir])
			return 0
		})
		verdict, summary := runAndRead(t, h, "integration")
		if verdict.Verdict != "GREEN" || verdict.Passed != 66 {
			t.Errorf("integration = %+v, want GREEN with the printed 66", verdict)
		}
		if strings.Contains(summary, "output truncated:") {
			t.Errorf("summary %q reports dropped output the run kept whole", summary)
		}
	})

	t.Run("stderr over the cap keeps its last lines", func(t *testing.T) {
		t.Parallel()
		progress := "internal/daemon: package still building\n"
		noise := strings.Repeat(progress, maxStderrCapture/len(progress)+1)
		const complaint = "tier-sweep: could not count the integration packages\n"
		var printed int
		h := newHarness(t, nil, func(_ []string, _ map[string]string, _, stderr io.Writer) int {
			io.WriteString(stderr, noise)
			io.WriteString(stderr, complaint)
			printed = len(noise) + len(complaint)
			return 1
		})
		verdict, summary := runAndRead(t, h, "integration")
		if verdict.Verdict != "RED" {
			t.Errorf("integration = %+v, want RED", verdict)
		}
		if !strings.Contains(summary, "could not count the integration packages") {
			t.Errorf("summary %q does not quote the sweep's last line", summary)
		}
		want := fmt.Sprintf("output truncated: %d bytes dropped from the head", printed-maxStderrCapture)
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not hold %q", summary, want)
		}
	})
}

// TestProcessSweepHandsTheSweepItsOverrides: the overrides the entrypoint passes
// are what the sweep's own option reads answer with, before the process
// environment, and what every child it starts inherits; a key with no override
// still reads the process (gt-dibnm).
func TestProcessSweepHandsTheSweepItsOverrides(t *testing.T) {
	t.Parallel()

	t.Run("an override answers before the process", func(t *testing.T) {
		t.Parallel()
		overrides := map[string]string{
			tiersweep.EnvLogDir: "/out/logs",
			tiersweep.EnvTested: testSHA,
			envJobs:             "4",
		}
		base := []string{"PATH=/bin", tiersweep.EnvLogDir + "=/elsewhere"}
		args := []string{"--root", "/work", "integration"}
		var gotArgs, gotEnv []string
		var logDir, unset string
		var ran bool
		run := func(args []string, stdout, _ io.Writer, d tiersweep.Deps) int {
			ran = true
			gotArgs, gotEnv = args, d.Environ
			logDir = d.Host.Env(tiersweep.EnvLogDir)
			unset = d.Host.Env("GT_SWEEP_NOT_SET")
			return 0
		}
		var stdout, stderr bytes.Buffer
		code := sweepWith(run, args, overrides, func(string) string { return "from the process" }, base,
			os.ReadFile, time.Now, &stdout, &stderr)
		if code != 0 || !ran {
			t.Fatalf("code = %d, ran = %v: the overrides did not reach a sweep", code, ran)
		}
		if !equal(gotArgs, args) {
			t.Errorf("sweep args = %q, want %q", gotArgs, args)
		}
		if logDir != "/out/logs" {
			t.Errorf("the sweep read %s = %q, want the override %q", tiersweep.EnvLogDir, logDir, "/out/logs")
		}
		if unset != "from the process" {
			t.Errorf("a key with no override read %q, want the process's own value", unset)
		}
		// The children inherit the process environment with the overrides after
		// it, in a fixed order.
		want := append(append([]string{}, base...),
			envJobs+"=4", tiersweep.EnvLogDir+"=/out/logs", tiersweep.EnvTested+"="+testSHA)
		if !equal(gotEnv, want) {
			t.Errorf("child environment = %q, want %q", gotEnv, want)
		}
	})

	t.Run("with no override the sweep reads the process", func(t *testing.T) {
		t.Parallel()
		var logDir string
		run := func(_ []string, _, _ io.Writer, d tiersweep.Deps) int {
			logDir = d.Host.Env(tiersweep.EnvLogDir)
			return 0
		}
		sweepWith(run, nil, nil, func(string) string { return "/process/logs" }, nil,
			os.ReadFile, time.Now, io.Discard, io.Discard)
		if logDir != "/process/logs" {
			t.Errorf("the sweep read %s = %q, want the process's own %q", tiersweep.EnvLogDir, logDir, "/process/logs")
		}
	})
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
