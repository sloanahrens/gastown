//go:build integration

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end gt done runs in a routed test town: real git with a bare
// origin and hooks, a bd recorder stub on PATH. The decisions they cover are
// unit-tested in done_submit_unit_test.go; these pin the wiring.

func TestIntegrationRunDoneWithRoutedIssueIgnoresCurrentRigMirror(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	setupRoutedSubmitGitRepo(t, workDir, false)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	townRoot := routedSourceTestTownRoot(workDir)
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Chdir(workDir)

	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	updateAgentStateOnDoneFn = func(cwd, townRoot, exitType, issueID string) error { return nil }
	if err := runDone(nil, nil); err != nil {
		t.Fatalf("runDone: %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	assertBDLogContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogContains(t, log, ownerBeadsDir, "update bd-source --append-notes READY TO LAND")
	assertBDLogContains(t, log, ownerBeadsDir, "update bd-source --add-label=gt:ready-to-land")
	if strings.Contains(log, "gt:merge-request") {
		t.Fatalf("gt done created an MR wisp:\n%s", log)
	}
	assertBDLogNotContains(t, log, currentBeadsDir, "show bd-source --json")
	assertBDLogNotContains(t, log, currentBeadsDir, "update bd-source")
}

// TestRunDoneReworkBranchRecordsActualWorkerNotBranchName covers gt-fl0n: a
// --branch rework reuses the ORIGINAL polecat's branch name (here
// "malachite"), but the polecat actually running `gt done` is a different
// one ("refuge", set via env). The READY TO LAND note's Worker must record the
// actual submitter so a rejection reaches whoever holds the issue now, never
// the name embedded in the reused branch.
func TestIntegrationRunDoneReworkBranchRecordsActualWorkerNotBranchName(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	setupReworkBranchGitRepo(t, workDir, "polecat/malachite/bd-source+mudreworkab")
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	townRoot := routedSourceTestTownRoot(workDir)
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Chdir(workDir)

	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	updateAgentStateOnDoneFn = func(cwd, townRoot, exitType, issueID string) error { return nil }
	if err := runDone(nil, nil); err != nil {
		t.Fatalf("runDone: %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	if !strings.Contains(log, "Worker: refuge") {
		t.Fatalf("ready note missing Worker: refuge (actual submitter):\n%s", log)
	}
	if strings.Contains(log, "Worker: malachite") {
		t.Fatalf("ready note recorded Worker: malachite from the reused branch name instead of the actual submitter:\n%s", log)
	}
}

// runDoneForExitCode runs gt done as a polecat in the routed test town with
// branchSetup shaping the repo, and returns runDone's error.
func runDoneForExitCode(t *testing.T, branchSetup func(t *testing.T, workDir string)) error {
	t.Helper()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	branchSetup(t, workDir)
	installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	oldDelays := pushLandingRetryDelays
	pushLandingRetryDelays = []time.Duration{0}
	t.Cleanup(func() { pushLandingRetryDelays = oldDelays })
	townRoot := routedSourceTestTownRoot(workDir)
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Chdir(workDir)

	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	updateAgentStateOnDoneFn = func(cwd, townRoot, exitType, issueID string) error { return nil }
	return runDone(nil, nil)
}

// installRemoteHook writes a git hook into the test repo's bare origin.
func installRemoteHook(t *testing.T, workDir, hook, body string) {
	t.Helper()
	out, err := exec.Command("git", "-C", workDir, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("origin url: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "hooks", hook)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write %s hook: %v", hook, err)
	}
}

// TestRunDoneExitsPushFailedWhenOriginRejects: origin refuses the branch on
// every attempt, so the work is only local: exit 10.
func TestIntegrationRunDoneExitsPushFailedWhenOriginRejects(t *testing.T) {
	err := runDoneForExitCode(t, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		installRemoteHook(t, workDir, "pre-receive", "echo 'rejected by test hook' >&2\nexit 1\n")
	})
	assertDoneExitCode(t, err, doneExitPushFailed, "feature/routed-submit")
}

// TestRunDoneExitsPushUnverifiedWhenOriginDropsTheBranch: every push command
// succeeds but origin never holds the commit afterwards: exit 11.
func TestIntegrationRunDoneExitsPushUnverifiedWhenOriginDropsTheBranch(t *testing.T) {
	err := runDoneForExitCode(t, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		installRemoteHook(t, workDir, "post-receive", "git update-ref -d refs/heads/feature/routed-submit\nexit 0\n")
	})
	assertDoneExitCode(t, err, doneExitPushUnverified, "feature/routed-submit")
}

// TestRunDoneExitsCloseFailedOnNoMRClose: a branch with nothing ahead of main
// closes its source bead without an MR; bd cannot close it: exit 13. Push
// verification of the no-MR close runs (it is no longer skippable) and
// passes, because HEAD is on origin/main.
func TestIntegrationRunDoneExitsCloseFailedOnNoMRClose(t *testing.T) {
	t.Setenv("GT_TEST_BD_CLOSE_FAILS", "1")
	err := runDoneForExitCode(t, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		runGitForMQSubmitTest(t, workDir, "reset", "--hard", "main")
	})
	assertDoneExitCode(t, err, doneExitCloseFailed, "could not close issue bd-source")
}

