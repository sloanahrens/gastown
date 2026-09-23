package refinery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// addWorktreeHolding checks out branch in a second worktree of the same
// repository. Git refuses to check a branch out twice, so while that worktree
// exists every `git checkout branch` elsewhere fails — the state a live polecat
// worktree sitting on main produced (gt-032w).
func addWorktreeHolding(t *testing.T, workDir, branch string) string {
	t.Helper()
	holder := filepath.Join(t.TempDir(), "holder")
	run(t, workDir, "git", "worktree", "add", holder, branch)
	return holder
}

// expectCheckoutRefused asserts git will not check branch out here, which is
// the condition under test; a scenario that only looks like a held branch
// would otherwise pass while proving nothing.
func expectCheckoutRefused(t *testing.T, workDir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", branch)
	cmd.Dir = workDir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("precondition failed: git checked out %s despite the holder worktree\n%s", branch, out)
	}
}

// engineerOutput returns what the Engineer logged, for assertions that a
// fallback path was the one taken.
func engineerOutput(t *testing.T, e *Engineer) string {
	t.Helper()
	buf, ok := e.output.(interface{ String() string })
	if !ok {
		t.Fatalf("engineer output is %T, not a string buffer", e.output)
	}
	return buf.String()
}

func TestPrepareMergeTarget_StagesTargetOnItsOriginTip(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	if err := e.prepareMergeTarget("main"); err != nil {
		t.Fatalf("prepareMergeTarget: %v", err)
	}

	if got := run(t, workDir, "git", "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("expected to be on main, got %q", got)
	}
	head := run(t, workDir, "git", "rev-parse", "HEAD")
	if originHead := run(t, workDir, "git", "rev-parse", "origin/main"); head != originHead {
		t.Errorf("expected HEAD at origin/main %s, got %s", originHead, head)
	}
}

func TestPrepareMergeTarget_DropsLocalTargetCommitsAheadOfOrigin(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	originHead := run(t, workDir, "git", "rev-parse", "origin/main")

	// Another agent commits onto the shared local main and leaves it there,
	// then walks away: the classic shape behind the near-miss where a WIP
	// checkpoint was one push away from being published as main's tip.
	writeFile(t, workDir, "wip.txt", "wip\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "WIP: checkpoint (auto)")
	if got := run(t, workDir, "git", "rev-parse", "main"); got == originHead {
		t.Fatal("precondition failed: local main did not advance past origin/main")
	}
	run(t, workDir, "git", "checkout", "-b", "someone-elses-branch")

	if err := e.prepareMergeTarget("main"); err != nil {
		t.Fatalf("prepareMergeTarget: %v", err)
	}

	head := run(t, workDir, "git", "rev-parse", "HEAD")
	if head != originHead {
		t.Errorf("merge base is the local main %s, not origin/main %s", head, originHead)
	}
}

func TestPrepareMergeTarget_DetachesWhenAnotherWorktreeHoldsTarget(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	originHead := run(t, workDir, "git", "rev-parse", "origin/main")

	// Move off main so a second worktree can hold it.
	run(t, workDir, "git", "checkout", "-b", "refinery/scratch")
	addWorktreeHolding(t, workDir, "main")
	expectCheckoutRefused(t, workDir, "main")

	if err := e.prepareMergeTarget("main"); err != nil {
		t.Fatalf("prepareMergeTarget with main held elsewhere: %v", err)
	}

	if got := run(t, workDir, "git", "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("expected a detached HEAD staging main, got branch %q", got)
	}
	head := run(t, workDir, "git", "rev-parse", "HEAD")
	if head != originHead {
		t.Errorf("expected detached HEAD at origin/main %s, got %s", originHead, head)
	}
	if out := engineerOutput(t, e); !strings.Contains(out, "detached HEAD") {
		t.Errorf("expected the fallback to be logged, got:\n%s", out)
	}
}

func TestWorktreeHolding_IgnoresThisWorktree(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	if holder, held := e.worktreeHolding("main"); held {
		t.Errorf("this worktree reported itself as the holder of main: %s", holder)
	}
}

func TestBuildRebaseStack_StagesWhenAnotherWorktreeHoldsTarget(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	run(t, workDir, "git", "checkout", "feature-a")
	addWorktreeHolding(t, workDir, "main")
	expectCheckoutRefused(t, workDir, "main")

	batch := []*MRInfo{makeMR("mr-a", "feature-a", "main"), makeMR("mr-b", "feature-b", "main")}
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("BuildRebaseStack with main held elsewhere: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts, got %d", len(conflicts))
	}
	if len(stacked) != 2 {
		t.Fatalf("expected both MRs stacked, got %d", len(stacked))
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, statErr := os.Stat(filepath.Join(workDir, name)); statErr != nil {
			t.Errorf("expected %s in the stacked tree: %v", name, statErr)
		}
	}
}

