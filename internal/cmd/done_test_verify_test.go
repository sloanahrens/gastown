package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/lintlock"
)

// initVerifyTestGoRepo builds a tiny Go module with two packages (pkga,
// pkgb) at HEAD "base", so tests can add commits on top and diff against
// that base to exercise changedGoPackages / runDefaultTestVerification.
func initVerifyTestGoRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	mustWrite("go.mod", "module example.test\n\ngo 1.21\n")
	mustWrite("pkga/a.go", "package pkga\n\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite("pkga/a_test.go", "package pkga\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n")
	mustWrite("pkgb/b.go", "package pkgb\n\nfunc Double(a int) int { return a * 2 }\n")
	mustWrite("pkgb/b_test.go", "package pkgb\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"bad double\")\n\t}\n}\n")

	runGit("add", ".")
	runGit("commit", "-q", "-m", "base")
	base = runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", base)
	return dir, base
}

// stubVerifyGate replaces the gate's slot-acquire and suite-runner hooks for
// the duration of a test, so the gate can be driven without the real
// container-gate slot (which would contend with every other suite on a shared
// Gas Town host) and without running `go test` over a real package tree.
func stubVerifyGate(
	t *testing.T,
	acquire func(townRoot, role string, timeout time.Duration) (func(), error),
	run func(ctx context.Context, worktree, script string, env []string, logFile *os.File) error,
) {
	t.Helper()
	if acquire != nil {
		prev := acquireVerifySlot
		acquireVerifySlot = acquire
		t.Cleanup(func() { acquireVerifySlot = prev })
	}
	if run != nil {
		prev := runVerifySuite
		runVerifySuite = run
		t.Cleanup(func() { runVerifySuite = prev })
	}
}

// stubLintLockRetryDelay shortens the waits between lint-lock retries so a
// test can drive the retry path (gt-xsty) without sleeping the real tens of
// seconds.
func stubLintLockRetryDelay(t *testing.T, delays ...time.Duration) {
	t.Helper()
	prev := lintlock.RetryDelay
	lintlock.RetryDelay = delays
	t.Cleanup(func() { lintlock.RetryDelay = prev })
}

// stubVerifyProgress shortens the gate's progress interval so a test can
// observe progress lines without waiting out the real one.
func stubVerifyProgress(t *testing.T, interval time.Duration) {
	t.Helper()
	prev := testVerifyProgressInterval
	testVerifyProgressInterval = interval
	t.Cleanup(func() { testVerifyProgressInterval = prev })
}

// stubGoBuildWholeModule replaces the whole-module build check that
// runDefaultTestVerification runs when a Go change resolves to no buildable
// package (a whole-package deletion) (gt-ytjh).
func stubGoBuildWholeModule(t *testing.T, err error) {
	t.Helper()
	prev := goBuildWholeModule
	goBuildWholeModule = func(string) error { return err }
	t.Cleanup(func() { goBuildWholeModule = prev })
}

// deletePkgb commits the removal of every file in the test repo's pkgb — a
// whole-package deletion, the diff shape gt-ytjh opened. It lives here (not in
// verify_integration_test.go) because both the unit and the integration test
// files need it, and the integration file only compiles with -tags integration.
func deletePkgb(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(dir, "pkgb")); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "delete pkgb")
}

// addNestedModule writes a module inside the test repo — its own go.mod, so the
// repo's module does not contain it — holding one .go file, and commits it. The
// package name need not match the directory, so one fixed name serves any depth
// ("sub", "plugins/dolt-snapshots").
func addNestedModule(t *testing.T, dir, relDir, modulePath string) {
	t.Helper()
	full := filepath.Join(dir, relDir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(full, "go.mod"), "module "+modulePath+"\n\ngo 1.21\n")
	writeFile(t, filepath.Join(full, "nested.go"), "package nestedmod\n\nfunc V() int { return 1 }\n")
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "add nested module at "+relDir)
}

