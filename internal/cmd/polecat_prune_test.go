package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
)

// oldEnoughSuffix returns a generated-branch timestamp suffix old enough to
// clear minRemoteBranchPruneAge, so a test's branch is judged purely on
// preservation/liveness rather than incidentally tripping the age gate.
func oldEnoughSuffix() string {
	return strconv.FormatInt(time.Now().Add(-2*minRemoteBranchPruneAge).UnixMilli(), 36)
}

// freshSuffix returns a generated-branch timestamp suffix from "just now",
// well inside minRemoteBranchPruneAge.
func freshSuffix() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 36)
}

// notFoundLookup simulates no polecat identity existing under any name —
// the common "truly stale, identity long retired" case.
func notFoundLookup(string) (polecat.State, error) {
	return "", polecat.ErrPolecatNotFound
}

// stubRemotePolecatBranchOpenPR replaces the --remote prune's open-PR guard for
// one test, reporting every branch named in openPRs as carrying an open pull
// request and every other branch as clear. It returns a pointer to the refs the
// guard was asked about, in call order, so a test can assert the guard is
// consulted with the branch and tip being deleted rather than, say, the target
// ref it was preserved on.
//
// The production lookup cannot run in these tests: they use local file remotes,
// and git.Git.HasOpenPullRequest fails closed on any error that is not "no such
// PR" — including "remote is not a GitHub repo" — so it would report every
// branch as protected and mask the rules under test.
func stubRemotePolecatBranchOpenPR(t *testing.T, openPRs ...string) *[]git.PullRequestRef {
	t.Helper()
	var calls []git.PullRequestRef
	old := remotePolecatBranchHasOpenPR
	remotePolecatBranchHasOpenPR = func(_ *git.Git, branch, headSHA string) bool {
		calls = append(calls, git.PullRequestRef{Branch: branch, HeadSHA: headSHA})
		return slices.Contains(openPRs, branch)
	}
	t.Cleanup(func() { remotePolecatBranchHasOpenPR = old })
	return &calls
}

// workingLookup simulates a polecat that is actively working — live, in the
// sense that matters for remote pruning.
func workingLookup(string) (polecat.State, error) {
	return polecat.StateWorking, nil
}

func TestPruneRemotePolecatBranchesDryRunIncludesPatchEquivalentBranch(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	localDir, mainBranch := initPolecatPruneTestRepo(t)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("prunepatch", "", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)
	if err := repoGit.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune: %v", err)
	}

	out := captureStdout(t, func() {
		result, err := pruneRemotePolecatBranches(notFoundLookup, repoGit, true)
		if err != nil {
			t.Fatalf("pruneRemotePolecatBranches: %v", err)
		}
		if result.Pruned != 1 {
			t.Fatalf("Pruned = %d, want 1", result.Pruned)
		}
	})
	assertRemotePruneDryRunKeptBranch(t, repoGit, out, branch)
}

func TestRunPolecatPruneRemoteDryRunIncludesPatchEquivalentBranch(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	townRoot, rigName := setupTestRigForSettings(t)
	localDir := filepath.Join(townRoot, rigName, "mayor", "rig")
	mainBranch := initPolecatPruneTestRepoAt(t, localDir)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("prunecmdpatch", "", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)

	oldRemote, oldDryRun := polecatPruneRemote, polecatPruneDryRun
	polecatPruneRemote = true
	polecatPruneDryRun = true
	t.Cleanup(func() {
		polecatPruneRemote = oldRemote
		polecatPruneDryRun = oldDryRun
	})

	out := captureStdout(t, func() {
		if err := runPolecatPrune(nil, []string{rigName}); err != nil {
			t.Fatalf("runPolecatPrune: %v", err)
		}
	})
	assertRemotePruneDryRunKeptBranch(t, repoGit, out, branch)
}

