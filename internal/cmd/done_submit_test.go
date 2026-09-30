package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
)

// recordingGate stands in for the local pre-submit gate: it records the tree
// and the commit it was asked to gate and answers with result.
type recordingGate struct {
	result land.GateResult
	dirs   []string
	heads  []string
}

func (g *recordingGate) Run(_ context.Context, dir string) land.GateResult {
	g.dirs = append(g.dirs, dir)
	out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	g.heads = append(g.heads, strings.TrimSpace(string(out)))
	return g.result
}

func useDoneGate(t *testing.T, g land.Gate) {
	t.Helper()
	old := doneLocalGate
	doneLocalGate = func(townRoot, rigName, dir string) (land.Gate, error) { return g, nil }
	t.Cleanup(func() { doneLocalGate = old })
}

func passingDoneGate() *recordingGate {
	return &recordingGate{result: land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "test"}}}}
}

// doneSubmitRun is one gt done run in the routed test town with its bd log,
// nudge log and the repo it ran in.
type doneSubmitRun struct {
	workDir  string
	bdLog    string
	nudgeLog string
	err      error
}

func runDoneSubmit(t *testing.T, gate land.Gate, branchSetup func(t *testing.T, workDir string)) doneSubmitRun {
	t.Helper()
	return runDoneSubmitWithFlags(t, gate, nil, branchSetup)
}

// runDoneSubmitWithFlags is runDoneSubmit with setFlags run after the done
// flags are reset, for a test that needs a flag such as --target set.
func runDoneSubmitWithFlags(t *testing.T, gate land.Gate, setFlags func(), branchSetup func(t *testing.T, workDir string)) doneSubmitRun {
	t.Helper()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	branchSetup(t, workDir)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	useDoneGate(t, gate)
	townRoot := routedSourceTestTownRoot(workDir)
	nudgeLog := filepath.Join(t.TempDir(), "nudge.log")
	t.Setenv("GT_TEST_NUDGE_LOG", nudgeLog)
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Chdir(workDir)
	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	if setFlags != nil {
		setFlags()
	}
	updateAgentStateOnDoneFn = func(cwd, townRoot, exitType, issueID string) error { return nil }
	err := runDone(nil, nil)
	bdLog, _ := os.ReadFile(logPath)
	return doneSubmitRun{workDir: workDir, bdLog: string(bdLog), nudgeLog: nudgeLog, err: err}
}

