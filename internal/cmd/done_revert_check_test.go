package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// The scenarios below reproduce gt-63sz against real repositories. The shape
// under test is a worktree that was cut at B0 and stayed there while the target
// advanced, followed by
//
//	git add -A; git reset --soft origin/main; git commit
//
// which moves HEAD to the current target tip while the index and working tree
// stay as B0 left them — so the commit encodes (B0 tree) - (tip), a revert of
// everything merged in between, under a message describing the intended work.
// Two polecat MRs landed that way in one night (gt-wisp-hrau, gt-wisp-p7nl).
//
// Every scenario also has a legit twin that ends in the same file states
// WITHOUT the reset. Those are the false-positive controls: the fix must
// distinguish the tree content from how the content got there, since ancestry
// is identical-adjacent in both.

// scenarioPaths locates the two working copies a scenario needs.
type scenarioPaths struct {
	seed    string // stands in for the shared origin/main branch
	polecat string // the polecat worktree, on polecat/zircon/gt-test
}

// newRevertScenario builds origin.git with a base commit, clones it into a
// seed checkout (main) and a polecat checkout, and leaves the polecat on its
// work branch. Files at the base commit: shared.txt ("base") and keep.txt.
func newRevertScenario(t *testing.T) scenarioPaths {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	runGitCmd(t, "", "init", "--bare", remote)
	// Point the bare repo's HEAD at main explicitly: git init's default branch
	// name is host-configurable, and the polecat clone below checks out
	// whatever HEAD names.
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFile(t, filepath.Join(seed, "shared.txt"), "base\n")
	writeTestFile(t, filepath.Join(seed, "keep.txt"), "keep\n")
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	// The polecat's checkout is cut HERE, at the base commit, and stays here
	// while main advances below.
	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/zircon/gt-test")
	return scenarioPaths{seed: seed, polecat: polecat}
}

// advanceMain merges three changes into main that a polecat still sitting at
// the base commit has never seen: a brand-new file, a change to the file the
// polecat is editing, and a change to a file the polecat never touches.
func advanceMain(t *testing.T, seed string) {
	t.Helper()
	writeTestFile(t, filepath.Join(seed, "merged.txt"), "merged\n")
	writeTestFile(t, filepath.Join(seed, "shared.txt"), "base\nmain line\n")
	writeTestFile(t, filepath.Join(seed, "keep.txt"), "keep\nmain touch\n")
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "merged: other work")
	runGitCmd(t, seed, "push", "origin", "main")
}

// commitPolecat writes edits relative to the polecat worktree and commits them.
func commitPolecat(t *testing.T, polecat string, edits map[string]string, message string) {
	t.Helper()
	for name, content := range edits {
		writeTestFile(t, filepath.Join(polecat, name), content)
	}
	runGitCmd(t, polecat, "add", "-A")
	runGitCmd(t, polecat, "commit", "-m", message)
}

// staleResetOntoMain is the habit under test: fetch, then squash the old tree
// onto the fresh tip without ever taking the fresh tree's content.
func staleResetOntoMain(t *testing.T, polecat string) {
	t.Helper()
	runGitCmd(t, polecat, "fetch", "origin")
	runGitCmd(t, polecat, "add", "-A")
	runGitCmd(t, polecat, "reset", "--soft", "origin/main")
	runGitCmd(t, polecat, "commit", "-m", "feat: fix the actual bug (gt-test)")
}

// TestDetectRevertedMerges_StaleResetOverFreshBase is the gt-63sz incident in
// miniature. The polecat's branch is exactly one commit ahead of a current
// origin/main with a current merge-base, so every ancestry-based check in gt
// done passes it; only the content check may refuse it.
func TestDetectRevertedMerges_StaleResetOverFreshBase(t *testing.T) {
	t.Parallel()
	s := newRevertScenario(t)
	commitPolecat(t, s.polecat, map[string]string{
		"shared.txt": "base\npolecat line\n",
		"fix.txt":    "the fix\n",
	}, "wip: start")
	advanceMain(t, s.seed)
	staleResetOntoMain(t, s.polecat)

	// Preconditions: the branch is exactly what the incident described — one
	// commit, merge-base equal to the fresh tip. If either stops holding, the
	// scenario is no longer exercising the mechanism.
	g := git.NewGit(s.polecat)
	mergeBase, err := g.MergeBase("origin/main", "HEAD")
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	tip, err := g.Rev("origin/main")
	if err != nil {
		t.Fatalf("rev origin/main: %v", err)
	}
	if mergeBase != tip {
		t.Fatalf("scenario precondition: merge-base %s != origin/main tip %s (the branch is no longer based on a fresh tip)", mergeBase, tip)
	}
	ahead, err := g.CommitsAhead("origin/main", "HEAD")
	if err != nil {
		t.Fatalf("commits ahead: %v", err)
	}
	if ahead != 1 {
		t.Fatalf("scenario precondition: %d commits ahead of origin/main, want 1", ahead)
	}

	found, err := git.DetectRevertedMerges(g, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(found), found)
	}
	wantCommit, err := g.Rev("origin/main")
	if err != nil {
		t.Fatalf("rev: %v", err)
	}
	if found[0].Commit != wantCommit {
		t.Errorf("reverted commit = %s, want the origin/main tip %s", found[0].Commit, wantCommit)
	}
	// All three paths of the merged commit must be reported, not just the ones
	// the blob-equality test can see. shared.txt is the hard case: the polecat
	// edited it as well as reverted main's change to it, so no blob comparison
	// separates them — only the line-level inversion does.
	for _, want := range []string{"merged.txt", "keep.txt", "shared.txt"} {
		if !containsString(found[0].Paths, want) {
			t.Errorf("reverted paths %v missing %s", found[0].Paths, want)
		}
	}
}