func TestPruneRemotePolecatBranchesUsesUpstreamBaseForOriginFork(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	localDir, mainBranch := initPolecatPruneOriginForkRepo(t)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("forkonly", "", oldEnoughSuffix())

	runGit(t, localDir, "checkout", "-b", branch, "upstream/"+mainBranch)
	writePolecatPruneTestFile(t, filepath.Join(localDir, "fork-only.txt"), "fork only\n")
	runGit(t, localDir, "add", "fork-only.txt")
	runGit(t, localDir, "commit", "-m", "fork-only branch work")
	branchSHA, err := repoGit.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev branch: %v", err)
	}
	runGit(t, localDir, "push", "origin", branch)

	runGit(t, localDir, "checkout", mainBranch)
	runGit(t, localDir, "reset", "--hard", "origin/"+mainBranch)
	writePolecatPruneTestFile(t, filepath.Join(localDir, "fork-advance.txt"), "fork advanced\n")
	runGit(t, localDir, "add", "fork-advance.txt")
	runGit(t, localDir, "commit", "-m", "advance fork target")
	runGit(t, localDir, "cherry-pick", strings.TrimSpace(branchSHA))
	runGit(t, localDir, "push", "origin", mainBranch)
	if err := repoGit.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune origin: %v", err)
	}
	if err := repoGit.FetchPrune("upstream"); err != nil {
		t.Fatalf("FetchPrune upstream: %v", err)
	}
	if got := repoGit.CleanDefaultBranchBaseRef("origin", mainBranch); got != "upstream/"+mainBranch {
		t.Fatalf("CleanDefaultBranchBaseRef = %q, want upstream/%s", got, mainBranch)
	}

	out := captureStdout(t, func() {
		result, err := pruneRemotePolecatBranches(notFoundLookup, repoGit, true)
		if err != nil {
			t.Fatalf("pruneRemotePolecatBranches: %v", err)
		}
		if result.Pruned != 0 {
			t.Fatalf("Pruned = %d, want 0 because branch is not preserved on upstream/%s", result.Pruned, mainBranch)
		}
	})
	if strings.Contains(out, branch) {
		t.Fatalf("dry-run output %q should not include fork-only branch %s", out, branch)
	}
	exists, err := repoGit.RemoteBranchExists("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchExists: %v", err)
	}
	if !exists {
		t.Fatal("fork-only branch should remain on origin")
	}
}

func TestPruneRemotePolecatBranchesNeverPrunesFreshNoCommitBranch(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	localDir, mainBranch := initPolecatPruneTestRepo(t)
	repoGit := git.NewGit(localDir)
	// A branch generated moments ago, with zero commits ahead of main, is
	// trivially "preserved" by ancestry alone — indistinguishable from a
	// truly stale, already-merged branch. This is exactly the shape of a
	// freshly spawned polecat's branch during a dispatch burst (gt-527j).
	branch := polecat.FormatGeneratedBranchName("freshcat", "gt-fresh", freshSuffix())
	pushNoCommitPolecatBranch(t, localDir, mainBranch, branch)
	if err := repoGit.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune: %v", err)
	}

	// Even a lookup reporting no live identity at all must not save this
	// branch — the age gate is a hard backstop, not merely a tie-breaker
	// consulted after liveness clears.
	result, err := pruneRemotePolecatBranches(notFoundLookup, repoGit, false)
	if err != nil {
		t.Fatalf("pruneRemotePolecatBranches: %v", err)
	}
	if result.Pruned != 0 {
		t.Fatalf("Pruned = %d, want 0 — a branch generated moments ago must never be pruned", result.Pruned)
	}
	assertRemoteBranchStillExists(t, repoGit, branch)
}

func TestPruneRemotePolecatBranchesNeverPrunesLiveWorkingPolecatEvenIfOld(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	localDir, mainBranch := initPolecatPruneTestRepo(t)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("livecat", "gt-live", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)
	if err := repoGit.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune: %v", err)
	}

	// Old enough to clear the age gate on its own, but the polecat that owns
	// it is still actively working — the liveness check must block deletion
	// independently of age.
	result, err := pruneRemotePolecatBranches(workingLookup, repoGit, false)
	if err != nil {
		t.Fatalf("pruneRemotePolecatBranches: %v", err)
	}
	if result.Pruned != 0 {
		t.Fatalf("Pruned = %d, want 0 — a live polecat's branch must never be pruned", result.Pruned)
	}
	assertRemoteBranchStillExists(t, repoGit, branch)
}

func TestPruneRemotePolecatBranchesPrunesTrulyStaleMergedBranch(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	localDir, mainBranch := initPolecatPruneTestRepo(t)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("stalecat", "gt-stale", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)
	if err := repoGit.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune: %v", err)
	}

	result, err := pruneRemotePolecatBranches(notFoundLookup, repoGit, false)
	if err != nil {
		t.Fatalf("pruneRemotePolecatBranches: %v", err)
	}
	if result.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1 — an old, merged, identity-retired branch should be pruned", result.Pruned)
	}
	exists, err := repoGit.RemoteBranchExists("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchExists: %v", err)
	}
	if exists {
		t.Fatal("truly stale merged branch should have been deleted")
	}
}