func (r doneSubmitRun) reportedDone() bool {
	data, _ := os.ReadFile(r.nudgeLog)
	return strings.Contains(string(data), "POLECAT_DONE")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func originURL(t *testing.T, workDir string) string {
	return gitOut(t, workDir, "remote", "get-url", "origin")
}

// advanceOriginMain lands a commit on origin/main from a separate clone, so
// the polecat's branch is behind its target.
func advanceOriginMain(t *testing.T, workDir, name, body string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitOut(t, filepath.Dir(other), "clone", "-q", originURL(t, workDir), other)
	gitOut(t, other, "config", "user.email", "o@example.com")
	gitOut(t, other, "config", "user.name", "Other")
	writeMQSubmitTestFile(t, other, name, body)
	gitOut(t, other, "add", name)
	gitOut(t, other, "commit", "-q", "-m", "other: "+name)
	gitOut(t, other, "push", "-q", "origin", "main")
}

const doneTestBranch = "feature/routed-submit"

// TestRunDoneRebasesGatesPushesAndMarksReady is the whole author side: the
// branch is rebased onto the moved target, its auto-save commit squashed, the
// rebased tree gated, the exact gated commit pushed, and the work bead marked
// ready to land. No MR bead is created and nothing reaches main.
func TestRunDoneRebasesGatesPushesAndMarksReady(t *testing.T) {
	gate := passingDoneGate()
	r := runDoneSubmit(t, gate, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		writeMQSubmitTestFile(t, workDir, "more.txt", "more\n")
		runGitForMQSubmitTest(t, workDir, "add", "more.txt")
		runGitForMQSubmitTest(t, workDir, "commit", "-m", "WIP: checkpoint (auto)")
		advanceOriginMain(t, workDir, "other.txt", "other\n")
	})
	if r.err != nil {
		t.Fatalf("runDone: %v", r.err)
	}
	head := gitOut(t, r.workDir, "rev-parse", "HEAD")
	originMain := gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/main")
	if !strings.HasPrefix(originMain, gitOut(t, r.workDir, "rev-parse", "origin/main")) {
		t.Fatalf("origin/main moved: %s", originMain)
	}
	if got := strings.Fields(gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/"+doneTestBranch)); len(got) == 0 || got[0] != head {
		t.Fatalf("origin/%s = %v, want pushed HEAD %s", doneTestBranch, got, head)
	}
	if _, err := exec.Command("git", "-C", r.workDir, "merge-base", "--is-ancestor", "origin/main", "HEAD").CombinedOutput(); err != nil {
		t.Error("branch was not rebased onto origin/main")
	}
	if subjects := gitOut(t, r.workDir, "log", "--format=%s", "origin/main..HEAD"); strings.Contains(subjects, "checkpoint (auto)") {
		t.Errorf("auto-save commit reached the pushed branch:\n%s", subjects)
	}
	if len(gate.heads) != 1 || gate.heads[0] != head {
		t.Errorf("gate ran on %v, want exactly the pushed head %s", gate.heads, head)
	}
	for _, want := range []string{"update bd-source --append-notes READY TO LAND", "Head: " + head, "Worker: refuge", "--add-label=gt:ready-to-land"} {
		if !strings.Contains(r.bdLog, want) {
			t.Errorf("bd log lacks %q:\n%s", want, r.bdLog)
		}
	}
	if strings.Contains(r.bdLog, "gt:merge-request") {
		t.Errorf("gt done created an MR wisp:\n%s", r.bdLog)
	}
	if !r.reportedDone() {
		t.Error("a landed submission did not report POLECAT_DONE")
	}
	// gt-obbx2: the seat has no session from here to the landing, and its hook
	// still holds the bead. The intent record is what tells the crash detectors
	// (and the supervisor's Restart) that this is a finished polecat.
	if rec := submittedIntentFor(t, r); !rec.Submitted() || rec.WorkBead != "bd-source" {
		t.Errorf("intent record = %+v, want desired=submitted for bd-source", rec)
	}
}

// submittedIntentFor reads the intent record of the polecat runDoneSubmit ran as.
func submittedIntentFor(t *testing.T, r doneSubmitRun) intent.Record {
	t.Helper()
	seat := intent.Seat{Rig: "gastown", Role: "polecat", Name: "refuge"}
	rec, err := intent.Read(routedSourceTestTownRoot(r.workDir), seat)
	if err != nil {
		t.Fatalf("reading the intent record: %v", err)
	}
	return rec
}

// TestRunDoneReplacesAnOlderBranchTipUnderLease: the branch was pushed by an
// earlier attempt, then rebased here; the rebased tip replaces it under a
// lease on the tip origin had.
func TestRunDoneReplacesAnOlderBranchTipUnderLease(t *testing.T) {
	r := runDoneSubmit(t, passingDoneGate(), func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, true)
		advanceOriginMain(t, workDir, "other.txt", "other\n")
	})
	if r.err != nil {
		t.Fatalf("runDone: %v", r.err)
	}
	head := gitOut(t, r.workDir, "rev-parse", "HEAD")
	if got := strings.Fields(gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/"+doneTestBranch)); len(got) == 0 || got[0] != head {
		t.Fatalf("origin branch = %v, want %s", got, head)
	}
}

