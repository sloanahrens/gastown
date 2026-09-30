package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func TestRoutedIssueBeadsUsesTownRoutesForCustomPrefix(t *testing.T) {
	t.Parallel()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)

	_, gotCurrent, gotRouted := routedIssueBeads(workDir, "bd-source")
	if gotCurrent != currentBeadsDir {
		t.Fatalf("current beads dir = %q, want %q", gotCurrent, currentBeadsDir)
	}
	if gotRouted != ownerBeadsDir {
		t.Fatalf("routed beads dir = %q, want %q", gotRouted, ownerBeadsDir)
	}
}

func TestSourceRouteContextNamesCurrentAndRoutedDB(t *testing.T) {
	t.Parallel()
	context := sourceRouteContext("/town/gastown/.beads", "/town/beads/.beads")
	for _, want := range []string{"current_db=/town/gastown/.beads", "routed_db=/town/beads/.beads"} {
		if !strings.Contains(context, want) {
			t.Fatalf("source route context %q missing %q", context, want)
		}
	}
}

func TestResolveSubmitSourceIssueIgnoresCurrentRigMirror(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	installSubmitSourceBDStub(t, currentBeadsDir, ownerBeadsDir, false)

	source, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}
	if source.Issue.Title != "owner source" {
		t.Fatalf("source title = %q, want routed owner source (current-rig mirror must be ignored)", source.Issue.Title)
	}
	if source.CurrentBeadsDir != currentBeadsDir || source.RoutedBeadsDir != ownerBeadsDir {
		t.Fatalf("route = current %q routed %q, want current %q routed %q", source.CurrentBeadsDir, source.RoutedBeadsDir, currentBeadsDir, ownerBeadsDir)
	}
}

func TestResolveSubmitSourceIssueFailureNamesRoutingContext(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	installSubmitSourceBDStub(t, currentBeadsDir, ownerBeadsDir, true)

	_, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err == nil {
		t.Fatal("resolveSubmitSourceIssue succeeded, want routed owner lookup failure")
	}
	errText := err.Error()
	for _, want := range []string{"source_issue bd-source could not be resolved", "current_db=" + currentBeadsDir, "routed_db=" + ownerBeadsDir} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error %q missing %q", errText, want)
		}
	}
}