// TestPruneRemotePolecatBranchesLeavesBranchWithOpenPR covers the hazard
// gas-fk4 closed on gt mq post-merge's deletion path (gt-o5rx): a branch that
// clears every other prune gate — old enough, owner retired, its work already
// preserved on the target — is still not deletable while an open pull request
// points at it, because deleting the branch makes GitHub auto-close the PR as
// "closed" instead of "merged" and destroys the audit trail.
func TestPruneRemotePolecatBranchesLeavesBranchWithOpenPR(t *testing.T) {
	localDir, mainBranch := initPolecatPruneTestRepo(t)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("prcat", "gt-pr", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)
	if err := repoGit.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune: %v", err)
	}
	branchSHA, err := repoGit.Rev("origin/" + branch)
	if err != nil {
		t.Fatalf("Rev origin/%s: %v", branch, err)
	}

	calls := stubRemotePolecatBranchOpenPR(t, branch)
	out := captureStdout(t, func() {
		result, err := pruneRemotePolecatBranches(notFoundLookup, repoGit, false)
		if err != nil {
			t.Fatalf("pruneRemotePolecatBranches: %v", err)
		}
		if result.Pruned != 0 {
			t.Fatalf("Pruned = %d, want 0 — a branch with an open PR must not be deleted", result.Pruned)
		}
		if result.OpenPR != 1 {
			t.Fatalf("OpenPR = %d, want 1", result.OpenPR)
		}
	})
	if !strings.Contains(out, "open PR exists (gas-fk4)") || !strings.Contains(out, branch) {
		t.Fatalf("output %q should report %s as skipped for an open PR", out, branch)
	}
	// The guard must be asked about the branch being deleted, pinned to the tip
	// that delete would use — not the target ref the work was preserved on.
	if len(*calls) != 1 {
		t.Fatalf("open-PR guard called %d times, want 1: %+v", len(*calls), *calls)
	}
	if got := (*calls)[0]; got.Branch != branch || got.HeadSHA != strings.TrimSpace(branchSHA) {
		t.Fatalf("open-PR guard called with %+v, want branch %s at %s", got, branch, strings.TrimSpace(branchSHA))
	}
	assertRemoteBranchStillExists(t, repoGit, branch)

	// A dry run answers what a real run would do, so it must report the same
	// skip rather than promising a delete the guarded run would refuse.
	dryOut := captureStdout(t, func() {
		result, err := pruneRemotePolecatBranches(notFoundLookup, repoGit, true)
		if err != nil {
			t.Fatalf("pruneRemotePolecatBranches dry run: %v", err)
		}
		if result.Pruned != 0 || result.OpenPR != 1 {
			t.Fatalf("dry run = %+v, want Pruned 0 and OpenPR 1", result)
		}
	})
	if strings.Contains(dryOut, "Would delete remote") {
		t.Fatalf("dry-run output %q must not promise the PR-protected branch a delete", dryOut)
	}
	if !strings.Contains(dryOut, "open PR exists (gas-fk4)") {
		t.Fatalf("dry-run output %q should report the skip for an open PR", dryOut)
	}
}

// TestRunPolecatPruneReportsRemoteBranchKeptForOpenPR pins the command-level
// reporting for the same case: a run that pruned nothing because a branch is
// PR-protected must not report "No stale remote polecat branches found", which
// would say the opposite of what happened.
func TestRunPolecatPruneReportsRemoteBranchKeptForOpenPR(t *testing.T) {
	townRoot, rigName := setupTestRigForSettings(t)
	localDir := filepath.Join(townRoot, rigName, "mayor", "rig")
	mainBranch := initPolecatPruneTestRepoAt(t, localDir)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("prcmdcat", "", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)
	stubRemotePolecatBranchOpenPR(t, branch)

	oldRemote, oldDryRun := polecatPruneRemote, polecatPruneDryRun
	polecatPruneRemote = true
	polecatPruneDryRun = false
	t.Cleanup(func() {
		polecatPruneRemote = oldRemote
		polecatPruneDryRun = oldDryRun
	})

	out := captureStdout(t, func() {
		if err := runPolecatPrune(nil, []string{rigName}); err != nil {
			t.Fatalf("runPolecatPrune: %v", err)
		}
	})
	if strings.Contains(out, "No stale remote polecat branches found") {
		t.Fatalf("output %q must not claim no stale branches were found", out)
	}
	if !strings.Contains(out, "left in place: open PR exists (gas-fk4)") {
		t.Fatalf("output %q should report the branch kept for an open PR", out)
	}
	assertRemoteBranchStillExists(t, repoGit, branch)
}

