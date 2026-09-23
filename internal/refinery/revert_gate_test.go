package refinery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// revertGateMR returns a synthetic MR for branch/commit targeting main, which
// keeps doMerge off the beads path the same way the other doMerge tests do.
func revertGateMR(id, branch, commit string) *MRInfo {
	return &MRInfo{ID: id, Branch: branch, Target: "main", CommitSHA: commit}
}

// setupRevertingBranch reproduces the gt-63sz shape: a branch cut from main,
// which then advances with a genuine commit (mergedCommit, adding
// merged.txt), followed by the stale-reset habit — `git reset --soft
// origin/main` over the old checkout — so the branch's own commit records
// (old tree) - (new tip), deleting merged.txt under a message describing
// unrelated work. Returns the branch's own commit SHA and the merged commit
// it undoes.
func setupRevertingBranch(t *testing.T, workDir, branch string) (commit, mergedCommit string) {
	t.Helper()
	writeFile(t, workDir, "keep.txt", "keep\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "chore: seed keep.txt")
	run(t, workDir, "git", "push", "origin", "main")

	run(t, workDir, "git", "checkout", "-b", branch, "main")
	run(t, workDir, "git", "checkout", "main")

	writeFile(t, workDir, "merged.txt", "merged\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: land merged work")
	run(t, workDir, "git", "push", "origin", "main")
	mergedCommit = run(t, workDir, "git", "rev-parse", "main")

	run(t, workDir, "git", "checkout", branch)
	run(t, workDir, "git", "reset", "--soft", "main")
	writeFile(t, workDir, "fix.txt", "the fix\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: fix the actual bug")
	commit = run(t, workDir, "git", "rev-parse", branch)
	pushBranch(t, workDir, branch)

	run(t, workDir, "git", "checkout", "main")
	return commit, mergedCommit
}

// writeAllowRevertsOverride writes a mayor-authored override file at the path
// checkRevertGate reads, for use by tests standing in for the mayor.
func writeAllowRevertsOverride(t *testing.T, townRoot, branch, reason string, shas ...string) {
	t.Helper()
	dir := filepath.Join(townRoot, allowRevertsOverrideDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	content := reason + "\n" + strings.Join(shas, "\n") + "\n"
	path := filepath.Join(dir, sanitizeOverrideBranchName(branch))
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestDoMergeRefusesRevertedMerge is the mayor's first acceptance test: a
// reverting branch is refused at the gate, with no override present.
func TestDoMergeRefusesRevertedMerge(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/revert-refused"
	commit, mergedCommit := setupRevertingBranch(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	result := e.doMerge(context.Background(), revertGateMR("mr-revert-refused", branch, commit))

	if result.Success {
		t.Fatal("doMerge succeeded for a branch that reverts merged work")
	}
	if !strings.Contains(result.Error, "undoes work already merged") {
		t.Errorf("refusal %q does not describe the revert", result.Error)
	}
	if !strings.Contains(result.Error, shortSHA(mergedCommit)) {
		t.Errorf("refusal %q does not name the reverted commit %s", result.Error, shortSHA(mergedCommit))
	}
	if !strings.Contains(result.Error, "escalate to the mayor") {
		t.Errorf("refusal %q does not name the mayor escalation path", result.Error)
	}
}

// TestDoMergeAllowsRevertWithMatchingOverride is the mayor's second
// acceptance test: the override file for the exact branch, naming the
// reverted commit, lets the merge through and records a note.
func TestDoMergeAllowsRevertWithMatchingOverride(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/revert-override"
	commit, mergedCommit := setupRevertingBranch(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	buf := &bytes.Buffer{}
	e.output = buf
	townRoot := filepath.Dir(e.rig.Path)
	writeAllowRevertsOverride(t, townRoot, branch, "mayor: verified this is an intentional cleanup", mergedCommit)

	result := e.doMerge(context.Background(), revertGateMR("mr-revert-override", branch, commit))
	if !result.Success {
		t.Fatalf("doMerge refused a branch with a matching allow-reverts override: %s", result.Error)
	}
	if !strings.Contains(buf.String(), "allow-reverts override applied") {
		t.Errorf("output does not record that the override was applied:\n%s", buf.String())
	}
}

// TestDoMergeOverrideForOtherBranchDoesNotApply is the mayor's third
// acceptance test: an override written for a different branch does not
// rescue this one, even though it names the same reverted commit.
func TestDoMergeOverrideForOtherBranchDoesNotApply(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/revert-wrong-branch"
	commit, mergedCommit := setupRevertingBranch(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	townRoot := filepath.Dir(e.rig.Path)
	writeAllowRevertsOverride(t, townRoot, "polecat/other/unrelated-branch", "mayor: unrelated override", mergedCommit)

	result := e.doMerge(context.Background(), revertGateMR("mr-revert-wrong-branch", branch, commit))
	if result.Success {
		t.Fatal("doMerge succeeded using another branch's allow-reverts override")
	}
}

// TestDoMergeRefusesBoilerplateRevert is the mayor's fourth acceptance test:
// with the relocation hatch gone entirely, a real revert of a boilerplate
// line stays refused even though the candidate's own diff happens to add a
// copy of the same line text elsewhere — the exact shape every earlier
// attempt's relocation hatch was fooled by.
func TestDoMergeRefusesBoilerplateRevert(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	seed := "package thing\n\nfunc TestA(t *T) {\n\tt.Run()\n}\n\nfunc TestB(t *T) {\n\tt.Run()\n\tt.Parallel()\n}\n"
	writeFile(t, workDir, "thing_test.go", seed)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "chore: seed test file")
	run(t, workDir, "git", "push", "origin", "main")

	branch := "polecat/test/boilerplate"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	run(t, workDir, "git", "checkout", "main")

	// The merged commit adds the repeated boilerplate line to TestA.
	merged := "package thing\n\nfunc TestA(t *T) {\n\tt.Run()\n\tt.Parallel()\n}\n\nfunc TestB(t *T) {\n\tt.Run()\n\tt.Parallel()\n}\n"
	writeFile(t, workDir, "thing_test.go", merged)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "test: t.Parallel for TestA (gt-kf0r)")
	run(t, workDir, "git", "push", "origin", "main")
	mergedCommit := run(t, workDir, "git", "rev-parse", "main")

	// The candidate, cut before that commit, removes the pre-existing
	// boilerplate line from TestB for an unrelated, legitimate reason — its
	// own diff shows "-t.Parallel()", the exact line the merged commit added
	// elsewhere in the same file, which a relocation hatch would read as
	// "the removed line survived, so this isn't a real revert".
	run(t, workDir, "git", "checkout", branch)
	candidate := "package thing\n\nfunc TestA(t *T) {\n\tt.Run()\n}\n\nfunc TestB(t *T) {\n\tt.Run()\n}\n"
	writeFile(t, workDir, "thing_test.go", candidate)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "fix: TestB can no longer run in parallel")
	commit := run(t, workDir, "git", "rev-parse", branch)
	pushBranch(t, workDir, branch)

	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	result := e.doMerge(context.Background(), revertGateMR("mr-boilerplate", branch, commit))

	if result.Success {
		t.Fatal("doMerge succeeded for a branch that reverts a merged boilerplate line")
	}
	if !strings.Contains(result.Error, shortSHA(mergedCommit)) {
		t.Errorf("refusal %q does not name the reverted commit %s", result.Error, shortSHA(mergedCommit))
	}
}