// addMainPackage writes a package main at cmd/ in the test repo and commits it,
// which is what puts the module into moduleHasMainPackage's true branch and so
// switches goBuildWholeModule to its -o form.
func addMainPackage(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cmd", "main.go"), "package main\n\nfunc main() {}\n")
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "add a main package")
}

// stubLintVerifyTimeout shrinks the lint gate's budget so a test can drive a
// real expiry (gt-taoz) rather than sleeping out the 10m default.
func stubLintVerifyTimeout(t *testing.T, budget time.Duration) {
	t.Helper()
	prev := lintVerifyTimeout
	lintVerifyTimeout = budget
	t.Cleanup(func() { lintVerifyTimeout = prev })
}

func TestResolveTestVerifyBudgets(t *testing.T) {

	t.Run("defaults: slot cap from the slot CLI default, run budget at the floor", func(t *testing.T) {
		t.Parallel()
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test ./..."})
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the %s floor (gt-btw1: the gate runs the full suite, so there is nothing to scale by)", b.runTimeout, defaultTestVerifyRunFloor)
		}
		if !strings.Contains(b.runSource, "full test_command") {
			t.Errorf("runSource = %q, want it to explain the full-suite floor", b.runSource)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want %s", b.slotTimeout, defaultTestVerifySlotTimeout)
		}
	})

	t.Run("rig config overrides both budgets", func(t *testing.T) {
		t.Parallel()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "90m",
			TestVerifySlotTimeout: "15m",
		}
		b := resolveTestVerifyBudgets(mq)
		if b.runTimeout != 90*time.Minute {
			t.Errorf("runTimeout = %s, want 90m", b.runTimeout)
		}
		if b.slotSource != "merge_queue.test_verify_slot_timeout" {
			t.Errorf("slotSource = %q", b.slotSource)
		}
		if b.slotTimeout != 15*time.Minute {
			t.Errorf("slotTimeout = %s, want 15m", b.slotTimeout)
		}
	})

	t.Run("invalid durations fall back and say so", func(t *testing.T) {
		t.Parallel()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "banana",
			TestVerifySlotTimeout: "-5m",
		}
		b := resolveTestVerifyBudgets(mq)
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the derived floor", b.runTimeout)
		}
		if !strings.Contains(b.runSource, "banana") {
			t.Errorf("runSource = %q, want it to flag the rejected override", b.runSource)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want the default", b.slotTimeout)
		}
		if !strings.Contains(b.slotSource, "-5m") {
			t.Errorf("slotSource = %q, want it to flag the rejected override", b.slotSource)
		}
	})
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestLintFailureDetail pins what a failed lint attempt is allowed to tell the
// polecat. Only a lint that ran to completion may ask for findings to be fixed;
// lock contention, a lint that stopped at golangci-lint's own timeout, and a
// budget that ran out while the lint was still going (gt-taoz:
// run.allow-serial-runners makes a contended golangci-lint block on the lock
// rather than exit with the marker) all linted nothing (gt-xsty).
//
// The verdicts come from the final attempt rather than from a retry count, so a
// lint that contended once and then reported a real finding is sent back as a
// finding (gt-ijqw, om-gate attempt 1 major).
func TestLintFailureDetail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		outcome       lintlock.Outcome
		budgetExpired bool
		want          string
		notWant       string
	}{
		{
			name:    "a lint that completed and reported findings",
			want:    "fix the lint findings before resubmitting",
			notWant: "no finding is reported",
		},
		{
			name:    "lock contention the retries already proved",
			outcome: lintlock.Outcome{Err: errors.New("exit 2"), Waits: 2, Contended: true, Unfinished: true},
			want:    "held the lock across 2 retries",
			notWant: "fix the lint findings",
		},
		{
			name:    "golangci-lint's own timeout",
			outcome: lintlock.Outcome{Err: errors.New("exit 1"), Unfinished: true},
			want:    "stopped without reporting findings",
			notWant: "fix the lint findings",
		},
		{
			name:          "the budget ran out while the lint was still going",
			outcome:       lintlock.Outcome{Err: errors.New("signal: killed")},
			budgetExpired: true,
			want:          "killed at its 1m budget without finishing",
			notWant:       "fix the lint findings",
		},
		{
			name:          "both: contention is the more specific story",
			outcome:       lintlock.Outcome{Err: errors.New("exit 2"), Waits: 2, Contended: true, Unfinished: true},
			budgetExpired: true,
			want:          "held the lock across 2 retries",
			notWant:       "fix the lint findings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lintFailureDetail(tc.outcome, tc.budgetExpired, time.Minute)
			if !strings.Contains(got, tc.want) {
				t.Errorf("lintFailureDetail(%+v, %v) = %q, want it to contain %q", tc.outcome, tc.budgetExpired, got, tc.want)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("lintFailureDetail(%+v, %v) = %q, must not contain %q", tc.outcome, tc.budgetExpired, got, tc.notWant)
			}
		})
	}
}

