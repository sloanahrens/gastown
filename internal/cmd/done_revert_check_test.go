package cmd

import (
	"os"
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

// The four scenarios below cover gt-0wy03: the contained (line-multiset)
// half of DetectRevertedMerges has no positional or context anchor, so it
// cannot by itself tell an actual revert apart from a candidate that merely
// deletes one instance of a repeated line a merged commit also added
// elsewhere. Each scenario shares the same shape — a candidate fully rebased
// onto (so containing every one of) a target commit that adds a line — and
// differs only in whether, and why, that removal should or should not be
// read as an inversion of the merged commit.

// newRebasedPackageScenario builds origin.git with a base commit holding
// path with initial content, clones it into a seed (standing in for
// origin/main) and a polecat checkout already rebased onto the seed's
// current tip — the fast-forward shape the real incident described ("HEAD's
// parent IS origin/main"), as opposed to newRevertScenario's stale,
// never-rebased checkouts.
func newRebasedPackageScenario(t *testing.T, path, initial string) scenarioPaths {
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
	if err := os.MkdirAll(filepath.Join(seed, filepath.Dir(path)), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	writeTestFile(t, filepath.Join(seed, path), initial)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/zircon/gt-test")
	return scenarioPaths{seed: seed, polecat: polecat}
}

// rebasePolecatOntoSeed advances main past its current tip with a commit
// that writes path to advanced (the merged commit under test), pushes it,
// then fast-forwards the polecat checkout onto it — so the polecat's tree
// genuinely contains the merged change, the shape the contained test's
// relocation hatch assumes.
func rebasePolecatOntoSeed(t *testing.T, s scenarioPaths, path, advanced, subject string) {
	t.Helper()
	writeTestFile(t, filepath.Join(s.seed, path), advanced)
	runGitCmd(t, s.seed, "add", "-A")
	runGitCmd(t, s.seed, "commit", "-m", subject)
	runGitCmd(t, s.seed, "push", "origin", "main")
	runGitCmd(t, s.polecat, "fetch", "origin")
	runGitCmd(t, s.polecat, "rebase", "origin/main")
}

// TestDetectRevertedMerges_RelocationSurvivesInPackage is gt-0wy03's own
// repro: the merged commit adds t.Parallel() to TestA only (TestB's own
// t.Parallel() predates it, at the base commit). The polecat, now fully
// rebased onto that commit, removes TestB's — a call for an unrelated
// reason (gt-k317: it swaps a package global and cannot run in parallel) —
// leaving TestA's untouched. The line-multiset test alone reads this as
// undoing the merged commit, since it removed one instance of the exact
// line the commit added; the fix must see that the commit's own line
// survives, right where the commit put it, and not refuse.
func TestDetectRevertedMerges_RelocationSurvivesInPackage(t *testing.T) {
	t.Parallel()
	const path = "pkg/foo_test.go"
	base := "package pkg\n\n" +
		"func TestA(t *testing.T) {\n\tdoStuff()\n}\n\n" +
		"func TestB(t *testing.T) {\n\tt.Parallel()\n\tdoOtherStuff()\n}\n"
	advanced := "package pkg\n\n" +
		"func TestA(t *testing.T) {\n\tt.Parallel()\n\tdoStuff()\n}\n\n" +
		"func TestB(t *testing.T) {\n\tt.Parallel()\n\tdoOtherStuff()\n}\n"
	final := "package pkg\n\n" +
		"func TestA(t *testing.T) {\n\tt.Parallel()\n\tdoStuff()\n}\n\n" +
		"func TestB(t *testing.T) {\n\tdoOtherStuff()\n}\n"

	s := newRebasedPackageScenario(t, path, base)
	rebasePolecatOntoSeed(t, s, path, advanced, "cmd tests: t.Parallel for TestA (gt-kf0r)")
	commitPolecat(t, s.polecat, map[string]string{path: final}, "fix(pkg): make TestB sequential, it swaps a global (gt-test)")

	assertNoRevertedMerges(t, s.polecat)
}

// TestDetectRevertedMerges_RevertsTrailerDoesNotBypass covers a removal
// whose content genuinely does not survive anywhere in the package, with a
// commit message that claims, by a "Reverts: <issue-id>" trailer, that the
// removal is deliberate and reviewed. That claim is author-controlled text
// and must never bypass the guard on its own (gt-0wy03 attempt 2): the
// commit is still refused, exactly as an identical commit with no trailer
// would be.
func TestDetectRevertedMerges_RevertsTrailerDoesNotBypass(t *testing.T) {
	t.Parallel()
	const path = "pkg/bar.go"
	base := "package pkg\n\nfunc Bar() {\n\tdoA()\n}\n"
	advanced := "package pkg\n\nfunc Bar() {\n\tdoA()\n\tdoB() // guard\n}\n"
	final := "package pkg\n\nfunc Bar() {\n\tdoA()\n}\n"

	s := newRebasedPackageScenario(t, path, base)
	rebasePolecatOntoSeed(t, s, path, advanced, "pkg: add guard call (gt-uniq1)")
	writeTestFile(t, filepath.Join(s.polecat, path), final)
	runGitCmd(t, s.polecat, "add", "-A")
	runGitCmd(t, s.polecat, "commit", "-m",
		"pkg: drop guard call, redundant with caller check\n\n"+
			"Reverts: gt-uniq1 doB guard in Bar\n\n"+
			"The guard is redundant now that callers check first; verified in review.")

	found, err := git.DetectRevertedMerges(git.NewGit(s.polecat), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1 (a Reverts: trailer must not bypass the check): %+v", len(found), found)
	}
}

// TestDetectRevertedMerges_PreexistingCopyDoesNotRescue is gt-0wy03 attempt
// 2's own repro for the relocation escape hatch being too permissive: the
// merged commit adds t.Parallel() to TestA in pkg/foo_test.go, and an
// unrelated file in the same package, pkg/bar_test.go, has always had its
// own t.Parallel() in TestC — present since the base commit, never touched
// by either the merged commit or the candidate. The candidate removes
// TestA's t.Parallel() (reverting the merged commit's own change) while also
// adding an unrelated new test, so the exact whole-file test cannot fire and
// only the contained (line-multiset) test, with its relocation hatch, is in
// play. TestC's t.Parallel() still sits in the package at head, but it was
// never ADDED by this branch — it predates the branch entirely — so it must
// not rescue the removal. The old content-scanning check counted any current
// copy anywhere in the directory and would have wrongly rescued this.
func TestDetectRevertedMerges_PreexistingCopyDoesNotRescue(t *testing.T) {
	t.Parallel()
	const (
		fooPath = "pkg/foo_test.go"
		barPath = "pkg/bar_test.go"
	)
	fooBase := "package pkg\n\nfunc TestA(t *testing.T) {\n\tdoStuff()\n}\n"
	fooAdvanced := "package pkg\n\nfunc TestA(t *testing.T) {\n\tt.Parallel()\n\tdoStuff()\n}\n"
	fooFinal := "package pkg\n\nfunc TestA(t *testing.T) {\n\tdoStuff()\n}\n\n" +
		"func TestD(t *testing.T) {\n\tdoD()\n}\n"
	barContent := "package pkg\n\nfunc TestC(t *testing.T) {\n\tt.Parallel()\n\tdoOtherStuff()\n}\n"

	s := newRebasedPackageScenario(t, fooPath, fooBase)
	writeTestFile(t, filepath.Join(s.seed, barPath), barContent)
	runGitCmd(t, s.seed, "add", "-A")
	runGitCmd(t, s.seed, "commit", "-m", "pkg: add TestC (gt-test)")
	runGitCmd(t, s.seed, "push", "origin", "main")
	runGitCmd(t, s.polecat, "fetch", "origin")
	runGitCmd(t, s.polecat, "rebase", "origin/main")

	rebasePolecatOntoSeed(t, s, fooPath, fooAdvanced, "cmd tests: t.Parallel for TestA (gt-kf0r)")
	commitPolecat(t, s.polecat, map[string]string{fooPath: fooFinal}, "fix(pkg): TestA cannot run in parallel, it swaps a global; add TestD (gt-test)")

	found, err := git.DetectRevertedMerges(git.NewGit(s.polecat), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1 (a pre-existing copy in another file must not rescue the removal): %+v", len(found), found)
	}
}

// TestDetectRevertedMerges_LaterTargetAdditionDoesNotRescue is gt-0wy03
// attempt 3's own repro for the hole attempt 2 left open: commit X, already
// merged to main, adds t.Parallel() to TestA in pkg/foo_test.go. A SECOND,
// LATER commit Y — also already merged, after X — adds its own t.Parallel()
// to TestZ in a sibling file, pkg/bar_test.go, that the candidate never
// touches. The polecat rebases onto both X and Y, then genuinely reverts X's
// own line (removes TestA's t.Parallel(), for an unrelated reason) without
// adding anything anywhere in the package. Y is a LATER TARGET commit's own
// contribution, not this branch's, and must not rescue the removal: the old
// parent-anchored check summed every line gained anywhere in the package
// since just before X ran, which included Y's unrelated addition to a file
// the candidate never touched.
func TestDetectRevertedMerges_LaterTargetAdditionDoesNotRescue(t *testing.T) {
	t.Parallel()
	const (
		fooPath = "pkg/foo_test.go"
		barPath = "pkg/bar_test.go"
	)
	fooBase := "package pkg\n\nfunc TestA(t *testing.T) {\n\tdoStuff()\n}\n"
	fooAdvanced := "package pkg\n\nfunc TestA(t *testing.T) {\n\tt.Parallel()\n\tdoStuff()\n}\n"
	fooFinal := "package pkg\n\nfunc TestA(t *testing.T) {\n\tdoStuff()\n}\n"
	barContent := "package pkg\n\nfunc TestZ(t *testing.T) {\n\tt.Parallel()\n\tdoOtherStuff()\n}\n"

	s := newRebasedPackageScenario(t, fooPath, fooBase)
	rebasePolecatOntoSeed(t, s, fooPath, fooAdvanced, "cmd tests: t.Parallel for TestA (gt-kf0r)")

	writeTestFile(t, filepath.Join(s.seed, barPath), barContent)
	runGitCmd(t, s.seed, "add", "-A")
	runGitCmd(t, s.seed, "commit", "-m", "cmd tests: t.Parallel for TestZ (gt-kf0r)")
	runGitCmd(t, s.seed, "push", "origin", "main")
	runGitCmd(t, s.polecat, "fetch", "origin")
	runGitCmd(t, s.polecat, "rebase", "origin/main")

	commitPolecat(t, s.polecat, map[string]string{fooPath: fooFinal}, "fix(pkg): TestA cannot run in parallel, it swaps a global (gt-test)")

	found, err := git.DetectRevertedMerges(git.NewGit(s.polecat), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1 (a later target commit's addition in an untouched sibling file must not rescue the removal): %+v", len(found), found)
	}
}

// TestDetectRevertedMerges_RefusesUndocumentedFullRemoval is the baseline
// guard: the identical shape as the Reverts-trailer scenario above — content
// gone from the whole package, so relocation cannot rescue it — but with no
// commit-message trailer naming the merged commit either. This must still be
// refused: the fix must not make every single-line removal pass.
func TestDetectRevertedMerges_RefusesUndocumentedFullRemoval(t *testing.T) {
	t.Parallel()
	const path = "pkg/bar.go"
	base := "package pkg\n\nfunc Bar() {\n\tdoA()\n}\n"
	advanced := "package pkg\n\nfunc Bar() {\n\tdoA()\n\tdoB() // guard\n}\n"
	final := "package pkg\n\nfunc Bar() {\n\tdoA()\n}\n"

	s := newRebasedPackageScenario(t, path, base)
	rebasePolecatOntoSeed(t, s, path, advanced, "pkg: add guard call (gt-uniq1)")
	commitPolecat(t, s.polecat, map[string]string{path: final}, "pkg: drop guard call")

	found, err := git.DetectRevertedMerges(git.NewGit(s.polecat), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1 (undocumented removal must still be refused): %+v", len(found), found)
	}
}

// TestRequireNonPolecatCloneForRevertOverride pins gt-0wy03 AC1: --allow-reverts
// is refused whenever cwd resolves under a "*/polecats/*" path component, no
// matter what identity env vars claim. GT_ROLE=mayor is set here specifically
// because a bead field or env var is always caller-controlled — only the
// actual worktree path, which the caller cannot fake without really running
// the command from a non-polecat clone, may authorize the override.
func TestRequireNonPolecatCloneForRevertOverride(t *testing.T) {
	t.Run("refuses a polecat worktree even with GT_ROLE=mayor", func(t *testing.T) {
		t.Setenv("GT_ROLE", "mayor")
		dir := t.TempDir()
		polecatWorktree := filepath.Join(dir, "gastown", "polecats", "onyx", "gastown")
		if err := os.MkdirAll(polecatWorktree, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		err := requireNonPolecatCloneForRevertOverride(polecatWorktree)
		if err == nil {
			t.Fatal("expected refusal for --allow-reverts from a polecat worktree")
		}
		if !strings.Contains(err.Error(), "gt-0wy03") {
			t.Errorf("error should reference the guardrail bead, got: %v", err)
		}
		if !strings.Contains(err.Error(), "gt mq submit --allow-reverts") {
			t.Errorf("error should name the mayor-side override path, got: %v", err)
		}
	})

	t.Run("allows a non-polecat clone", func(t *testing.T) {
		dir := t.TempDir()
		mayorClone := filepath.Join(dir, "gastown", "mayor", "rig")
		if err := os.MkdirAll(mayorClone, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := requireNonPolecatCloneForRevertOverride(mayorClone); err != nil {
			t.Errorf("expected a non-polecat clone to pass, got: %v", err)
		}
	})
}
