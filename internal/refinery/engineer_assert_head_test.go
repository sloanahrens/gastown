package refinery

import (
	"context"
	"os"
	"path/filepath"
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
// polecat's source-issue bead.
func setupAssertEngineer(t *testing.T, workDir, branch, mrID, head string) *Engineer {
	t.Helper()
	store := newPrepushStore(
		prepushIssue("gt-sda9", ""),
		assertMRBead(mrID, branch, "main", "gt-sda9", head),
	)
	return newPrepushEngineer(t, workDir, store)
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

	if refusal := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, head), head); refusal != nil {
		t.Fatalf("reachable, pushed head must be accepted, got: %v", refusal.Err)
	}
}

// TestAssertSubmittedHeadReachableOnOrigin_UnreachableUnpushed: the core gt-sda9
// case — the declared head is the local branch tip and the local ref matches
// (so submittedBranchHead, which reads the same ref in a shared .repo.git rig,
// passes), yet origin never received it. The guard must refuse loudly, and
// classify the refusal so the caller escalates instead of nudging the polecat.
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

	refusal := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, head), head)
	if refusal == nil {
		t.Fatal("unpushed head must be refused, got nil")
	}
	for _, want := range []string{"refusing to gate", "origin has no branch", branch} {
		if !strings.Contains(refusal.Err.Error(), want) {
			t.Errorf("refusal reason missing %q:\n%s", want, refusal.Err.Error())
		}
	}
	if !refusal.BranchMissing {
		t.Error("a branch origin never received must classify as BranchMissing so the refusal escalates")
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

	refusal := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, declared), declared)
	if refusal == nil {
		t.Fatal("rewritten-away head must be refused, got nil")
	}
	if !strings.Contains(refusal.Err.Error(), "not reachable from origin/") {
		t.Errorf("refusal reason missing reachability wording: %s", refusal.Err.Error())
	}
	if refusal.BranchMissing {
		t.Error("the branch exists on origin, so this is a submission refusal, not a missing-branch escalation")
	}
}

// TestAssertSubmittedHeadReachableOnOrigin_OriginUnreadable: origin cannot be
// read at all (here: its URL points nowhere). The guard must still fail closed,
// but classify the refusal as infrastructure so no polecat is nudged to push a
// branch that may already be published.
func TestAssertSubmittedHeadReachableOnOrigin_OriginUnreadable(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/flint/gt-sda9"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	head := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)

	e := setupAssertEngineer(t, workDir, branch, "gt-mr-1", head)
	run(t, workDir, "git", "remote", "set-url", "origin", filepath.Join(workDir, "no-such-remote.git"))

	refusal := e.assertSubmittedHeadReachableOnOrigin(assertTestMR("gt-mr-1", branch, head), head)
	if refusal == nil {
		t.Fatal("an unreadable origin must fail closed, got nil")
	}
	if !refusal.OriginUnreadable {
		t.Errorf("a read failure must classify as OriginUnreadable, got: %+v", refusal)
	}
	if refusal.BranchMissing {
		t.Error("an unreadable origin says nothing about whether the branch exists")
	}
	if !strings.Contains(refusal.Err.Error(), "cannot read origin") {
		t.Errorf("refusal reason missing the read-failure wording: %s", refusal.Err.Error())
	}
}

// TestDoMerge_UnreachableHeadRefusesBeforeGates: the doMerge half of the guard.
// An unpushed head must refuse before any gate runs — spending the suite on
// work origin never received is the cost this assertion exists to avoid — and
// nothing may reach origin on the way out.
func TestDoMerge_UnreachableHeadRefusesBeforeGates(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/flint/gt-sda9"
	createFeatureBranch(t, workDir, branch, "unpushed.txt", "local only\n")
	head := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)

	gateMarker := filepath.Join(workDir, "gate-ran")
	e := setupAssertEngineer(t, workDir, branch, "gt-mr-1", head)
	e.config.Gates = map[string]*GateConfig{
		"marker": {Cmd: "touch " + gateMarker},
	}
	beforeMain := run(t, workDir, "git", "rev-parse", "origin/main")

	result := e.doMerge(context.Background(), assertTestMR("gt-mr-1", branch, head))

	if result.Success {
		t.Fatal("doMerge must refuse an unpushed head")
	}
	if !result.BranchNotFound {
		t.Errorf("origin has no such branch, so the refusal must classify as BranchNotFound: %+v", result)
	}
	if _, err := os.Stat(gateMarker); !os.IsNotExist(err) {
		t.Error("a gate ran before the reachability assertion refused the merge")
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != beforeMain {
		t.Errorf("origin/main moved %s → %s: a refused merge must publish nothing", shortSHA(beforeMain), shortSHA(after))
	}
}

// TestBuildRebaseStack_UnreachableHeadDropsMember: the batch path applies the
// same assertion as doMerge. One unstrackable member must be reported and left
// out of the stack, not stacked and gated, and must not fail the whole batch.
func TestBuildRebaseStack_UnreachableHeadDropsMember(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const pushed = "polecat/flint/gt-pushed"
	createFeatureBranch(t, workDir, pushed, "pushed.txt", "pushed\n")
	pushedHead := run(t, workDir, "git", "rev-parse", "refs/heads/"+pushed)
	run(t, workDir, "git", "push", "-u", "origin", pushed)

	const unpushed = "polecat/flint/gt-unpushed"
	createFeatureBranch(t, workDir, unpushed, "unpushed.txt", "local only\n")
	unpushedHead := run(t, workDir, "git", "rev-parse", "refs/heads/"+unpushed)

	store := newPrepushStore(
		prepushIssue("gt-src-a", ""),
		prepushIssue("gt-src-b", ""),
		assertMRBead("gt-mr-pushed", pushed, "main", "gt-src-a", pushedHead),
		assertMRBead("gt-mr-unpushed", unpushed, "main", "gt-src-b", unpushedHead),
	)
	e := newPrepushEngineer(t, workDir, store)

	batch := []*MRInfo{
		{ID: "gt-mr-pushed", Branch: pushed, Target: "main", SourceIssue: "gt-src-a", CommitSHA: pushedHead},
		{ID: "gt-mr-unpushed", Branch: unpushed, Target: "main", SourceIssue: "gt-src-b", CommitSHA: unpushedHead},
	}

	stacked, dropped, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("one unstrackable member must not fail the batch: %v", err)
	}
	if ids := stackedIDs(stacked); len(ids) != 1 || ids[0] != "gt-mr-pushed" {
		t.Errorf("want only the pushed MR stacked, got %v (dropped %v)", ids, stackedIDs(dropped))
	}
	if ids := stackedIDs(dropped); len(ids) != 1 || ids[0] != "gt-mr-unpushed" {
		t.Errorf("want the unpushed MR reported as not stacked, got %v", ids)
	}
}