// TestDetectRevertedMerges_LegitimateBranches covers the false-positive
// controls: the same repositories, the same merged work, and a polecat whose
// tree is NOT stale. None may be refused — every one of these is ordinary work
// that the merge would land correctly.
func TestDetectRevertedMerges_LegitimateBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edits map[string]string
	}{
		{
			name:  "edits an unrelated file",
			edits: map[string]string{"fix.txt": "the fix\n"},
		},
		{
			// The polecat edits the very file main also changed. Its blob
			// differs from main's from every direction, so a naive "branch
			// content != main content" rule would fire here.
			name:  "edits the same file main changed",
			edits: map[string]string{"shared.txt": "base\npolecat line\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newRevertScenario(t)
			commitPolecat(t, s.polecat, tt.edits, "feat: real work (gt-test)")
			advanceMain(t, s.seed)
			runGitCmd(t, s.polecat, "fetch", "origin")

			assertNoRevertedMerges(t, s.polecat)
		})
	}

	// Rebased before working: the branch's base is the advanced tip, so the
	// merge base is current and the polecat's commits were replayed onto it.
	t.Run("rebased onto main before working", func(t *testing.T) {
		s := newRevertScenario(t)
		advanceMain(t, s.seed)
		runGitCmd(t, s.polecat, "fetch", "origin")
		runGitCmd(t, s.polecat, "rebase", "origin/main")
		commitPolecat(t, s.polecat, map[string]string{"shared.txt": "base\nmain line\npolecat line\n"}, "feat: real work (gt-test)")

		assertNoRevertedMerges(t, s.polecat)
	})
}

func assertNoRevertedMerges(t *testing.T, repo string) {
	t.Helper()
	found, err := git.DetectRevertedMerges(git.NewGit(repo), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("detectRevertedMerges refused legitimate work: %+v", found)
	}
}

// newShallowBoundaryScenario builds the gt-zeuip shape: origin/main holds
// victim.txt unchanged since its first commit, and the polecat's clone is cut
// at a shallow boundary, so git has no parent tree for the boundary commit and
// prints the boundary's ENTIRE tree as creations — naming victim.txt as content
// that commit introduced.
//
// Real clones of the gastown repo are cut this way, which is what turned the
// deletion below into a refusal: the fabricated entry's pre-image is the empty
// blob, so a deletion matched it exactly (gt-zeuip).
func newShallowBoundaryScenario(t *testing.T) scenarioPaths {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFile(t, filepath.Join(seed, "victim.txt"), "victim\n")
	writeTestFile(t, filepath.Join(seed, "other.txt"), "other\n")
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "add victim and other")
	// A second commit puts victim.txt's creation one commit below the tip, so
	// the shallow clone keeps the file without keeping the commit that made it.
	writeTestFile(t, filepath.Join(seed, "other.txt"), "other\nsecond\n")
	runGitCmd(t, seed, "commit", "-am", "touch other")
	runGitCmd(t, seed, "push", "origin", "main")

	// Local clones ignore --depth without a file:// source.
	runGitCmd(t, "", "clone", "--depth", "1", "file://"+remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/zircon/gt-test")
	return scenarioPaths{seed: seed, polecat: polecat}
}

// requireGraftBoundary asserts the clone is cut the way the incident needs: git
// reports origin/main as parentless, and still prints that commit's whole tree
// as additions. Either one failing means this scenario no longer reproduces the
// fabrication, and the test below would pass without testing anything.
func requireGraftBoundary(t *testing.T, repo string) {
	t.Helper()
	if parents := gitOutput(t, repo, "log", "-1", "--format=%P", "origin/main"); parents != "" {
		t.Fatalf("scenario precondition: origin/main has parents %q, want a graft boundary", parents)
	}
	raw := gitOutput(t, repo, "log", "--raw", "--no-abbrev", "--no-renames", "-1", "origin/main")
	if !strings.Contains(raw, "A\tvictim.txt") {
		t.Fatalf("scenario precondition: git no longer reports the boundary's tree as additions:\n%s", raw)
	}
}