// TestRunDefaultTestVerification_LintBudgetExpiry drives that case end to end:
// a lint that outlives its budget is killed by the gate's own context, and the
// refusal must not read as a finding. This is the failure gt-taoz opened —
// nothing in the log is attributable, because a lint blocked on a lock prints
// nothing at all.
func TestRunDefaultTestVerification_LintBudgetExpiry(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()
	stubLintVerifyTimeout(t, 250*time.Millisecond)

	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

	testMarker := filepath.Join(dir, "tests-ran")
	mq := &config.MergeQueueConfig{
		TestCommand:       "go test ./...",
		LintCommand:       "sleep 30",
		TestVerifyCommand: "echo ran > '" + testMarker + "'",
	}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-budget-role")
	if err == nil {
		t.Fatalf("expected a refusal when the lint outlives its budget, got result=%+v", result)
	}
	if !strings.Contains(err.Error(), "budget without finishing") {
		t.Errorf("a lint killed by its budget was not attributed to the budget: %v", err)
	}
	if strings.Contains(err.Error(), "fix the lint findings") {
		t.Errorf("a lint killed by its budget was reported as a lint finding: %v", err)
	}
	if _, statErr := os.Stat(testMarker); statErr == nil {
		t.Error("tests ran despite the lint never finishing")
	}
}

// TestRunDefaultTestVerification_DeletionRecordsWholeModule pins the marker a
// whole-package deletion leaves on the MR bead (gt-ytjh): when the changed .go
// files resolve to no buildable package and the whole-module build passes, the
// gate proceeds and records packages=["."].
func TestRunDefaultTestVerification_DeletionRecordsWholeModule(t *testing.T) {
	t.Parallel()
	stubNoContainers(t)
	stubGoBuildWholeModule(t, nil)
	stubVerifyGate(t,
		func(string, string, time.Duration) (func(), error) { return func() {}, nil },
		func(context.Context, string, string, []string, *os.File) error { return nil })

	dir, _ := initVerifyTestGoRepo(t)
	deletePkgb(t, dir)

	mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unit-delete-marker")
	if err != nil {
		t.Fatalf("runDefaultTestVerification: %v", err)
	}
	if !result.ran || !result.success {
		t.Fatalf("result = %+v, want ran=true success=true", result)
	}
	if len(result.packages) != 1 || result.packages[0] != "." {
		t.Errorf("packages = %v, want [.] — the whole-module build stands in for the deleted package", result.packages)
	}
}

