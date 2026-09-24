package cmd

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

// TestRunVerifiedMQPostMerge_RealRemoteRewrittenBranchWithStaleRecordedSHA
// pins the branch-delete half of gt-lbzd against a real remote: the branch is
// rewritten after submission (same content, new identity), so the commit_sha
// the MR bead recorded is a dangling object that is reachable from neither the
// branch nor the merge target, while the refinery merged the branch's live tip.
//
// The other real-remote post-merge tests all advance the branch by pushing
// onto it, which leaves the recorded head an ancestor of the target. This one
// rewrites it, so the recorded head is unreachable and the merge proof can only
// hold by patch-id content preservation. Both shapes must delete at the live
// tip: a guard that asserts the recorded sha is rejected by the remote as
// "stale info" and leaks the branch, which is the failure gt-lbzd reports.
func TestRunVerifiedMQPostMerge_RealRemoteRewrittenBranchWithStaleRecordedSHA(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	originPath := filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone := filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "README.md", "seed\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	const branch = "polecat/sapphire/gt-azmw+mu4fcwv4"
	runOrphanCleanupGit(t, clone, "checkout", "-b", branch)
	writeOrphanCleanupFile(t, clone, "work.txt", "polecat work\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "polecat work")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", branch)

	recordedSHA := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")

	// Amending with a new date keeps the tree and parent — so the patch-id is
	// unchanged and the proof still holds — while giving the branch a new tip.
	// The recorded commit is left behind with nothing pointing at it.
	runOrphanCleanupGit(t, clone, "commit", "--amend", "--no-edit",
		"--date=2026-09-17T12:00:00+00:00")
	liveSHA := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	if liveSHA == recordedSHA {
		t.Fatal("premise broken: the rewrite did not change the branch tip")
	}
	runOrphanCleanupGit(t, clone, "push", "--force", "origin", branch)

	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--no-ff", "-m", "merge "+branch, branch)
	runOrphanCleanupGit(t, clone, "push", "origin", "main")

	// The shape under test: the recorded head is on no branch. `--is-ancestor`
	// exits non-zero for exactly this, which is why it cannot go through
	// runOrphanCleanupGit's fail-on-error helper.
	notAncestor := exec.Command("git", "merge-base", "--is-ancestor", recordedSHA, "origin/main")
	notAncestor.Dir = clone
	if err := notAncestor.Run(); err == nil {
		t.Fatalf("premise broken: recorded head %s is reachable from origin/main", recordedSHA)
	}

	// A fresh clone stands in for the refinery's worktree, matching the other
	// real-remote tests: the delete must work from a repo whose
	// remote-tracking refs were never moved to the rewritten tip.
	postMergeDir := filepath.Join(tmp, "postmerge")
	runOrphanCleanupGit(t, tmp, "clone", originPath, postMergeDir)
	rigGit := orphanCleanupRealGit{git.NewGit(postMergeDir)}

	mgr := &fakeMQPostMergeManager{mr: &refinery.MergeRequest{
		ID:           "gt-wisp-4bi",
		Branch:       branch,
		Worker:       "polecats/sapphire",
		IssueID:      "gt-azmw",
		TargetBranch: "main",
		CommitSHA:    recordedSHA,
	}}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("a rewritten branch whose content landed must still be cleaned up: %v", err)
	}
	if cleanup.AlreadyGone {
		t.Fatal("cleanup reported the branch already gone; the delete never happened")
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted at the live tip", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", branch); remote != "" {
		t.Fatalf("remote branch survived cleanup: %s", remote)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called")
	}
}
