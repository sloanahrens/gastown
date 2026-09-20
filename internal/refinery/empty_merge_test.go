package refinery

import (
	"context"
	"strings"
	"testing"
)

// emptyMergeMR returns a synthetic merge-mechanics MR for branch, which keeps
// doMerge off the beads path the same way the other doMerge tests do.
func emptyMergeMR(branch, commit string) *MRInfo {
	return &MRInfo{
		ID:        "mr-empty-merge",
		Branch:    branch,
		Target:    "main",
		CommitSHA: commit,
	}
}

// TestDoMergeRefusesEmptySubmission reproduces gt-j5cc: a branch that commits a
// payload and then a second commit that deletes it again ends with the target's
// own tree, so merging it lands nothing. Before the check, the gates ran
// against main's tree, passed, and the MR closed its source issue as merged.
func TestDoMergeRefusesEmptySubmission(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/empty"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "payload.txt", "one\ntwo\nthree\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: add payload")
	run(t, workDir, "git", "rm", "payload.txt")
	run(t, workDir, "git", "commit", "-m", "fix: resolve conflict by dropping the payload")
	// The commit that drops the payload is the branch head, and so is the
	// commit the refusal should name.
	commit := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "checkout", "main")
	mainHead := run(t, workDir, "git", "rev-parse", "main")

	e := newTestEngineer(t, workDir, g)
	result := e.doMerge(context.Background(), emptyMergeMR(branch, commit))

	if result.Success {
		t.Fatal("doMerge succeeded for a submission that changes nothing")
	}
	if !result.NoMerge {
		t.Errorf("NoMerge = false (error %q); an empty MR is ineligible, not a build failure", result.Error)
	}
	for _, want := range []string{
		"empty merge (before gates)",
		"identical trees",
		shortSHA(commit),
		"removes 3 lines and adds none",
	} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("refusal %q does not mention %q", result.Error, want)
		}
	}
	if got := run(t, workDir, "git", "rev-parse", "main"); got != mainHead {
		t.Errorf("main moved to %s, want it left at %s", got, mainHead)
	}
}

// TestDoMergeRefusesAlreadyLandedBranch covers the case the submitted head
// cannot answer: the branch's own tree differs from the target's, so it passes
// the pre-gate check, but the target already carries the same change and the
// merge result is the target's tree. That is an empty merge commit, which is
// how a superseded MR would otherwise land as merged.
func TestDoMergeRefusesAlreadyLandedBranch(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/landed"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "f.txt", "v2\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: set f to v2")
	commit := run(t, workDir, "git", "rev-parse", branch)

	// main reaches the same content by a different commit, and moves further
	// ahead, so the two trees differ.
	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "f.txt", "v2\n")
	writeFile(t, workDir, "unrelated.txt", "other work\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "fix: land f=v2 alongside other work")
	run(t, workDir, "git", "push", "origin", "main")
	mainHead := run(t, workDir, "git", "rev-parse", "main")

	e := newTestEngineer(t, workDir, g)
	result := e.doMerge(context.Background(), emptyMergeMR(branch, commit))

	if result.Success {
		t.Fatal("doMerge succeeded for a branch whose change main already carries")
	}
	if !result.NoMerge {
		t.Errorf("NoMerge = false (error %q); an empty merge is ineligible, not a build failure", result.Error)
	}
	if !strings.Contains(result.Error, "empty merge (before push)") {
		t.Errorf("refusal %q does not report the pre-push stage", result.Error)
	}
	if !strings.Contains(result.Error, "the merge result and origin/main have identical trees") {
		t.Errorf("refusal %q does not name what was compared", result.Error)
	}
	if got := run(t, workDir, "git", "rev-parse", "main"); got != mainHead {
		t.Errorf("main moved to %s, want the local merge reset back to %s", got, mainHead)
	}
	if got := run(t, workDir, "git", "rev-parse", "origin/main"); got != mainHead {
		t.Errorf("origin/main is %s, want it unpushed at %s", got, mainHead)
	}
}