// TestRunDefaultTestVerification_BrokenDeletionRefusalQuotesTheBuild forces the
// still-refusing half of gt-ytjh: a whole-package deletion whose importers no
// longer build must refuse, and the refusal has to show the build failure it is
// asking the polecat to fix rather than only telling it to fix the build.
func TestRunDefaultTestVerification_BrokenDeletionRefusalQuotesTheBuild(t *testing.T) {
	stubNoContainers(t)
	compilerSaid := "no required module provides package example.test/pkgb"
	stubGoBuildWholeModule(t, errors.New("go build ./...: exit status 1: "+compilerSaid))
	suiteRan := false
	stubVerifyGate(t,
		func(string, string, time.Duration) (func(), error) { return func() {}, nil },
		func(context.Context, string, string, []string, *os.File) error {
			suiteRan = true
			return nil
		})

	dir, _ := initVerifyTestGoRepo(t)
	deletePkgb(t, dir)

	mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unit-delete-refusal")
	if err == nil {
		t.Fatalf("runDefaultTestVerification: a deletion that breaks the build must refuse, got result=%+v", result)
	}
	if !strings.Contains(err.Error(), "unbuildable") {
		t.Errorf("error does not report the deletion refusal: %v", err)
	}
	if !strings.Contains(err.Error(), compilerSaid) {
		t.Errorf("the refusal does not surface the build failure it tells the polecat to fix: %v", err)
	}
	if suiteRan {
		t.Error("the suite ran for a deletion whose build is already broken")
	}
}

// TestRunDefaultTestVerification_UnresolvableGoChangeRefuses pins the refusal
// for a changed .go file that is still in the worktree but that `go list` will
// not resolve to a package: it is not a package deletion, and no whole-module
// build can stand in for it — for a package whose .go files are all excluded by
// build constraints `go build ./...` SUCCEEDS, having nothing to build. Every
// case below stubs the build to succeed and the suite to pass, so a gate that
// reached either would report success: that is what makes the refusal the
// assertion, rather than merely an error message.
func TestRunDefaultTestVerification_UnresolvableGoChangeRefuses(t *testing.T) {
	// addExcludedPkg adds a directory whose only .go file is excluded by its
	// build constraints — the shape whose whole-module build succeeds.
	addExcludedPkg := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "pkgexcluded"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pkgexcluded", "x.go"), []byte("//go:build never\n\npackage pkgexcluded\n\nfunc X() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "add a build-constraint-excluded package")
	}

	// runGate drives the gate with the build stubbed to pass, returning how
	// many times it was called: a gate that reached the build would go on to
	// succeed, so the count is what separates a refusal from a pass.
	runGate := func(t *testing.T, dir string) (testVerifyResult, error, int) {
		t.Helper()
		stubNoContainers(t)
		builds := 0
		prev := goBuildWholeModule
		goBuildWholeModule = func(string) error { builds++; return nil }
		t.Cleanup(func() { goBuildWholeModule = prev })
		stubVerifyGate(t,
			func(string, string, time.Duration) (func(), error) { return func() {}, nil },
			func(context.Context, string, string, []string, *os.File) error { return nil })

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unresolvable-role")
		return result, err, builds
	}

	t.Run("all-build-excluded .go file: refuses instead of falling through to a build that would pass", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addExcludedPkg(t, dir)

		result, err, builds := runGate(t, dir)
		if err == nil {
			t.Fatalf("runDefaultTestVerification: a .go file that resolves to no package must refuse, got result=%+v", result)
		}
		if !strings.Contains(err.Error(), "pkgexcluded") {
			t.Errorf("error does not name the unresolvable directory: %v", err)
		}
		if !strings.Contains(err.Error(), "not a package deletion") {
			t.Errorf("error does not say why a whole-module build cannot stand in here: %v", err)
		}
		if builds != 0 {
			t.Errorf("goBuildWholeModule ran %d time(s); the build cannot verify a file it never compiles", builds)
		}
	})

	t.Run("resolved package alongside an unresolvable one: still refuses", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		// pkga resolves, so the gate has a package list to scope a suite to —
		// the mixed case in which an unresolvable file escaped with no check.
		if err := os.WriteFile(filepath.Join(dir, "pkga", "a.go"), []byte("package pkga\n\nfunc Add(a, b int) int { return a + b + 1 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")
		addExcludedPkg(t, dir)

		result, err, builds := runGate(t, dir)
		if err == nil {
			t.Fatalf("runDefaultTestVerification: an unresolvable change must refuse even when another changed package resolved, got result=%+v", result)
		}
		if !strings.Contains(err.Error(), "pkgexcluded") {
			t.Errorf("error does not name the unresolvable directory: %v", err)
		}
		if builds != 0 {
			t.Errorf("goBuildWholeModule ran %d time(s), want 0", builds)
		}
	})
}