// TestRunDoneClassifiesOnTheLastPushAttempt: the first push fails, the retry
// push succeeds, and origin still does not hold the commit. The push did not
// fail; origin is unverified: exit 11, not 10 (om review).
func TestIntegrationRunDoneClassifiesOnTheLastPushAttempt(t *testing.T) {
	err := runDoneForExitCode(t, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		marker := filepath.Join(t.TempDir(), "rejected-once")
		installRemoteHook(t, workDir, "pre-receive", "if [ ! -e '"+marker+"' ]; then touch '"+marker+"'; echo 'transient rejection' >&2; exit 1; fi\nexit 0\n")
		installRemoteHook(t, workDir, "post-receive", "git update-ref -d refs/heads/feature/routed-submit\nexit 0\n")
	})
	assertDoneExitCode(t, err, doneExitPushUnverified, "feature/routed-submit")
}

func resetDoneFlagsForTest(t *testing.T) {
	t.Helper()
	oldIssue, oldStatus, oldCleanupStatus, oldTarget := doneIssue, doneStatus, doneCleanupStatus, doneTarget
	oldUpdateAgentStateOnDoneFn := updateAgentStateOnDoneFn
	doneIssue = ""
	doneStatus = ExitCompleted
	doneCleanupStatus = ""
	doneTarget = ""
	t.Cleanup(func() {
		doneIssue, doneStatus, doneCleanupStatus, doneTarget = oldIssue, oldStatus, oldCleanupStatus, oldTarget
		updateAgentStateOnDoneFn = oldUpdateAgentStateOnDoneFn
	})
	// No test runs the rig's real gate; a test that cares replaces this.
	useDoneGate(t, passingDoneGate())
}

// setupReworkBranchGitRepo mirrors setupRoutedSubmitGitRepo but takes an
// explicit branch name so a test can simulate `gt sling --branch` reusing a
// different polecat's surviving branch.
func setupReworkBranchGitRepo(t *testing.T, workDir, branch string) string {
	t.Helper()
	remote := t.TempDir()
	runGitForMQSubmitTest(t, remote, "init", "--bare")
	runGitForMQSubmitTest(t, workDir, "init")
	runGitForMQSubmitTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, workDir, "config", "user.name", "Test User")
	runGitForMQSubmitTest(t, workDir, "remote", "add", "origin", remote)
	writeMQSubmitTestFile(t, workDir, ".gitignore", ".beads/\n.runtime/\n")
	writeMQSubmitTestFile(t, workDir, "file.txt", "main\n")
	runGitForMQSubmitTest(t, workDir, "add", ".gitignore", "file.txt")
	runGitForMQSubmitTest(t, workDir, "commit", "-m", "main")
	runGitForMQSubmitTest(t, workDir, "branch", "-M", "main")
	runGitForMQSubmitTest(t, workDir, "push", "-u", "origin", "main")
	runGitForMQSubmitTest(t, workDir, "checkout", "-b", branch)
	writeMQSubmitTestFile(t, workDir, "file.txt", "rework\n")
	runGitForMQSubmitTest(t, workDir, "commit", "-am", "rework")
	return branch
}

func assertDoneExitCode(t *testing.T, err error, want int, wantText string) {
	t.Helper()
	var coded *ExitCodeError
	if !errors.As(err, &coded) || coded.Code != want {
		t.Fatalf("runDone error = %T %v, want *ExitCodeError with code %d", err, err, want)
	}
	if !strings.Contains(err.Error(), wantText) {
		t.Errorf("error %q lacks %q", err, wantText)
	}
}
