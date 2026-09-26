package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
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
//
// It also stands the container watch's docker listing down (gt-0ss4). The
// watch is the gate's one `docker ps` caller, and a slot-free gate run now
// takes a baseline listing: a stray dolt/testcontainers/ryuk container on a
// shared host would otherwise decide a gate test's outcome. A test that drives
// the watch overrides the listing itself, after this call.
func stubVerifyGate(
	t *testing.T,
	acquire func(townRoot, role string, timeout time.Duration) (func(), error),
	run func(ctx context.Context, worktree, script string, env []string, logFile *os.File) error,
) {
	t.Helper()
	stubNoContainers(t)
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
// verify_integration_test.go) because both test files need it.
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

// gt-0hbm: the slot decision and the environment it is made for have to be one
// fact, so for a Go rig the switch the gate resolves and the switch value the
// run's environment ends up carrying must always agree — including when the
// session's own value says the opposite.
func TestResolveContainerSwitchStatesTheSlotDecision(t *testing.T) {
	cases := []struct {
		name     string
		isGoRig  bool
		commands []string
		ambient  string
		want     string // the switch value the run's environment must carry
		slot     bool
	}{
		{"Go rig that asks nothing", true, []string{"GOFLAGS=-p=8 make test"}, "", "0", false},
		{"Go rig asking inline", true, []string{dockerTestsEnv + "=1 go test ./..."}, "", "1", true},
		{"Go rig asking after the packages token", true, []string{"make test", dockerTestsEnv + "=1 go test {packages}"}, "", "1", true},
		{"Go rig that asks nothing, with the session's opt-in on", true, []string{"GOFLAGS=-p=8 make test"}, "1", "0", false},
		{"Go rig that asks inline, with the session's opt-in off", true, []string{dockerTestsEnv + "=1 go test ./..."}, "0", "1", true},
		{"Go rig explicitly opting out, with the session's opt-in on", true, []string{dockerTestsEnv + "=0 go test ./..."}, "1", "0", false},
		{"non-Go rig, opaque command", false, []string{"npm test"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dockerTestsEnv, tc.ambient)
			got := resolveContainerSwitch(tc.isGoRig, tc.commands...)
			if got.value != tc.want || got.slot != tc.slot {
				t.Errorf("resolveContainerSwitch = %+v, want value %q slot %t", got, tc.want, tc.slot)
			}
			// The run's environment is built from the same decision, and the
			// rig prefix deliberately disagrees with it: the environment is
			// the value the child reads, so the leak a pass-through would
			// leave here is the hole gt-0hbm closed.
			env := verifyGateEnv([]string{"GOFLAGS=-p=8", dockerTestsEnv + "=0"}, got.value)
			var seen []string
			for _, kv := range env {
				if strings.HasPrefix(kv, dockerTestsEnv+"=") {
					seen = append(seen, strings.TrimPrefix(kv, dockerTestsEnv+"="))
				}
			}
			if tc.want == "" {
				// A non-Go rig's environment is left alone: the gate cannot
				// know what the switch means to a command it cannot read.
				if !containsEnv(env, dockerTestsEnv+"=0") || containsEnv(env, dockerTestsEnv+"=1") {
					t.Errorf("non-Go rig env carries %v, want the rig's own value untouched and no switch the gate invented", seen)
				}
				return
			}
			if len(seen) != 1 || seen[0] != tc.want {
				t.Errorf("run env carries %v, want exactly [%s]: the value the child reads is the value the slot decision was made for", seen, tc.want)
			}
			if got.slot != (seen[0] == "1") {
				t.Errorf("slot=%t but the run's environment says %s — a container could start outside the slot, or a slot be held for nothing", got.slot, seen[0])
			}
		})
	}
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
// budget that ran out while the lint was still going all linted nothing
// (gt-xsty, gt-taoz).
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

// TestRunDefaultTestVerification_LintFindingAfterContention pins gt-1g9n's
// finding where it can still go wrong: a contended lint is retried, so the
// attempt that decides the verdict is not always the first one, and the gate
// must read that attempt's output rather than the log it shares with the
// attempts before it. Reading the whole log reports a real finding as
// contention — "nothing was linted; re-run", about the log the message cites —
// the misreport a retry count produces (om major on gt-wisp-bob, gt-1g9n;
// policy fixed under gt-ijqw).
//
// Both of gt done's lint gates share one log across attempts; only this one
// acts on the verdict the byte offset decides — the --pre-verified gate reports
// the failing gate's name and exit code and drops it, and the refinery's gate
// buffers each attempt separately. The sibling shapes in
// done_test_verify_lint_test.go cannot catch the offset going wrong (each
// prints the marker on every attempt, or runs a single attempt). This test
// needs no real suite run — the lint refuses first.
//
// Serial because stubLintLockRetryDelay swaps lintlock's retry schedule and
// restores it (gt-k317).
func TestRunDefaultTestVerification_LintFindingAfterContention(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()
	stubLintLockRetryDelay(t, time.Millisecond, time.Millisecond)

	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

	// Attempt 1 loses golangci-lint's lock and analyses nothing; attempt 2 gets
	// it and reports one real finding. The counter is both the switch between
	// the two attempts and the attempt count the assertions below read.
	counter := filepath.Join(dir, "lint-attempts")
	lint := fmt.Sprintf(`echo x >> %q; `+
		`if [ "$(wc -l < %q)" -lt 2 ]; then echo 'Error: parallel golangci-lint is running' >&2; exit 2; fi; `+
		`echo 'pkga/a.go:1:1: something is wrong (fakelint)'; exit 1`, counter, counter)

	testMarker := filepath.Join(dir, "tests-ran")
	mq := &config.MergeQueueConfig{
		TestCommand:       "go test ./...",
		LintCommand:       lint,
		TestVerifyCommand: "echo ran > '" + testMarker + "'",
	}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-finding-after-contention-role")
	if err == nil {
		t.Fatalf("expected a lint refusal, got result=%+v", result)
	}
	for _, want := range []string{
		"lint-verify failed",
		"exit 1",
		"fakelint",
		"fix the lint findings",
		"no tests were run",
		// The first attempt's marker really is in the log the refusal quotes,
		// so the assertion below is about the verdict, not about the marker
		// having gone missing from the gate's output.
		"parallel golangci-lint is running",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "held the lock") {
		t.Errorf("a finding reported by the retry was attributed to lock contention: %v", err)
	}
	if result.lintRan {
		t.Errorf("the lint was recorded as having run: %+v", result)
	}
	if got, readErr := os.ReadFile(counter); readErr != nil {
		t.Errorf("reading the lint attempt counter %s: %v", counter, readErr)
	} else if got := strings.Count(string(got), "\n"); got != 2 {
		t.Errorf("lint attempts = %d, want 2 (the collision, then the attempt that found something)", got)
	}
	if _, statErr := os.Stat(testMarker); statErr == nil {
		t.Error("tests ran despite the lint refusing the submission")
	}
}

// TestRunDefaultTestVerification_DeletionRecordsWholeModule pins the marker a
// whole-package deletion leaves on the MR bead (gt-ytjh): when the changed .go
// files resolve to no buildable package and the whole-module build passes, the
// gate proceeds and records packages=["."].
//
// Serial because stubGoBuildWholeModule swaps a package variable and restores
// it (gt-k317). Install-once is not available: the sibling tests need a
// different stub, one that fails the build rather than passing it.
func TestRunDefaultTestVerification_DeletionRecordsWholeModule(t *testing.T) {
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

// TestRunDefaultTestVerification_NestedModuleChangeIsBuiltInItsOwnModule pins
// the shape a rig with a module under its tree produces (this rig ships
// plugins/dolt-snapshots): the changed .go file belongs to that module, so this
// module's `go list` fails on it while the file itself is fine. The gate must
// not refuse it — that locks out every change to a nested module — and it
// cannot verify it with this module's suite, so it builds that module where it
// lives.
func TestRunDefaultTestVerification_NestedModuleChangeIsBuiltInItsOwnModule(t *testing.T) {
	stubNoContainers(t)
	var builds []string
	prev := goBuildWholeModule
	goBuildWholeModule = func(dir string) error { builds = append(builds, dir); return nil }
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
	// The build has to be of the module that owns the file. Building the
	// worktree here would compile the wrong tree: this module's `./...` skips a
	// nested module, so its success would say nothing about the change.
	want := filepath.Join(dir, "plugins", "example-sub")
	if len(builds) != 1 || builds[0] != want {
		t.Errorf("goBuildWholeModule called with %v, want exactly [%s]", builds, want)
	}
	// The switch has to survive in the artifact: a reader who sees the changed
	// .go file and no line about it cannot tell it was out of reach from it
	// having been tested.
	logBytes, readErr := os.ReadFile(result.logPath)
	if readErr != nil {
		t.Fatalf("reading verify log: %v", readErr)
	}
	logText := string(logBytes)
	for _, want := range []string{"not scoped:", "plugins/example-sub", "example.test/plugin", "built that module in its own directory"} {
		if !strings.Contains(logText, want) {
			t.Errorf("verify log is missing %q, so the directory the gate could not scope is invisible:\n%s", want, logText)
		}
	}
}

// TestRunDefaultTestVerification_NestedModuleBuildFailureRefuses is the failing
// half of the nested-module build: `go list` does not typecheck, so a file that
// resolves inside its own module can still be one this build is the only check
// of — including when this module requires the nested one and so compiles it as
// a dependency. The failure has to name the module and keep the compiler's
// output.
func TestRunDefaultTestVerification_NestedModuleBuildFailureRefuses(t *testing.T) {
	stubNoContainers(t)
	compilerSaid := `plugins/example-sub/nested.go:5:9: cannot use "x" (untyped string constant) as int value in return statement`
	prev := goBuildWholeModule
	goBuildWholeModule = func(string) error { return errors.New("go build ./...: exit status 1: " + compilerSaid) }
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
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unit-nested-module-build")
	if err == nil {
		t.Fatalf("runDefaultTestVerification: a nested module that does not build must refuse, got result=%+v", result)
	}
	if !strings.Contains(err.Error(), "plugins/example-sub") {
		t.Errorf("the refusal does not name the nested module: %v", err)
	}
	if !strings.Contains(err.Error(), compilerSaid) {
		t.Errorf("the refusal does not surface the build failure it tells the polecat to fix: %v", err)
	}
	if suiteRan {
		t.Error("the suite ran for a nested module whose build is already broken")
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

// TestRunDefaultTestVerification_MixedDeletionAndNestedModuleBuildsBoth pins
// gt-dlsd: a diff that deletes a whole package AND touches a nested module
// used to run only the deletion's whole-module build (a switch's first
// matching case wins, so the nested module's build never ran) while the log
// still printed the nested module's "built that module in its own directory"
// line — a false record of a check that never happened. Both shapes can
// appear in the same diff, so both builds must run.
func TestRunDefaultTestVerification_MixedDeletionAndNestedModuleBuildsBoth(t *testing.T) {
	stubNoContainers(t)
	var builds []string
	prev := goBuildWholeModule
	goBuildWholeModule = func(dir string) error { builds = append(builds, dir); return nil }
	t.Cleanup(func() { goBuildWholeModule = prev })
	stubVerifyGate(t,
		func(string, string, time.Duration) (func(), error) { return func() {}, nil },
		func(context.Context, string, string, []string, *os.File) error { return nil })

	dir, _ := initVerifyTestGoRepo(t)
	addNestedModule(t, dir, "plugins/example-sub", "example.test/plugin")
	deletePkgb(t, dir)

	mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
	g := git.NewGit(dir)
	result, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/unit-mixed-deletion-nested")
	if err != nil {
		t.Fatalf("runDefaultTestVerification: %v", err)
	}
	wantWorktreeBuild := dir
	wantNestedBuild := filepath.Join(dir, "plugins", "example-sub")
	if len(builds) != 2 {
		t.Fatalf("goBuildWholeModule ran %v, want exactly two builds — one for the deletion, one for the nested module", builds)
	}
	foundWorktree, foundNested := false, false
	for _, b := range builds {
		switch b {
		case wantWorktreeBuild:
			foundWorktree = true
		case wantNestedBuild:
			foundNested = true
		}
	}
	if !foundWorktree {
		t.Errorf("builds = %v, missing the deletion's whole-module build of %s", builds, wantWorktreeBuild)
	}
	if !foundNested {
		t.Errorf("builds = %v, missing the nested module's build of %s", builds, wantNestedBuild)
	}

	logBytes, readErr := os.ReadFile(result.logPath)
	if readErr != nil {
		t.Fatalf("reading verify log: %v", readErr)
	}
	logText := string(logBytes)
	for _, want := range []string{"not scoped:", "plugins/example-sub", "built that module in its own directory"} {
		if !strings.Contains(logText, want) {
			t.Errorf("verify log is missing %q:\n%s", want, logText)
		}
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
		data, err := os.ReadFile(testVerifyLogPath(worktree))
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

// TestRunDefaultTestVerification_SlotWaitIsBounded covers the bounded-wait
// path gt-7dxw asks for: `gt done` waits for the container-gate slot on its
// own, for a wait BOUNDED by the rig's configured cap, printing a progress
// line while it waits, and fails with a message that tells the polecat what to
// do instead of looping.
//
// This is the shape the incident's improvised retry loop was working around,
// so the three things a polecat needs are asserted together: the cap the rig
// configured is the cap the acquire actually gets (not the 60m default), the
// wait ends when that cap expires rather than re-arming, and both the progress
// line and the giving-up message reach the polecat.
func TestRunDefaultTestVerification_SlotWaitIsBounded(t *testing.T) {
	const slotCap = 80 * time.Millisecond
	dir, _ := initVerifyTestGoRepo(t)
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

	// One progress tick must land inside the cap, or the pane looks hung for
	// the whole wait — the failure gt-pnkd's victims could not diagnose.
	stubVerifyProgress(t, slotCap/4)

	var (
		gotTimeout    time.Duration
		acquires      int
		acquireWindow time.Duration
	)
	stubVerifyGate(t,
		func(_ string, _ string, timeout time.Duration) (func(), error) {
			acquires++
			gotTimeout = timeout
			// Stand in for the real slot.AcquirePool: block until the cap
			// expires, then give up with the slot package's own error text.
			started := time.Now()
			time.Sleep(timeout)
			acquireWindow = time.Since(started)
			return nil, errors.New("timed out after " + timeout.String() + " waiting for container-gate slot")
		},
		nil)

	mq := &config.MergeQueueConfig{
		TestCommand:           dockerTestsEnv + "=1 go test ./...",
		TestVerifySlotTimeout: slotCap.String(),
	}
	g := git.NewGit(dir)
	_, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/bounded-wait-role")

	if err == nil {
		t.Fatal("expected a slot-contention error once the cap expired")
	}
	if gotTimeout != slotCap {
		t.Errorf("acquire got a %s cap, want the rig's configured %s", gotTimeout, slotCap)
	}
	if acquires != 1 {
		t.Errorf("the gate acquired the slot %d times, want exactly 1 — the wait is not re-armed", acquires)
	}
	// Bounded: the acquire waits its cap out and gives up inside it. The
	// window is timed from inside the stub, because everything the gate does
	// before the acquire — the diff, the package list — is subprocess work
	// whose latency would otherwise land in the bound and make it flaky
	// (gt-7dxw review). acquires == 1 is what proves the wait is not re-armed.
	if acquireWindow < slotCap {
		t.Errorf("gave up after %s, before the %s cap expired", acquireWindow, slotCap)
	}
	if acquireWindow > slotCap+10*time.Second {
		t.Errorf("held the acquire for %s under a %s cap", acquireWindow, slotCap)
	}
	logPath := testVerifyLogPath(dir)
	msg := err.Error()
	for _, want := range []string{"slot contention", "NOT a test failure", slotCap.String(), "merge_queue.test_verify_slot_timeout", "gt escalate -s medium", logPath} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message is missing %q: %v", want, err)
		}
	}
	if strings.Contains(msg, "Re-run gt done") {
		t.Errorf("failure message still invites a re-run of the command that just failed: %v", err)
	}
}

// TestRunDefaultTestVerification_SlotWaitPrintsProgressBeforeGivingUp pins the
// progress half of the same path: while the wait is inside its cap the verify
// log — the artifact a bystander reads after the fact — already carries a
// waiting line, so a polecat's quiet pane is diagnosable without a restart
// (gt-pnkd's frozen 144-byte log).
func TestRunDefaultTestVerification_SlotWaitPrintsProgressBeforeGivingUp(t *testing.T) {
	const slotCap = 60 * time.Millisecond
	dir, _ := initVerifyTestGoRepo(t)
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

	stubVerifyProgress(t, slotCap/6)
	stubVerifyGate(t,
		func(_ string, _ string, timeout time.Duration) (func(), error) {
			time.Sleep(timeout)
			return nil, errors.New("timed out after " + timeout.String() + " waiting for container-gate slot")
		},
		nil)

	mq := &config.MergeQueueConfig{
		TestCommand:           dockerTestsEnv + "=1 go test ./...",
		TestVerifySlotTimeout: slotCap.String(),
	}
	g := git.NewGit(dir)
	_, err := runDefaultTestVerification(g, dir, "main", "main", mq, t.TempDir(), "test/bounded-log-role")
	if err == nil {
		t.Fatal("expected a slot-contention error once the cap expired")
	}
	logPath := testVerifyLogPath(dir)
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("reading %s: %v", logPath, readErr)
	}
	logText := string(data)
	if !strings.Contains(logText, "still waiting for the container-gate slot") {
		t.Errorf("verify log has no slot-wait progress line, so a quiet pane is undiagnosable:\n%s", logText)
	}
	if !strings.Contains(logText, "slot cap:") {
		t.Errorf("verify log does not record the cap it was waiting under:\n%s", logText)
	}
}