// TestRunDefaultTestVerification_NestedModuleChangeIsNotARefusal pins the shape
// a rig with a module under its tree produces (this rig ships
// plugins/dolt-snapshots): the changed .go file belongs to that module, so this
// module's `go list` fails on it while the file itself is fine. The gate must
// neither refuse it — that locks out every change to a nested module — nor
// claim to verify it, which it cannot.
func TestRunDefaultTestVerification_NestedModuleChangeIsNotARefusal(t *testing.T) {
	stubNoContainers(t)
	builds := 0
	prev := goBuildWholeModule
	goBuildWholeModule = func(string) error { builds++; return nil }
	t.Cleanup(func() { goBuildWholeModule = prev })
	suiteRan := false
	stubVerifyGate(t,
		func(string, string, time.Duration) (func(), error) { return func() {}, nil },
		func(context.Context, string, string, []string, *os.File) error {
			suiteRan = true
			return nil
		})

	dir, _ := initVerifyTestGoRepo(t)
	addNestedModule(t, dir, "plugins/example-sub", "example.test/plugin")

	mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unit-nested-module")
	if err != nil {
		t.Fatalf("runDefaultTestVerification: a change inside a nested module must not refuse: %v", err)
	}
	if !result.ran || !result.success {
		t.Fatalf("result = %+v, want ran=true success=true", result)
	}
	if len(result.packages) != 1 || result.packages[0] != "." {
		t.Errorf("packages = %v, want [.] — no package of this module can be scoped to the change", result.packages)
	}
	if !suiteRan {
		t.Error("the suite did not run for a nested-module change")
	}
	if builds != 0 {
		t.Errorf("goBuildWholeModule ran %d time(s); this module's build skips a nested module, so a failure there would not be the change's", builds)
	}
	// The skip has to survive in the artifact: a reader who sees the changed
	// .go file and no line about it cannot tell it was out of reach from it
	// having been tested.
	logBytes, readErr := os.ReadFile(result.logPath)
	if readErr != nil {
		t.Fatalf("reading verify log: %v", readErr)
	}
	logText := string(logBytes)
	for _, want := range []string{"not scoped:", "plugins/example-sub", "example.test/plugin"} {
		if !strings.Contains(logText, want) {
			t.Errorf("verify log is missing %q, so the directory the gate could not scope is invisible:\n%s", want, logText)
		}
	}
}

// TestRunDefaultTestVerification_MixedDeletionAndModificationBuildsWholeModule:
// a diff that deletes a package and modifies another still needs the
// whole-module build. The packages that import the deleted one are not in the
// changed set — `go list` resolves a package whose imports are broken — so the
// resolved modified package is not a substitute for the build.
func TestRunDefaultTestVerification_MixedDeletionAndModificationBuildsWholeModule(t *testing.T) {
	stubNoContainers(t)
	builds := 0
	prev := goBuildWholeModule
	goBuildWholeModule = func(string) error { builds++; return nil }
	t.Cleanup(func() { goBuildWholeModule = prev })
	stubVerifyGate(t,
		func(string, string, time.Duration) (func(), error) { return func() {}, nil },
		func(context.Context, string, string, []string, *os.File) error { return nil })

	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")
	deletePkgb(t, dir)

	mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unit-mixed-diff")
	if err != nil {
		t.Fatalf("runDefaultTestVerification: %v", err)
	}
	if builds != 1 {
		t.Errorf("goBuildWholeModule ran %d time(s), want 1 — a resolved package elsewhere in the diff does not cover the deletion", builds)
	}
	if len(result.packages) != 1 || !strings.HasSuffix(result.packages[0], "/pkga") {
		t.Errorf("packages = %v, want just the modified pkga recorded for the MR bead", result.packages)
	}
}

