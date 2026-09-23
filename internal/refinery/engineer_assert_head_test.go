package refinery

import (
	"strings"
	"testing"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

// assertMRBead builds an MR bead (the gt-sda9 shape: commit_sha recorded at
// submission) for a branch that exists locally. The "unpushed" and "rewritten"
// variants simulate the trap this guard exists for: the local
// refs/heads/<branch> — the same ref a shared-.repo.git refinery reads — still
// matches commit_sha, so submittedBranchHead passes, yet origin never received
// the declared head.
func assertMRBead(id, branch, target, sourceIssue, commitSHA string) *beadsdk.Issue {
	return prepushIssue(id, beads.FormatMRFields(&beads.MRFields{
		Branch:      branch,
		Target:      target,
		SourceIssue: sourceIssue,
		Worker:      "polecats/test",
		Rig:         "test-rig",
		CommitSHA:   commitSHA,
	}), "gt:merge-request")
}

// setupAssertEngineer builds an engineer over workDir with the MR bead and the
// polecat's source-issue bead, and fetches the branch the way the guard's
// reachability check does.
func setupAssertEngineer(t *testing.T, workDir, branch, mrID, head string) *Engineer {
	t.Helper()
	store := newPrepushStore(
		prepushIssue("gt-sda9", ""),
		assertMRBead(mrID, branch, "main", "gt-sda9", head),
	)
	e := newPrepushEngineer(t, workDir, store)
	if err := e.git.FetchBranch("origin", branch); err != nil {
		// A branch origin never received has no remote ref to fetch; the
		// guard's ls-remote reads the (absent) tip directly.
		if !strings.Contains(err.Error(), "couldn't find remote ref") {
			t.Fatalf("fetch %s for guard: %v", branch, err)
		}
	}
	return e
}

func assertTestMR(mrID, branch, head string) *MRInfo {
	return &MRInfo{ID: mrID, Branch: branch, Target: "main", SourceIssue: "gt-sda9", CommitSHA: head}
}

// TestAssertSubmittedHeadReachableOnOrigin_Reachable: origin/<branch> carries
// the declared head. The guard must pass — a healthy submission must never be
// refused.
func TestAssertSubmittedHeadReachableOnOrigin_Reachable(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/flint/gt-sda9"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	head := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)
	run(t, workDir, "git", "push", "-u", "origin", branch)

	e := setupAssertEngineer(t, workDir, branch, "gt-mr-1", head)

	if err := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, head)); err != nil {
		t.Fatalf("reachable, pushed head must be accepted, got: %v", err)
	}
}

// TestAssertSubmittedHeadReachableOnOrigin_UnreachableUnpushed: the core gt-sda9
// case — the declared head is the local branch tip and the local ref matches
// (so submittedBranchHead, which reads the same ref in a shared .repo.git rig,
// passes), yet origin never received it. The guard must refuse loudly with a
// clear reason, not spend gates on it.
func TestAssertSubmittedHeadReachableOnOrigin_UnreachableUnpushed(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/flint/gt-sda9"
	createFeatureBranch(t, workDir, branch, "unpushed.txt", "local only\n")
	head := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)

	// The local ref matches commit_sha — the trap.
	if localRef := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch); localRef != head {
		t.Fatalf("test setup: local ref %s != head %s", localRef, head)
	}
	// And origin has no such branch.
	if tip := run(t, workDir, "git", "ls-remote", "origin", "refs/heads/"+branch); tip != "" {
		t.Fatalf("test setup: origin has %s (%q) but this case wants it absent", branch, tip)
	}

	e := setupAssertEngineer(t, workDir, branch, "gt-mr-1", head)

	err := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, head))
	if err == nil {
		t.Fatal("unpushed head must be refused, got nil")
	}
	for _, want := range []string{"refusing to gate", "not reachable from origin's tip", branch} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal reason missing %q:\n%s", want, err.Error())
		}
	}
}

// TestAssertSubmittedHeadReachableOnOrigin_UnreachableRewritten: origin has the
// branch at a different tip and the declared head is not an ancestor of it (a
// rewrite, not a fast-forward — distinct trees, so no patch-preservation
// fallback can excuse it). The guard must refuse: the MR recorded a head
// origin no longer has in history.
func TestAssertSubmittedHeadReachableOnOrigin_UnreachableRewritten(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/flint/gt-sda9"
	createFeatureBranch(t, workDir, branch, "old.txt", "v1\n")
	declared := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)
	run(t, workDir, "git", "push", "-u", "origin", branch)

	// A history rewrite with disjoint trees makes the new tip neither the
	// declared head nor an ancestor-descendant of it (a fast-forward would
	// keep the declared head reachable, which is the legitimate case) — and,
	// unlike a same-content amend, nothing in the patch-preservation fallback
	// can excuse it.
	run(t, workDir, "git", "checkout", "--orphan", branch+"-rewritten", declared)
	writeFile(t, workDir, "old.txt", "rewritten-v2\n")
	run(t, workDir, "git", "commit", "-m", "chore: rewrite branch")
	rewritten := run(t, workDir, "git", "rev-parse", "HEAD")
	run(t, workDir, "git", "branch", "-f", branch, "HEAD")
	// The temp branch stays checked out with the orphan's tree in the index;
	// hard-reset to it so switching back to main is not blocked by a dirty
	// index/worktree.
	run(t, workDir, "git", "reset", "-q", "--hard")
	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "branch", "-D", branch+"-rewritten")
	run(t, workDir, "git", "push", "--force", "origin", branch)
	if declared == rewritten {
		t.Fatal("test setup: rewrite did not move the branch")
	}

	e := setupAssertEngineer(t, workDir, branch, "gt-mr-1", declared)

	err := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, declared))
	if err == nil {
		t.Fatal("rewritten-away head must be refused, got nil")
	}
	if !strings.Contains(err.Error(), "not reachable from origin's tip") {
		t.Errorf("refusal reason missing reachability wording: %s", err.Error())
	}
}