// TestDetectRevertedMerges_ShallowBoundaryDeletionIsNoRevert is the gt-zeuip
// false positive in miniature. The polecat deletes a file main has held
// unchanged since before the clone was cut — a deletion of the polecat's own
// work, against a path main's history never touched after that. The boundary
// commit's fabricated creation entry names that file with an empty pre-image,
// which the deletion matched, so the check refused the submission and named a
// commit that never created anything.
func TestDetectRevertedMerges_ShallowBoundaryDeletionIsNoRevert(t *testing.T) {
	t.Parallel()
	s := newShallowBoundaryScenario(t)
	requireGraftBoundary(t, s.polecat)

	runGitCmd(t, s.polecat, "rm", "victim.txt")
	runGitCmd(t, s.polecat, "commit", "-m", "feat: retire victim.txt (gt-test)")

	found, err := git.DetectRevertedMerges(git.NewGit(s.polecat), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("detectRevertedMerges refused a deletion of a path main never changed after the clone was cut: %+v", found)
	}
}

// TestDetectRevertedMerges_DeletingAFileMainDidAddRefuses is the fail-closed
// control for the fix: the same grafted clone, but main adds live.txt AFTER the
// clone was cut, so the creating commit's parent IS in the clone and git
// reports a creation it actually saw. Deleting that file still refuses.
func TestDetectRevertedMerges_DeletingAFileMainDidAddRefuses(t *testing.T) {
	t.Parallel()
	s := newShallowBoundaryScenario(t)
	requireGraftBoundary(t, s.polecat)

	writeTestFile(t, filepath.Join(s.seed, "live.txt"), "live\n")
	runGitCmd(t, s.seed, "add", "-A")
	runGitCmd(t, s.seed, "commit", "-m", "add live.txt")
	runGitCmd(t, s.seed, "push", "origin", "main")

	// Take main's tip, then delete what main just added.
	runGitCmd(t, s.polecat, "fetch", "origin")
	runGitCmd(t, s.polecat, "merge", "--ff-only", "origin/main")
	runGitCmd(t, s.polecat, "rm", "live.txt")
	runGitCmd(t, s.polecat, "commit", "-m", "feat: retire live.txt (gt-test)")

	g := git.NewGit(s.polecat)
	wantCommit, err := g.Rev("origin/main")
	if err != nil {
		t.Fatalf("rev origin/main: %v", err)
	}
	found, err := git.DetectRevertedMerges(g, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want the one that added live.txt: %+v", len(found), found)
	}
	if found[0].Commit != wantCommit {
		t.Errorf("reverted commit = %s, want %s (the commit that added live.txt)", found[0].Commit, wantCommit)
	}
	if !containsString(found[0].Paths, "live.txt") {
		t.Errorf("reverted paths %v missing live.txt", found[0].Paths)
	}
}

// TestDetectRevertedMerges_UnresolvableTargetFailsClosed pins the failure mode:
// when the comparison cannot be made, the caller must get an error, because an
// empty result and a failed check are indistinguishable to every caller
// downstream — and this check exists precisely because that silence is how
// reverted work reached the merge queue.
func TestDetectRevertedMerges_UnresolvableTargetFailsClosed(t *testing.T) {
	t.Parallel()
	s := newRevertScenario(t)
	commitPolecat(t, s.polecat, map[string]string{"fix.txt": "the fix\n"}, "feat: work (gt-test)")

	g := git.NewGit(s.polecat)
	if _, err := git.DetectRevertedMerges(g, "origin/does-not-exist", "HEAD"); err == nil {
		t.Fatal("detectRevertedMerges returned no error for an unresolvable target, want failure")
	}
}

// TestReportRevertedMerges_RefusesStaleBranch checks the operator-facing half:
// a refusal, with the undone commit named and the rebase remedy stated, and the
// branch's diff stat printed so the polecat can see whose files are in its diff.
func TestReportRevertedMerges_RefusesStaleBranch(t *testing.T) {
	t.Parallel()
	s := newRevertScenario(t)
	commitPolecat(t, s.polecat, map[string]string{
		"shared.txt": "base\npolecat line\n",
		"fix.txt":    "the fix\n",
	}, "wip: start")
	advanceMain(t, s.seed)
	staleResetOntoMain(t, s.polecat)

	g := git.NewGit(s.polecat)
	err := reportRevertedMerges(g, "origin/main")
	if err == nil {
		t.Fatal("reportRevertedMerges accepted a branch that reverts merged work")
	}
	msg := err.Error()
	for _, want := range []string{"refusing to submit", "merged: other work", "git rebase origin/main", "git diff --stat origin/main...HEAD"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "--allow-reverts") {
		t.Errorf("refusal message advertises the override flag — agents read refusal text and self-bypass:\n%s", msg)
	}

	// A clean branch must be accepted, so the check cannot be passing by
	// refusing everything.
	clean := newRevertScenario(t)
	commitPolecat(t, clean.polecat, map[string]string{"fix.txt": "the fix\n"}, "feat: work (gt-test)")
	if err := reportRevertedMerges(git.NewGit(clean.polecat), "origin/main"); err != nil {
		t.Errorf("reportRevertedMerges refused a clean branch: %v", err)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// gitOutput returns git's trimmed stdout, for preconditions that ask git's own
// view of a repository rather than a git.Git method's.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}