// TestModuleHasMainPackage covers the discriminator that decides whether the
// whole-module build can use -o: only a module with something to link may pass
// an output directory, and a listing that fails must not be reported as a main
// package (the build that follows says why in the compiler's own words).
func TestModuleHasMainPackage(t *testing.T) {
	t.Run("a module with a main package reports true", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addMainPackage(t, dir)
		if !moduleHasMainPackage(dir) {
			t.Error("moduleHasMainPackage = false, want true for a module with cmd/main.go")
		}
	})

	t.Run("a library-only module reports false", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		if moduleHasMainPackage(dir) {
			t.Error("moduleHasMainPackage = true, want false for a module of libraries (go build -o would refuse it)")
		}
	})

	t.Run("a directory that is not a Go module reports false", func(t *testing.T) {
		if moduleHasMainPackage(t.TempDir()) {
			t.Error("moduleHasMainPackage = true, want false when `go list` fails")
		}
	})
}

// TestRunDefaultTestVerification_ScopeLabelMatchesWhatRan: the gate's log
// header is the only record of whether a polecat verified the whole suite or
// just its changed packages, and an operator reading "scope=full" on a run
// that tested two packages cannot tell a scoped gate from a full one. The
// label was hardcoded to "full" even when merge_queue.test_verify_command
// replaced the suite with a {packages} variant, so it reported the opposite
// of what happened — the same class of defect as a check whose failure path
// emits its success value.
func TestRunDefaultTestVerification_ScopeLabelMatchesWhatRan(t *testing.T) {
	readScope := func(t *testing.T, worktree string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(worktree, constants.DirRuntime, "gt-done-verify.log"))
		if err != nil {
			t.Fatalf("read verify log: %v", err)
		}
		m := regexp.MustCompile(`scope=([a-z]+)`).FindStringSubmatch(string(data))
		if m == nil {
			t.Fatalf("verify log has no scope= label:\n%s", data)
		}
		return m[1]
	}

	// The gate skips when nothing changed, so give it a real diff to scope.
	touchBothPackages := func(t *testing.T, dir string) {
		t.Helper()
		for _, rel := range []string{"pkga/a.go", "pkgb/b.go"} {
			path := filepath.Join(dir, rel)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, []byte("\n// touched\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch both packages")
	}

	t.Run("full suite is labelled full", func(t *testing.T) {
		stubNoContainers(t)
		stubVerifyGate(t,
			func(string, string, time.Duration) (func(), error) { return func() {}, nil },
			func(context.Context, string, string, []string, *os.File) error { return nil })
		dir, _ := initVerifyTestGoRepo(t)
		touchBothPackages(t, dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		if _, err := runDefaultTestVerification(git.NewGit(dir), dir, "main", "main", mq, t.TempDir(), "test/scope-full"); err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if got := readScope(t, dir); got != "full" {
			t.Errorf("scope = %q, want %q", got, "full")
		}
	})

	t.Run("a scoped test_verify_command is labelled changed", func(t *testing.T) {
		stubNoContainers(t)
		stubVerifyGate(t,
			func(string, string, time.Duration) (func(), error) { return func() {}, nil },
			func(context.Context, string, string, []string, *os.File) error { return nil })
		dir, _ := initVerifyTestGoRepo(t)
		touchBothPackages(t, dir)
		mq := &config.MergeQueueConfig{
			TestCommand:       "go test ./...",
			TestVerifyCommand: "go test {packages}",
		}
		if _, err := runDefaultTestVerification(git.NewGit(dir), dir, "main", "main", mq, t.TempDir(), "test/scope-changed"); err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if got := readScope(t, dir); got != "changed" {
			t.Errorf("scope = %q, want %q — the log must say what actually ran", got, "changed")
		}
	})
}
