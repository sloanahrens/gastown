package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// TestRunDefaultTestVerification_Lint covers the rig lint_command inside gt
// done's default gate: it runs slot-free before the tests, a failure refuses
// the submission without spending a suite run, and it still runs when the
// changed packages include container-backed ones (gt-btw1: no deferral).
func TestRunDefaultTestVerification_Lint(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	t.Run("lint passes, then tests run; recorded on the result", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		marker := filepath.Join(dir, "lint-ran")
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: "echo ok > '" + marker + "'"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.lintRan || result.lintCommand == "" {
			t.Errorf("lint not recorded: %+v", result)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("lint command did not run: %v", statErr)
		}
		if !result.ran || !result.success || len(result.packages) != 1 {
			t.Errorf("tests did not run after lint: %+v", result)
		}
		if acquired {
			t.Error("lint/test of a container-free package took a slot")
		}
		log, _ := os.ReadFile(result.logPath)
		if !strings.Contains(string(log), "lint passed in") {
			t.Errorf("verify log does not record the lint pass:\n%s", log)
		}
	})

	t.Run("lint fails: refuse with the findings, tests never run", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		testMarker := filepath.Join(dir, "tests-ran")
		lint := "echo 'pkga/a.go:1:1: something is wrong (fakelint)'; exit 3"
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: lint, TestVerifyCommand: "echo ran > '" + testMarker + "'"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-fail-role")
		if err == nil {
			t.Fatalf("expected a lint refusal, got result=%+v", result)
		}
		for _, want := range []string{"lint-verify failed", "exit 3", "fakelint", "no tests were run"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if _, statErr := os.Stat(testMarker); statErr == nil {
			t.Error("tests ran despite the lint failure")
		}
	})

	t.Run("container-backed package changed: lint still runs, then the full suite runs slot-free", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "only internal/cmd")

		// nil runner: the lint and the suite must actually run here, since this
		// subtest is about what the gate does around them (the slot stub alone
		// keeps a slot from being taken).
		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		marker := filepath.Join(dir, "lint-ran")
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: "echo ok > '" + marker + "'"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-container-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.lintRan {
			t.Errorf("lint did not run: %+v", result)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("lint command did not run: %v", statErr)
		}
		if !result.ran || !result.success {
			t.Errorf("tests did not run after lint (no deferral under gt-btw1): %+v", result)
		}
		// gt-wx53: a container-backed package in the diff is not by itself a
		// reason to queue for the town-wide slot — the rig's command has to ask
		// for containers, and this one does not.
		if result.slotUsed || acquired {
			t.Errorf("slotUsed=%v acquired=%v, want false/false", result.slotUsed, acquired)
		}
		if !result.containersOptedOut {
			t.Error("containersOptedOut = false, want true")
		}
	})

	t.Run("no lint_command configured: gate unchanged", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/no-lint-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.lintRan || result.lintCommand != "" {
			t.Errorf("lint recorded although none was configured: %+v", result)
		}
		if !result.ran || !result.success {
			t.Errorf("tests did not run: %+v", result)
		}
	})
}

// TestRunDefaultTestVerification_LintLockContention covers gt-xsty: two lints
// running at once make golangci-lint's loser exit with "parallel golangci-lint
// is running" without analysing anything. The gate must wait the holder out
// and retry (attributing the wait) instead of reporting the collision as a
// lint finding, and must still report a real finding as a finding.
func TestRunDefaultTestVerification_LintLockContention(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	// collidingLint appends one line per attempt and prints golangci-lint's
	// contention message on the first attempt only, so the second attempt
	// looks like an uncontended lint that found nothing.
	collidingLint := func(dir string) (lint, counter string) {
		counter = filepath.Join(dir, "lint-attempts")
		lint = fmt.Sprintf(`n=$(grep -c . %q 2>/dev/null || echo 0); echo x >> %q; `+
			`if [ "$n" -lt 1 ]; then echo 'Error: parallel golangci-lint is running' >&2; exit 2; fi`,
			counter, counter)
		return lint, counter
	}
	attemptsMade := func(t *testing.T, counter string) int {
		t.Helper()
		got, err := os.ReadFile(counter)
		if err != nil {
			t.Fatalf("reading attempt counter %s: %v", counter, err)
		}
		n := 0
		for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		return n
	}
	commitPkga := func(t *testing.T, dir string) {
		t.Helper()
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")
	}

	t.Run("lock held once: waited out, retried, then lint passes and tests run", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		commitPkga(t, dir)
		stubLintLockRetryDelay(t, time.Millisecond, time.Millisecond)

		lint, counter := collidingLint(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: lint}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-lock-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification after a released lock: %v", err)
		}
		if !result.lintRan {
			t.Errorf("lint not recorded after a successful retry: %+v", result)
		}
		if !result.ran || !result.success {
			t.Errorf("tests did not run after the retry: %+v", result)
		}
		if got := attemptsMade(t, counter); got != 2 {
			t.Errorf("lint ran %d times, want 2 (one collision, one retry)", got)
		}
		log, _ := os.ReadFile(result.logPath)
		for _, want := range []string{"lint lock held by another golangci-lint", "attempt 1/3", "lint passed in"} {
			if !strings.Contains(string(log), want) {
				t.Errorf("verify log is missing %q:\n%s", want, log)
			}
		}
	})

	t.Run("lock never released: bounded retries, reported as contention not a finding", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		commitPkga(t, dir)
		stubLintLockRetryDelay(t, time.Millisecond, time.Millisecond)

		testMarker := filepath.Join(dir, "tests-ran")
		counter := filepath.Join(dir, "lint-attempts")
		lint := fmt.Sprintf(`echo x >> %q; echo 'Error: parallel golangci-lint is running' >&2; exit 2`, counter)
		mq := &config.MergeQueueConfig{
			TestCommand:       "go test ./...",
			LintCommand:       lint,
			TestVerifyCommand: "echo ran > '" + testMarker + "'",
		}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-lock-stuck-role")
		if err == nil {
			t.Fatalf("expected a refusal while the lock stays held, got result=%+v", result)
		}
		for _, want := range []string{
			"lint-verify failed",
			"exit 2",
			"another golangci-lint held the lock across 2 retries",
			"no tests were run",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "fix the lint findings") {
			t.Errorf("lock contention was reported as a lint finding: %v", err)
		}
		if got := attemptsMade(t, counter); got != 3 {
			t.Errorf("lint attempts = %d, want 3 (initial + 2 retries)", got)
		}
		if _, statErr := os.Stat(testMarker); statErr == nil {
			t.Error("tests ran despite the lint lock never being released")
		}
	})

	t.Run("a real finding is not mistaken for lock contention: no retry", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		commitPkga(t, dir)
		stubLintLockRetryDelay(t, time.Millisecond, time.Millisecond)

		counter := filepath.Join(dir, "lint-attempts")
		lint := fmt.Sprintf(`echo x >> %q; echo 'pkga/a.go:1:1: something is wrong (fakelint)'; exit 3`, counter)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: lint}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-real-finding-role")
		if err == nil {
			t.Fatalf("expected a lint refusal, got result=%+v", result)
		}
		for _, want := range []string{"lint-verify failed", "exit 3", "fakelint", "fix the lint findings", "no tests were run"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "held the lock") {
			t.Errorf("a real finding was attributed to lock contention: %v", err)
		}
		if got := attemptsMade(t, counter); got != 1 {
			t.Errorf("lint attempts = %d, want 1 (a finding is never retried)", got)
		}
	})
}

// The retry policy's own unit tests (detection wording, budget bounds) live
// with the policy in internal/lintlock; what stays here is gt done's use of it.