// TestRunDoneRedLocalGateExits15: a red local gate stops before the push:
// exit 15, nothing on origin, no ready mark, no done report.
func TestRunDoneRedLocalGateExits15(t *testing.T) {
	gate := &recordingGate{result: land.GateResult{Steps: []land.StepResult{{Name: "test", ExitCode: 2, Tail: "FAIL\tpkg/x\n"}}}}
	r := runDoneSubmit(t, gate, func(t *testing.T, workDir string) { setupRoutedSubmitGitRepo(t, workDir, false) })
	assertDoneExitCode(t, r.err, doneExitGateFailed, "FAIL\tpkg/x")
	if got := gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/"+doneTestBranch); got != "" {
		t.Errorf("a red gate pushed the branch: %s", got)
	}
	if strings.Contains(r.bdLog, "gt:ready-to-land") || r.reportedDone() {
		t.Errorf("a red gate marked ready or reported done:\n%s", r.bdLog)
	}
}

// TestRunDoneRebaseConflictExits14: the target changed the same lines: exit
// 14 naming the file, and the worktree is not left mid-rebase.
func TestRunDoneRebaseConflictExits14(t *testing.T) {
	r := runDoneSubmit(t, passingDoneGate(), func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
		advanceOriginMain(t, workDir, "file.txt", "main moved\n")
	})
	assertDoneExitCode(t, r.err, doneExitRebaseConflict, "file.txt")
	if _, err := os.Stat(filepath.Join(r.workDir, ".git", "rebase-merge")); err == nil {
		t.Error("worktree left mid-rebase")
	}
	if r.reportedDone() {
		t.Error("a rebase conflict reported done")
	}
}

// TestRunDoneExitsReadyRecordFailed: the branch is on origin but bd could not
// mark the work bead ready: exit 12 and no done report.
func TestRunDoneExitsReadyRecordFailed(t *testing.T) {
	t.Setenv("GT_TEST_BD_UPDATE_FAILS", "1")
	r := runDoneSubmit(t, passingDoneGate(), func(t *testing.T, workDir string) { setupRoutedSubmitGitRepo(t, workDir, false) })
	assertDoneExitCode(t, r.err, doneExitReadyFailed, "ready to land")
	if r.reportedDone() {
		t.Error("an unmarked submission reported done")
	}
	if rec := submittedIntentFor(t, r); rec.Submitted() {
		t.Errorf("a bead that never got gt:ready-to-land was recorded as submitted: %+v", rec)
	}
}

// TestDoneLandingFlagsAreGone: gt done has no landing modes and no gate
// bypass (ADR 0004).
func TestDoneLandingFlagsAreGone(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pre-verified", "skip-tests", "skip-verify", "merge", "resume", "priority"} {
		if doneCmd.Flags().Lookup(name) != nil {
			t.Errorf("gt done still has --%s", name)
		}
	}
}

// TestRunDoneRefusesToPushOverSomeoneElsesWork: origin's branch holds a
// commit this worktree does not have (another session reworked the branch).
// The lease would let gt done replace it, so it compares change-sets first and
// refuses real divergence (gt-bf5x, gt-i0z3): exit 10, origin untouched.
func TestRunDoneRefusesToPushOverSomeoneElsesWork(t *testing.T) {
	var theirs string
	r := runDoneSubmit(t, passingDoneGate(), func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, true)
		other := filepath.Join(t.TempDir(), "other")
		gitOut(t, filepath.Dir(other), "clone", "-q", "-b", doneTestBranch, originURL(t, workDir), other)
		gitOut(t, other, "config", "user.email", "o@example.com")
		gitOut(t, other, "config", "user.name", "Other")
		writeMQSubmitTestFile(t, other, "theirs.txt", "their rework\n")
		gitOut(t, other, "add", "theirs.txt")
		gitOut(t, other, "commit", "-q", "-m", "their rework")
		gitOut(t, other, "push", "-q", "origin", doneTestBranch)
		theirs = gitOut(t, other, "rev-parse", "HEAD")
		// Local moves on without their commit.
		writeMQSubmitTestFile(t, workDir, "mine.txt", "mine\n")
		runGitForMQSubmitTest(t, workDir, "add", "mine.txt")
		runGitForMQSubmitTest(t, workDir, "commit", "-m", "mine")
	})
	assertDoneExitCode(t, r.err, doneExitPushFailed, "real divergence")
	if got := strings.Fields(gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/"+doneTestBranch)); len(got) == 0 || got[0] != theirs {
		t.Fatalf("origin branch = %v, want their commit %s kept", got, theirs)
	}
}