func TestDoneNoMRClosePathUsesRoutedSourceBeads(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)

	source, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}
	if skipReason, fatal := doneSourceCloseSkipReason(source.BD, "bd-source", source.Issue); skipReason != "" || fatal {
		t.Fatalf("doneSourceCloseSkipReason = %q, %v; want close allowed", skipReason, fatal)
	}
	if err := source.BD.ForceCloseWithReason("done", "bd-source"); err != nil {
		t.Fatalf("routed source close: %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	assertBDLogContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogContains(t, log, ownerBeadsDir, "close bd-source")
	assertBDLogNotContains(t, log, currentBeadsDir, "close bd-source")
}

func TestRunDoneWithRoutedIssueIgnoresCurrentRigMirror(t *testing.T) {
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
func TestRunDoneReworkBranchRecordsActualWorkerNotBranchName(t *testing.T) {
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

func setupRoutedSourceTestTown(t *testing.T) (workDir, currentBeadsDir, ownerBeadsDir string) {
	t.Helper()
	townRoot := t.TempDir()
	// Resolve symlinks now so every path derived below matches what
	// resolveDonePolecatWorktreeAt produces: it canonicalizes cwd via
	// filepath.EvalSymlinks before deriving BEADS_DIR routing. On macOS,
	// t.TempDir() lives under /var/folders/..., a symlink to
	// /private/var/folders/...; without this, runDone's canonicalized cwd
	// diverges from the non-canonical paths the bd stub below expects,
	// and every routed bd lookup falsely reports "issue not found".
	if resolved, err := filepath.EvalSymlinks(townRoot); err == nil {
		townRoot = resolved
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write town sentinel: %v", err)
	}

	workDir = filepath.Join(townRoot, "gastown", "polecats", "refuge", "gastown")
	currentBeadsDir = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	ownerBeadsDir = filepath.Join(townRoot, "beads", "mayor", "rig", ".beads")
	townBeadsDir := filepath.Join(townRoot, ".beads")
	for _, dir := range []string{filepath.Join(workDir, ".beads"), currentBeadsDir, ownerBeadsDir, townBeadsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, ".beads", "redirect"), []byte("../../../mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	return workDir, currentBeadsDir, ownerBeadsDir
}

func routedSourceTestTownRoot(workDir string) string {
	return filepath.Clean(filepath.Join(workDir, "..", "..", "..", ".."))
}

func setupRoutedSubmitCommandTown(t *testing.T, workDir string) {
	t.Helper()
	townRoot := routedSourceTestTownRoot(workDir)
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if err := config.SaveRigsConfig(rigsPath, &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "file://test-gastown"},
		},
	}); err != nil {
		t.Fatalf("save rigs config: %v", err)
	}
}

func setupRoutedSubmitGitRepo(t *testing.T, workDir string, pushBranch bool) string {
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
	branch := "feature/routed-submit"
	runGitForMQSubmitTest(t, workDir, "checkout", "-b", branch)
	writeMQSubmitTestFile(t, workDir, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, workDir, "commit", "-am", "feature")
	if pushBranch {
		runGitForMQSubmitTest(t, workDir, "push", "origin", branch)
	}
	return branch
}

func installSubmitSourceBDStub(t *testing.T, currentBeadsDir, ownerBeadsDir string, ownerMissing bool) {
	t.Helper()
	binDir := t.TempDir()
	ownerCase := fmt.Sprintf(`
if [ "$BEADS_DIR" = %q ]; then
  echo '[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task"}]'
  exit 0
fi`, ownerBeadsDir)
	if ownerMissing {
		ownerCase = fmt.Sprintf(`
if [ "$BEADS_DIR" = %q ]; then
  echo "Issue not found in owner" >&2
  exit 1
fi`, ownerBeadsDir)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  shift
fi
if [ "$1" = "version" ]; then
  echo "bd stub"
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "bd-source" ]; then
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task"}]'
    exit 0
  fi
%s
  echo "Issue not found in $BEADS_DIR" >&2
  exit 1
fi
echo "unexpected bd command: $*" >&2
exit 1
`, currentBeadsDir, ownerCase)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
}

func installSubmitSourceBDRecorder(t *testing.T, currentBeadsDir, ownerBeadsDir string) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  shift
fi
if [ "$1" = "version" ]; then
  echo "bd stub"
  exit 0
fi
printf '%%s\t%%s\n' "$BEADS_DIR" "$*" >> %q
if [ "$1" = "update" ]; then
  if [ -n "$GT_TEST_BD_UPDATE_FAILS" ]; then
    echo "Error: database not found: gastown" >&2
    exit 1
  fi
  case "$*" in *--add-label=gt:ready-to-land*) : > %q.ready ;; esac
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "gt-gastown-polecat-refuge" ]; then
  echo '[{"id":"gt-gastown-polecat-refuge","title":"Polecat refuge","status":"open","issue_type":"agent","labels":["gt:agent","done-intent:COMPLETED:1738972800"]}]'
  exit 0
fi
labels='[]'
if [ -e %q.ready ]; then labels='["gt:ready-to-land"]'; fi
if [ "$1" = "show" ] && [ "$2" = "bd-source" ]; then
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task","labels":'"$labels"',"description":"convoy_id: hq-cv-test\\nmerge_strategy: mr"}]'
    exit 0
  fi
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task","labels":'"$labels"',"description":"convoy_id: hq-cv-test\\nmerge_strategy: mr"}]'
    exit 0
  fi
  echo "Issue not found in $BEADS_DIR" >&2
  exit 1
fi
if [ "$1" = "show" ] && [ "$2" = "gt-mr" ]; then
  echo '[{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"],"description":"branch: feature/routed-submit\\ntarget: main\\nsource_issue: bd-source\\nrig: gastown"}]'
  exit 0
fi
if [ "$1" = "list" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "sql" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "create" ]; then
  if [ -n "$GT_TEST_BD_CREATE_FAILS" ]; then
    echo "Error: database not found: gastown" >&2
    exit 1
  fi
  echo '{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"]}'
  exit 0
fi
if [ "$1" = "comments" ] && [ "$2" = "add" ]; then
  exit 0
fi
if [ "$1" = "close" ]; then
  if [ -n "$GT_TEST_BD_CLOSE_FAILS" ]; then
    echo "Error: database not found: gastown" >&2
    exit 1
  fi
  exit 0
fi
echo "unexpected bd command: $*" >&2
exit 1
`, logPath, logPath, logPath, currentBeadsDir, ownerBeadsDir)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd recorder: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
	return logPath
}

func readSubmitSourceBDLog(t *testing.T, logPath string) string {
	t.Helper()
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd recorder log: %v", err)
	}
	return string(log)
}

func assertBDLogContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if !strings.Contains(log, needle) {
		t.Fatalf("bd log missing %q:\n%s", needle, log)
	}
}

func assertBDLogNotContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if strings.Contains(log, needle) {
		t.Fatalf("bd log unexpectedly contains %q:\n%s", needle, log)
	}
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

// TestRunDoneExitsPushFailedWhenOriginRejects: origin refuses the branch on
// every attempt, so the work is only local: exit 10.
func TestRunDoneExitsPushFailedWhenOriginRejects(t *testing.T) {
	err := runDoneForExitCode(t, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		installRemoteHook(t, workDir, "pre-receive", "echo 'rejected by test hook' >&2\nexit 1\n")
	})
	assertDoneExitCode(t, err, doneExitPushFailed, "feature/routed-submit")
}

// TestRunDoneExitsPushUnverifiedWhenOriginDropsTheBranch: every push command
// succeeds but origin never holds the commit afterwards: exit 11.
func TestRunDoneExitsPushUnverifiedWhenOriginDropsTheBranch(t *testing.T) {
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
func TestRunDoneExitsCloseFailedOnNoMRClose(t *testing.T) {
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
func TestRunDoneClassifiesOnTheLastPushAttempt(t *testing.T) {
	err := runDoneForExitCode(t, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		marker := filepath.Join(t.TempDir(), "rejected-once")
		installRemoteHook(t, workDir, "pre-receive", "if [ ! -e '"+marker+"' ]; then touch '"+marker+"'; echo 'transient rejection' >&2; exit 1; fi\nexit 0\n")
		installRemoteHook(t, workDir, "post-receive", "git update-ref -d refs/heads/feature/routed-submit\nexit 0\n")
	})
	assertDoneExitCode(t, err, doneExitPushUnverified, "feature/routed-submit")
}