func TestDoMerge_SucceedsWhenAnotherWorktreeHoldsTarget(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	branch := "polecat/test/held-main"
	createFeatureBranch(t, workDir, branch, "held.txt", "held\n")
	head := run(t, workDir, "git", "rev-parse", branch)

	// The refinery does not hold main; a live polecat worktree does.
	run(t, workDir, "git", "checkout", branch)
	holder := addWorktreeHolding(t, workDir, "main")
	expectCheckoutRefused(t, workDir, "main")

	mr := &MRInfo{ID: "mr-held-main", Branch: branch, Target: "main"}
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed with main held by another worktree: %s\n%s", result.Error, engineerOutput(t, e))
	}

	parents := run(t, workDir, "git", "log", "-1", "--format=%P", "origin/main")
	if !strings.Contains(parents, head) {
		t.Errorf("landed merge commit %s does not carry the submitted head %s (parents %q)", result.MergeCommit, head, parents)
	}
	if got := run(t, holder, "git", "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("the holding worktree was moved off main, now on %q", got)
	}
}

// TestDoMerge_RealignsWhenLocalTargetAheadOfOrigin covers gt-u093 fix (b) on
// the single-MR path: a stray commit left on the shared local main by another
// agent (gt-032w) or a run that exited before restoreTargetToOrigin must not
// permanently block merging. doMerge realigns target to origin up front and
// proceeds — prepareMergeTarget was going to discard the stray commit anyway.
func TestDoMerge_RealignsWhenLocalTargetAheadOfOrigin(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)
	originMain := run(t, workDir, "git", "rev-parse", "origin/main")
	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "leftover.txt", "leftover\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "Merge rejected-branch into main (gt-xxxx)")
	leftoverSHA := run(t, workDir, "git", "rev-parse", "main")

	mr := makeMR("mr-a", "feature-a", "main")
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("expected doMerge to realign and proceed when local main is ahead of origin/main: %s", result.Error)
	}

	if err := exec.Command("git", "-C", workDir, "merge-base", "--is-ancestor", leftoverSHA, "origin/main").Run(); err == nil {
		t.Errorf("leftover commit %s is still reachable from origin/main — it should have been discarded by realignment", leftoverSHA)
	}
	if err := exec.Command("git", "-C", workDir, "merge-base", "--is-ancestor", originMain, "origin/main").Run(); err != nil {
		t.Errorf("origin/main's prior tip %s is not an ancestor of the landed merge", originMain)
	}
}

// TestRestoreTargetToOrigin_Attached covers gt-u093 fix (a) when this
// worktree has target checked out: restoreTargetToOrigin must hard-reset it.
func TestRestoreTargetToOrigin_Attached(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	originMain := run(t, workDir, "git", "rev-parse", "origin/main")

	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "extra.txt", "extra\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "ahead of origin")

	if err := e.restoreTargetToOrigin("main"); err != nil {
		t.Fatalf("restoreTargetToOrigin: %v", err)
	}

	if got := run(t, workDir, "git", "rev-parse", "main"); got != originMain {
		t.Errorf("expected main reset to origin/main %s, got %s", originMain, got)
	}
}

// TestRestoreTargetToOrigin_DetachedMovesBranchPointer covers gt-u093 fix (a)
// in the case its bug report names explicitly: a run staged on a detached
// HEAD (because this worktree moved off target after advancing it) must reset
// its own HEAD and working tree, not just target's ref — a plain `branch -f`
// only moves the latter.
func TestRestoreTargetToOrigin_DetachedMovesBranchPointer(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	originMain := run(t, workDir, "git", "rev-parse", "origin/main")

	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "extra.txt", "extra\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "ahead of origin")
	run(t, workDir, "git", "checkout", "--detach")

	if err := e.restoreTargetToOrigin("main"); err != nil {
		t.Fatalf("restoreTargetToOrigin: %v", err)
	}

	if got := run(t, workDir, "git", "rev-parse", "main"); got != originMain {
		t.Errorf("expected main force-updated to origin/main %s, got %s", originMain, got)
	}
	if got := run(t, workDir, "git", "rev-parse", "HEAD"); got != originMain {
		t.Errorf("expected this worktree's detached HEAD reset to origin/main %s, got %s", originMain, got)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "extra.txt")); statErr == nil {
		t.Error("expected extra.txt from the ahead commit gone from the working tree after restore")
	}
}

// TestRestoreTargetToOrigin_HeldElsewhere_NoOp covers gt-u093 fix (a) when
// another worktree holds target: this run never advanced refs/heads/target,
// so there is nothing to restore and it must not error.
func TestRestoreTargetToOrigin_HeldElsewhere_NoOp(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	run(t, workDir, "git", "checkout", "-b", "refinery/scratch")
	addWorktreeHolding(t, workDir, "main")
	expectCheckoutRefused(t, workDir, "main")

	if err := e.restoreTargetToOrigin("main"); err != nil {
		t.Fatalf("restoreTargetToOrigin with main held elsewhere: %v", err)
	}
}