// TestRunDoneFailureClearsTheDoneIntentLabel: the label written before the
// long stages must not outlive a run that failed and reported nothing, or the
// witness restarts a polecat that is fixing its branch (gt-wmpy). A run that
// succeeded leaves it to updateAgentStateOnDone, which clears it last.
func TestRunDoneFailureClearsTheDoneIntentLabel(t *testing.T) {
	red := &recordingGate{result: land.GateResult{Steps: []land.StepResult{{Name: "test", ExitCode: 1}}}}
	r := runDoneSubmit(t, red, func(t *testing.T, workDir string) { setupRoutedSubmitGitRepo(t, workDir, false) })
	assertDoneExitCode(t, r.err, doneExitGateFailed, "local gate failed")
	if !strings.Contains(r.bdLog, "--remove-label=done-intent:COMPLETED:") {
		t.Errorf("a failed run left its done-intent label:\n%s", r.bdLog)
	}

	ok := runDoneSubmit(t, passingDoneGate(), func(t *testing.T, workDir string) { setupRoutedSubmitGitRepo(t, workDir, false) })
	if ok.err != nil {
		t.Fatalf("runDone: %v", ok.err)
	}
	if strings.Contains(ok.bdLog, "--remove-label=done-intent:") {
		t.Errorf("a reported run cleared the label before updateAgentStateOnDone:\n%s", ok.bdLog)
	}
}

// TestRunDoneGateThatCouldNotRunExits16: a gate that could not run says
// nothing about the code, so it gets its own exit code and the polecat is told
// to escalate rather than fix code.
func TestRunDoneGateThatCouldNotRunExits16(t *testing.T) {
	broken := &recordingGate{result: land.GateResult{Err: errors.New("sh: not found")}}
	r := runDoneSubmit(t, broken, func(t *testing.T, workDir string) { setupRoutedSubmitGitRepo(t, workDir, false) })
	assertDoneExitCode(t, r.err, doneExitGateUnavailable, "not a verdict on your change")
	if got := gitOut(t, r.workDir, "ls-remote", "origin", "refs/heads/"+doneTestBranch); got != "" {
		t.Errorf("a gate that did not run pushed the branch: %s", got)
	}
}

// TestRunDoneNoCodeChecksTheResolvedTarget: a branch with nothing ahead of a
// non-default target is verified as landed on THAT target and closed with it
// recorded. Checking the rig default instead would refuse work that is on the
// target it was aimed at, and record the wrong landing branch.
func TestRunDoneNoCodeChecksTheResolvedTarget(t *testing.T) {
	r := runDoneSubmitWithFlags(t, passingDoneGate(), func() { doneTarget = "release" }, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, true)
		// The feature commit is on origin/release but not on origin/main.
		runGitForMQSubmitTest(t, workDir, "push", "origin", "HEAD:refs/heads/release")
	})
	if r.err != nil {
		t.Fatalf("runDone: %v", r.err)
	}
	head := gitOut(t, r.workDir, "rev-parse", "HEAD")
	for _, want := range []string{"target_branch: release", "commit_sha: " + head} {
		if !strings.Contains(r.bdLog, want) {
			t.Errorf("bd log lacks %q:\n%s", want, r.bdLog)
		}
	}
	if strings.Contains(r.bdLog, "target_branch: main") {
		t.Errorf("close recorded the rig default as the target:\n%s", r.bdLog)
	}
}