func pushNoCommitPolecatBranch(t *testing.T, localDir, mainBranch, branch string) {
	t.Helper()
	runGit(t, localDir, "checkout", "-b", branch, mainBranch)
	runGit(t, localDir, "push", "origin", branch)
	runGit(t, localDir, "checkout", mainBranch)
}

func assertRemoteBranchStillExists(t *testing.T, repoGit *git.Git, branch string) {
	t.Helper()
	exists, err := repoGit.RemoteBranchExists("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchExists: %v", err)
	}
	if !exists {
		t.Fatalf("branch %s must not have been deleted", branch)
	}
}

func createPatchEquivalentRemoteBranch(t *testing.T, repoGit *git.Git, localDir, mainBranch, branch string) {
	t.Helper()
	runGit(t, localDir, "checkout", "-b", branch, mainBranch)
	writePolecatPruneTestFile(t, filepath.Join(localDir, "feature.txt"), "feature\n")
	runGit(t, localDir, "add", "feature.txt")
	runGit(t, localDir, "commit", "-m", "feature work")
	branchSHA, err := repoGit.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev branch: %v", err)
	}
	runGit(t, localDir, "push", "origin", branch)

	runGit(t, localDir, "checkout", mainBranch)
	writePolecatPruneTestFile(t, filepath.Join(localDir, "advance.txt"), "target advanced\n")
	runGit(t, localDir, "add", "advance.txt")
	runGit(t, localDir, "commit", "-m", "advance target")
	runGit(t, localDir, "cherry-pick", strings.TrimSpace(branchSHA))
	runGit(t, localDir, "push", "origin", mainBranch)
}

func assertRemotePruneDryRunKeptBranch(t *testing.T, repoGit *git.Git, out, branch string) {
	t.Helper()
	if !strings.Contains(out, "Would delete remote") || !strings.Contains(out, branch) {
		t.Fatalf("dry-run output %q, want branch %s", out, branch)
	}
	exists, err := repoGit.RemoteBranchExists("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchExists: %v", err)
	}
	if !exists {
		t.Fatal("dry-run should not delete the remote branch")
	}
}

func initPolecatPruneTestRepo(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	localDir := filepath.Join(tmp, "local")
	return localDir, initPolecatPruneTestRepoAt(t, localDir)
}

func initPolecatPruneTestRepoAt(t *testing.T, localDir string) string {
	t.Helper()
	tmp := t.TempDir()
	remoteDir := filepath.Join(tmp, "remote.git")
	mainBranch := "main"

	if err := os.MkdirAll(filepath.Dir(localDir), 0755); err != nil {
		t.Fatalf("mkdir repo parent: %v", err)
	}
	runGit(t, tmp, "init", "--bare", "--initial-branch", mainBranch, remoteDir)
	runGit(t, tmp, "clone", remoteDir, localDir)
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test User")
	writePolecatPruneTestFile(t, filepath.Join(localDir, "README.md"), "test\n")
	runGit(t, localDir, "add", "README.md")
	runGit(t, localDir, "commit", "-m", "initial")
	runGit(t, localDir, "push", "-u", "origin", mainBranch)

	return mainBranch
}

func initPolecatPruneOriginForkRepo(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	upstreamDir := filepath.Join(tmp, "upstream.git")
	forkDir := filepath.Join(tmp, "fork.git")
	localDir := filepath.Join(tmp, "local")
	mainBranch := "main"

	runGit(t, tmp, "init", "--bare", "--initial-branch", mainBranch, upstreamDir)
	runGit(t, tmp, "init", "--bare", "--initial-branch", mainBranch, forkDir)
	runGit(t, tmp, "clone", upstreamDir, localDir)
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test User")
	writePolecatPruneTestFile(t, filepath.Join(localDir, "README.md"), "test\n")
	runGit(t, localDir, "add", "README.md")
	runGit(t, localDir, "commit", "-m", "initial")
	runGit(t, localDir, "push", "-u", "origin", mainBranch)
	runGit(t, localDir, "push", forkDir, mainBranch)
	runGit(t, localDir, "remote", "set-url", "origin", forkDir)
	runGit(t, localDir, "remote", "add", "upstream", upstreamDir)
	runGit(t, localDir, "fetch", "origin")
	runGit(t, localDir, "fetch", "upstream")

	return localDir, mainBranch
}

func writePolecatPruneTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
