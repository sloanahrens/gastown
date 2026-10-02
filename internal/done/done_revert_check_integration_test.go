//go:build integration

package done

import (
	"os"
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
	p := cachedGitFixtureStrings(t, "newRevertScenario", func(dir string) []string {
		sp := buildNewRevertScenario(t, dir)
		return []string{sp.seed, sp.polecat}
	})
	return scenarioPaths{seed: p[0], polecat: p[1]}
}

// buildNewRevertScenario makes newRevertScenario's repos under dir.
func buildNewRevertScenario(t *testing.T, dir string) scenarioPaths {
	t.Helper()
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
func TestIntegrationDetectRevertedMerges_StaleResetOverFreshBase(t *testing.T) {
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

	report, err := git.DetectRevertedMerges(g, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report)
	}
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges reported %d relocated commits on a stale tree: %+v", len(report.Relocated), report.Relocated)
	}
	wantCommit, err := g.Rev("origin/main")
	if err != nil {
		t.Fatalf("rev: %v", err)
	}
	if report.Reverted[0].Commit != wantCommit {
		t.Errorf("reverted commit = %s, want the origin/main tip %s", report.Reverted[0].Commit, wantCommit)
	}
	// All three paths of the merged commit must be reported, not just the ones
	// the blob-equality test can see. shared.txt is the hard case: the polecat
	// edited it as well as reverted main's change to it, so no blob comparison
	// separates them — only the line-level inversion does.
	for _, want := range []string{"merged.txt", "keep.txt", "shared.txt"} {
		if !containsString(report.Reverted[0].Paths, want) {
			t.Errorf("reverted paths %v missing %s", report.Reverted[0].Paths, want)
		}
	}
}

// TestDetectRevertedMerges_LegitimateBranches covers the false-positive
// controls: the same repositories, the same merged work, and a polecat whose
// tree is NOT stale. None may be refused — every one of these is ordinary work
// that the merge would land correctly.
func TestIntegrationDetectRevertedMerges_LegitimateBranches(t *testing.T) {
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
			t.Parallel()
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
		t.Parallel()
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
	report := detectRevertedMerges(t, repo)
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges refused legitimate work: %+v", report)
	}
}

// detectRevertedMerges runs the check the way gt done does: against
// origin/main, on the branch as it stands.
func detectRevertedMerges(t *testing.T, repo string) git.RevertReport {
	t.Helper()
	report, err := git.DetectRevertedMerges(git.NewGit(repo), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	return report
}

// TestDetectRevertedMerges_MovingAPureAdditionIsNoRevert is the gt-tlw9u false
// positive. Main merged a commit that only ADDED lines (its numstat is "N 0"),
// and the polecat's branch relocates those lines within the file. Every line the
// commit added shows up as removed in the branch's diff — and re-added, since it
// was moved, not deleted — so a check that counts removals without netting them
// against additions calls the move a revert. Nothing is undone: the file still
// carries every line main merged.
func TestIntegrationDetectRevertedMerges_MovingAPureAdditionIsNoRevert(t *testing.T) {
	t.Parallel()
	s := newRevertScenario(t)

	// The moved block (A, B) is shorter than the block it moves past (x, y, z),
	// so git's diff keeps x/y/z in place and reports A/B as removed and re-added.
	writeTestFile(t, filepath.Join(s.seed, "shared.txt"), "base\nx\ny\nz\n")
	runGitCmd(t, s.seed, "add", "-A")
	runGitCmd(t, s.seed, "commit", "-m", "add x y z")
	writeTestFile(t, filepath.Join(s.seed, "shared.txt"), "A\nB\nbase\nx\ny\nz\n")
	runGitCmd(t, s.seed, "add", "-A")
	runGitCmd(t, s.seed, "commit", "-m", "add A B (pure addition)")
	runGitCmd(t, s.seed, "push", "origin", "main")

	runGitCmd(t, s.polecat, "fetch", "origin")
	runGitCmd(t, s.polecat, "merge", "--ff-only", "origin/main")
	commitPolecat(t, s.polecat, map[string]string{"shared.txt": "base\nx\ny\nz\nA\nB\n"}, "refactor: move A B (gt-test)")

	assertNoRevertedMerges(t, s.polecat)
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
func TestIntegrationDetectRevertedMerges_ShallowBoundaryDeletionIsNoRevert(t *testing.T) {
	t.Parallel()
	s := newShallowBoundaryScenario(t)
	requireGraftBoundary(t, s.polecat)

	runGitCmd(t, s.polecat, "rm", "victim.txt")
	runGitCmd(t, s.polecat, "commit", "-m", "feat: retire victim.txt (gt-test)")

	report, err := git.DetectRevertedMerges(git.NewGit(s.polecat), "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges refused a deletion of a path main never changed after the clone was cut: %+v", report)
	}
}

// TestDetectRevertedMerges_DeletingAFileMainDidAddRefuses is the fail-closed
// control for the fix: the same grafted clone, but main adds live.txt AFTER the
// clone was cut, so the creating commit's parent IS in the clone and git
// reports a creation it actually saw. Deleting that file still refuses.
func TestIntegrationDetectRevertedMerges_DeletingAFileMainDidAddRefuses(t *testing.T) {
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
	report, err := git.DetectRevertedMerges(g, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("detectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want the one that added live.txt: %+v", len(report.Reverted), report)
	}
	if report.Reverted[0].Commit != wantCommit {
		t.Errorf("reverted commit = %s, want %s (the commit that added live.txt)", report.Reverted[0].Commit, wantCommit)
	}
	if !containsString(report.Reverted[0].Paths, "live.txt") {
		t.Errorf("reverted paths %v missing live.txt", report.Reverted[0].Paths)
	}
}

// TestDetectRevertedMerges_UnresolvableTargetFailsClosed pins the failure mode:
// when the comparison cannot be made, the caller must get an error, because an
// empty result and a failed check are indistinguishable to every caller
// downstream — and this check exists precisely because that silence is how
// reverted work reached the merge queue.
func TestIntegrationDetectRevertedMerges_UnresolvableTargetFailsClosed(t *testing.T) {
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
func TestIntegrationReportRevertedMerges_RefusesStaleBranch(t *testing.T) {
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

// The constants below build the gt-x748o shape: a Go package whose file gains
// a comment block and one code line on main, and a polecat checkout cut AFTER
// that commit. The branch under test is therefore not stale, and the only
// thing that may explain a revert observation is where the content went —
// within the package, or out of it and into another (gt-bbk1f).
const (
	pkgAdditiveSubject = "add the summary line to the reject record"
	pkgRefactorSubject = "refactor: extract rejectionRequest (gt-test)"

	// pkgFooBase predates main's additive commit, and is also what a polecat
	// tree holds when the block was taken out of the file entirely.
	pkgFooBase = `package pkg

type request struct {
	ID            string
	FailureType   string
	ErrorMsg      string
	AttemptNumber int
	Summary       string
}

func Reject(reason string) request {
	return request{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
	}
}
`

	// pkgFooWithSummaryLiteral is main's additive commit: a comment and one
	// code line inside an existing struct literal, the shape bfbba004 added and
	// gt-s4f6's refactor relocated (gt-x748o).
	pkgFooWithSummaryLiteral = `package pkg

type request struct {
	ID            string
	FailureType   string
	ErrorMsg      string
	AttemptNumber int
	Summary       string
}

func Reject(reason string) request {
	return request{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		// A manual reject carries no verdict to build a Receipt from, so the
		// deacon falls back to the plain attempt-count redispatch.
		Summary: reason,
	}
}
`

	// pkgFooWithHelper relocates that literal into a helper in the same file,
	// re-wrapping the comment and letting gofmt realign the moved key — both of
	// which rewrite the lines at their new position without changing behavior.
	pkgFooWithHelper = `package pkg

type request struct {
	ID            string
	FailureType   string
	ErrorMsg      string
	AttemptNumber int
	Summary       string
}

// rejectionRequest builds the record a rejection is remembered by.
func rejectionRequest(reason string) request {
	// The reason doubles as the summary line, so a manual reject still records
	// one and the deacon can name the attempt.
	return request{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		Summary:       reason,
	}
}

func Reject(reason string) request {
	return rejectionRequest(reason)
}
`

	// pkgSiblingHelper is the same relocation one file over: the package keeps
	// the code, the path it was observed on does not.
	pkgSiblingHelper = `package pkg

// rejectionRequest builds the record a rejection is remembered by.
func rejectionRequest(reason string) request {
	// The reason doubles as the summary line, so a manual reject still records
	// one and the deacon can name the attempt.
	return request{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		Summary:       reason,
	}
}
`

	// pkgOtherPackageHelper is the same code landing in a DIFFERENT package,
	// where the type it builds is named as that package sees it and a comment
	// joins it: the shape a move across a package boundary leaves (gt-bbk1f).
	pkgOtherPackageHelper = `package other

// rejectionRequest builds the record a rejection is remembered by.
func rejectionRequest(reason string) pkgRequest {
	// The reason doubles as the summary line, so a manual reject still records
	// one and the deacon can name the attempt.
	return pkgRequest{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		Summary:       reason,
	}
}
`

	// pkgFooWithVerdictLiteral is main's additive commit when the change is a
	// block: three code lines in the literal, enough for a move to another
	// package to be read as one (gt-s5eou).
	pkgFooWithVerdictLiteral = `package pkg

type request struct {
	ID            string
	FailureType   string
	ErrorMsg      string
	AttemptNumber int
	Summary       string
}

func Reject(reason string) request {
	return request{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		// A manual reject carries no verdict to build a Receipt from, so the
		// deacon falls back to the plain attempt-count redispatch.
		Summary:   reason,
		Verdict:   verdictFor(reason),
		Retryable: !isFatal(reason),
	}
}
`

	// pkgOtherVerdictHelper is that block in a DIFFERENT package, the type it
	// builds named as that package sees it.
	pkgOtherVerdictHelper = `package other

// rejectionRequest builds the record a rejection is remembered by.
func rejectionRequest(reason string) pkgRequest {
	return pkgRequest{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		Summary:       reason,
		Verdict:       verdictFor(reason),
		Retryable:     !isFatal(reason),
	}
}
`

	// pkgOtherBase is the sibling package's unchanged content.
	pkgOtherBase = `package other

type pkgRequest struct {
	ID string
}
`

	// pkgFooWithCommentBlock is main's additive commit when it adds comments
	// and nothing else — the case the relocation test must not excuse, because
	// comments are excluded from its evidence.
	pkgFooWithCommentBlock = `package pkg

type request struct {
	ID            string
	FailureType   string
	ErrorMsg      string
	AttemptNumber int
	Summary       string
}

func Reject(reason string) request {
	return request{
		ID:            "mr-1",
		FailureType:   "editorial",
		ErrorMsg:      reason,
		AttemptNumber: 1,
		// A manual reject carries no verdict to build a Receipt from, so the
		// deacon falls back to the plain attempt-count redispatch.
	}
}
`
)

// newPackageRelocationScenario builds the gt-x748o repositories around one
// additive commit on main. key names additiveFoo: two builds under one key
// would hand out whichever ran first.
func newPackageRelocationScenario(t *testing.T, key, additiveFoo string) scenarioPaths {
	t.Helper()
	p := cachedGitFixtureStrings(t, "packageRelocation/"+key, func(dir string) []string {
		sp := buildPackageRelocationScenario(t, dir, additiveFoo)
		return []string{sp.seed, sp.polecat}
	})
	return scenarioPaths{seed: p[0], polecat: p[1]}
}

func buildPackageRelocationScenario(t *testing.T, dir, additiveFoo string) scenarioPaths {
	t.Helper()
	return buildPackageScenarioFrom(t, dir, pkgFooBase, additiveFoo)
}

// buildPackageScenarioFrom is the scenario with foo.go's base content named, for
// the changes whose before-state is not pkgFooBase.
func buildPackageScenarioFrom(t *testing.T, dir, baseFoo, additiveFoo string) scenarioPaths {
	t.Helper()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFileAt(t, filepath.Join(seed, "internal/pkg/foo.go"), baseFoo)
	writeTestFileAt(t, filepath.Join(seed, "internal/other/bar.go"), pkgOtherBase)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	// main's additive commit, and the point the polecat's checkout is cut at:
	// the branch under test descends from it, so nothing about it is stale.
	writeTestFileAt(t, filepath.Join(seed, "internal/pkg/foo.go"), additiveFoo)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", pkgAdditiveSubject)
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/zircon/gt-test")
	return scenarioPaths{seed: seed, polecat: polecat}
}

// TestDetectRevertedMerges_RelocatedBlockIsNotARevert is the gt-x748o incident:
// the polecat moves a merged change's code into a helper, re-indenting it and
// rewriting the comment around it. The behavior is intact, so the branch must
// be reported as a relocation and not refused.
func TestIntegrationDetectRevertedMerges_RelocatedBlockIsNotARevert(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edits map[string]string
	}{
		{
			name:  "into a helper in the same file",
			edits: map[string]string{"internal/pkg/foo.go": pkgFooWithHelper},
		},
		{
			name: "into a sibling file of the same package",
			edits: map[string]string{
				"internal/pkg/foo.go":    pkgFooBase,
				"internal/pkg/helper.go": pkgSiblingHelper,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newPackageRelocationScenario(t, "summary-literal", pkgFooWithSummaryLiteral)
			commitPolecat(t, s.polecat, tt.edits, pkgRefactorSubject)

			report := detectRevertedMerges(t, s.polecat)
			if len(report.Reverted) != 0 {
				t.Fatalf("detectRevertedMerges refused a relocation: %+v", report.Reverted)
			}
			if len(report.Relocated) != 1 {
				t.Fatalf("detectRevertedMerges reported %d relocations, want 1: %+v", len(report.Relocated), report.Relocated)
			}
			wantCommit, err := git.NewGit(s.polecat).Rev("origin/main")
			if err != nil {
				t.Fatalf("rev origin/main: %v", err)
			}
			if report.Relocated[0].Commit != wantCommit {
				t.Errorf("relocated commit = %s, want main's additive commit %s", report.Relocated[0].Commit, wantCommit)
			}
			if !containsString(report.Relocated[0].Paths, "internal/pkg/foo.go") {
				t.Errorf("relocated paths %v missing internal/pkg/foo.go", report.Relocated[0].Paths)
			}
		})
	}
}

// TestDetectRevertedMerges_CodeMovedToAnotherPackageIsNoRevert is the gt-bbk1f
// incident: the polecat carries a merged change's code out of a file that keeps
// existing and into another package — the thin cobra slice handing its engine
// to a leaf package — which rewrites the qualifiers around the code as it lands
// (request arrives as pkgRequest, the receiver is renamed). The behavior is
// intact, so the branch must be reported as a relocation and not refused.
func TestIntegrationDetectRevertedMerges_CodeMovedToAnotherPackageIsNoRevert(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edits map[string]string
	}{
		{
			name: "into a file the other package already has",
			edits: map[string]string{
				"internal/pkg/foo.go":   pkgFooBase,
				"internal/other/bar.go": pkgOtherVerdictHelper,
			},
		},
		{
			name: "into a file the move itself creates",
			edits: map[string]string{
				"internal/pkg/foo.go":      pkgFooBase,
				"internal/other/helper.go": pkgOtherVerdictHelper,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newPackageRelocationScenario(t, "verdict-literal", pkgFooWithVerdictLiteral)
			commitPolecat(t, s.polecat, tt.edits, pkgRefactorSubject)

			report := detectRevertedMerges(t, s.polecat)
			if len(report.Reverted) != 0 {
				t.Fatalf("detectRevertedMerges refused a move to another package: %+v", report.Reverted)
			}
			if len(report.Relocated) != 1 {
				t.Fatalf("detectRevertedMerges reported %d relocations, want 1: %+v", len(report.Relocated), report.Relocated)
			}
			wantCommit, err := git.NewGit(s.polecat).Rev("origin/main")
			if err != nil {
				t.Fatalf("rev origin/main: %v", err)
			}
			if report.Relocated[0].Commit != wantCommit {
				t.Errorf("relocated commit = %s, want main's additive commit %s", report.Relocated[0].Commit, wantCommit)
			}
			if !containsString(report.Relocated[0].Paths, "internal/pkg/foo.go") {
				t.Errorf("relocated paths %v missing internal/pkg/foo.go", report.Relocated[0].Paths)
			}
		})
	}
}

// The constants below are the gt-s5eou shape: main's commit fixes a lock leak
// by turning one statement into the same statement with a keyword in front, and
// a stale branch turns it back.
const (
	pkgUnlockSubject = "store: release the lock on every path out of put"

	pkgLockBefore = `package pkg

func (s *store) put(k string) {
	s.mu.Lock()
	s.items[k] = true
	s.mu.Unlock()
}
`

	pkgLockFixed = `package pkg

func (s *store) put(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[k] = true
}
`

	// pkgUnrelatedUnlock is a file the stale branch's own work wrote, in another
	// package, that holds the line the fix replaced with its keyword stripped.
	pkgUnrelatedUnlock = `package other

func release(x *guard) {
	x.Unlock()
}
`
)

// TestDetectRevertedMerges_UndoingAOneLineFixBesideAnUnrelatedFileIsARevert is
// the gt-s5eou incident: the polecat reverts mu.Unlock() to defer mu.Unlock()'s
// predecessor while writing an unrelated file that holds x.Unlock(). The fix
// must not be read as moved to that file.
func TestIntegrationDetectRevertedMerges_UndoingAOneLineFixBesideAnUnrelatedFileIsARevert(t *testing.T) {
	t.Parallel()
	p := cachedGitFixtureStrings(t, "packageRelocation/unlock-fix", func(dir string) []string {
		sp := buildPackageScenarioFrom(t, dir, pkgLockBefore, pkgLockFixed)
		return []string{sp.seed, sp.polecat}
	})
	commitPolecat(t, p[1], map[string]string{
		"internal/pkg/foo.go":       pkgLockBefore,
		"internal/other/release.go": pkgUnrelatedUnlock,
	}, pkgUnlockSubject)

	report := detectRevertedMerges(t, p[1])
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges read an undone one-line fix as a relocation: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
	if !containsString(report.Reverted[0].Paths, "internal/pkg/foo.go") {
		t.Errorf("reverted paths %v missing internal/pkg/foo.go", report.Reverted[0].Paths)
	}
}

// TestDetectRevertedMerges_OneLineCopiedToAnotherPackageIsARevert is the
// threshold's edge: main's change is a single code line, and another package
// holds it — the same code the move test above carries, one line of it. A line
// that small turns up in any unrelated file, so it excuses nothing (gt-s5eou).
func TestIntegrationDetectRevertedMerges_OneLineCopiedToAnotherPackageIsARevert(t *testing.T) {
	t.Parallel()
	s := newPackageRelocationScenario(t, "summary-literal", pkgFooWithSummaryLiteral)
	commitPolecat(t, s.polecat, map[string]string{
		"internal/pkg/foo.go":      pkgFooBase,
		"internal/other/helper.go": pkgOtherPackageHelper,
	}, pkgRefactorSubject)

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges excused a one-line change by a copy in another package: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
}

// TestDetectRevertedMerges_CodeDeletedOutrightIsARevert is the fail-closed
// control for that reading: the same edit to the file the commit wrote, with
// the code carried nowhere. No file in the candidate's tree holds those lines,
// so the observation stands — a function the branch really deleted is still a
// revert, however it is compared.
func TestIntegrationDetectRevertedMerges_CodeDeletedOutrightIsARevert(t *testing.T) {
	t.Parallel()
	s := newPackageRelocationScenario(t, "summary-literal", pkgFooWithSummaryLiteral)
	commitPolecat(t, s.polecat, map[string]string{"internal/pkg/foo.go": pkgFooBase}, pkgRefactorSubject)

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges called a deletion a relocation: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
	if !containsString(report.Reverted[0].Paths, "internal/pkg/foo.go") {
		t.Errorf("reverted paths %v missing internal/pkg/foo.go", report.Reverted[0].Paths)
	}
}

// TestDetectRevertedMerges_CommentOnlyChangeIsARevert pins the second edge of
// excluding comments from the relocation evidence: with no code line to find,
// there is no evidence of a move, so a comment block that survives verbatim
// elsewhere in the package is still reported as reverted.
func TestIntegrationDetectRevertedMerges_CommentOnlyChangeIsARevert(t *testing.T) {
	t.Parallel()
	s := newPackageRelocationScenario(t, "comment-block", pkgFooWithCommentBlock)
	commitPolecat(t, s.polecat, map[string]string{"internal/pkg/foo.go": pkgFooWithHelper}, pkgRefactorSubject)

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges excused a change with no code line to look for: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
}

// The constants below build the gt-mdyds shapes: a package that predates the
// polecat's checkout, and a later commit on main that ADDS a file to it. The
// addition is pure — the file is new, the package is not — so the deletion the
// polecat makes restores the state of the path before that commit, which is
// what a stale checkout holds too. Nothing but the candidate's own tree
// separates the two.
const (
	pkgTestMainSubject = "testpolicy: run the unit tier through unittier.Main"

	// pkgTestMain is main's purely additive commit, and the file the polecat
	// then retires or carries to another package.
	pkgTestMain = `package pkg

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
`

	// pkgTestMainMoved is the same file one package over: the package clause
	// follows the file, which is the edit that makes this a move to git's
	// rename detection rather than an identical copy.
	pkgTestMainMoved = `package slot

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
`

	// pkgUnrelatedFile shares no code line with pkgTestMain, so a deletion it
	// accompanies is a deletion the candidate's tree does not answer for.
	pkgUnrelatedFile = `package slot

// Retire records the package a slot run left behind.
type Retire struct {
	Dir  string
	When string
}
`
)

// newMergedTestMainScenario builds the gt-mdyds repositories: origin/main holds
// one file in the package, and a second commit adds pkgTestMain to it.
func newMergedTestMainScenario(t *testing.T) scenarioPaths {
	t.Helper()
	p := cachedGitFixtureStrings(t, "mergedTestMain", func(dir string) []string {
		sp := buildMergedTestMainScenario(t, dir)
		return []string{sp.seed, sp.polecat}
	})
	return scenarioPaths{seed: p[0], polecat: p[1]}
}

func buildMergedTestMainScenario(t *testing.T, dir string) scenarioPaths {
	t.Helper()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFileAt(t, filepath.Join(seed, "internal/pkg/foo.go"), pkgFooBase)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	// main's additive commit, and the point the polecat's checkout is cut at:
	// the branch under test descends from it, so nothing about it is stale.
	writeTestFileAt(t, filepath.Join(seed, "internal/pkg/main_test.go"), pkgTestMain)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", pkgTestMainSubject)
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/zircon/gt-test")
	return scenarioPaths{seed: seed, polecat: polecat}
}

// TestDetectRevertedMerges_RetiringAPackageIsNoRevert is the gt-mdyds incident:
// a polecat retires a package whole, so every file in it leaves the tree —
// including a file main added there after the checkout was cut. Read as an
// inversion of that commit, the retirement is refused and the polecat has to
// ask for an override to do what its bead says.
func TestIntegrationDetectRevertedMerges_RetiringAPackageIsNoRevert(t *testing.T) {
	t.Parallel()
	s := newMergedTestMainScenario(t)

	if err := os.RemoveAll(filepath.Join(s.polecat, "internal/pkg")); err != nil {
		t.Fatalf("removing internal/pkg: %v", err)
	}
	runGitCmd(t, s.polecat, "add", "-A")
	runGitCmd(t, s.polecat, "commit", "-m", "feat: retire internal/pkg (gt-test)")

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges refused the retirement of a whole package: %+v", report)
	}
}

// TestDetectRevertedMerges_DeletingAMergedFileInASurvivingPackageRefuses is the
// fail-closed control for that reading: the same file, deleted from a package
// the polecat keeps. What main added to the path is then gone from a tree that
// still holds its neighbors — the state a stale checkout leaves behind — so the
// deletion is reported as the inversion it is.
func TestIntegrationDetectRevertedMerges_DeletingAMergedFileInASurvivingPackageRefuses(t *testing.T) {
	t.Parallel()
	s := newMergedTestMainScenario(t)

	runGitCmd(t, s.polecat, "rm", "internal/pkg/main_test.go")
	runGitCmd(t, s.polecat, "commit", "-m", "feat: drop the package's TestMain (gt-test)")

	g := git.NewGit(s.polecat)
	wantCommit, err := g.Rev("origin/main")
	if err != nil {
		t.Fatalf("rev origin/main: %v", err)
	}
	report := detectRevertedMerges(t, s.polecat)
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want the one that added internal/pkg/main_test.go: %+v", len(report.Reverted), report)
	}
	if report.Reverted[0].Commit != wantCommit {
		t.Errorf("reverted commit = %s, want %s (the commit that added the file)", report.Reverted[0].Commit, wantCommit)
	}
	if !containsString(report.Reverted[0].Paths, "internal/pkg/main_test.go") {
		t.Errorf("reverted paths %v missing internal/pkg/main_test.go", report.Reverted[0].Paths)
	}
}

// TestDetectRevertedMerges_MovingAFileToAnotherPackageIsNoRevert is the
// gt-638go.12 incident: the polecat's work carries a file main added out of its
// package and into another one (internal/cmd -> internal/slot there), rewriting
// the package clause and the references around it. The path the commit wrote is
// gone, but its content is not — it is at a new path in the candidate's own
// tree, which is what git's rename detection calls a move.
func TestIntegrationDetectRevertedMerges_MovingAFileToAnotherPackageIsNoRevert(t *testing.T) {
	t.Parallel()
	s := newMergedTestMainScenario(t)

	runGitCmd(t, s.polecat, "rm", "internal/pkg/main_test.go")
	writeTestFileAt(t, filepath.Join(s.polecat, "internal/slot/main_test.go"), pkgTestMainMoved)
	runGitCmd(t, s.polecat, "add", "-A")
	runGitCmd(t, s.polecat, "commit", "-m", "refactor: move the TestMain into internal/slot (gt-test)")

	g := git.NewGit(s.polecat)
	wantCommit, err := g.Rev("origin/main")
	if err != nil {
		t.Fatalf("rev origin/main: %v", err)
	}
	report := detectRevertedMerges(t, s.polecat)
	if len(report.Reverted) != 0 {
		t.Fatalf("detectRevertedMerges refused a file moved to another package: %+v", report.Reverted)
	}
	if len(report.Relocated) != 1 {
		t.Fatalf("detectRevertedMerges reported %d relocations, want 1: %+v", len(report.Relocated), report.Relocated)
	}
	if report.Relocated[0].Commit != wantCommit {
		t.Errorf("relocated commit = %s, want %s (the commit that added the file)", report.Relocated[0].Commit, wantCommit)
	}
	if !containsString(report.Relocated[0].Paths, "internal/pkg/main_test.go") {
		t.Errorf("relocated paths %v missing internal/pkg/main_test.go", report.Relocated[0].Paths)
	}
}

// TestDetectRevertedMerges_DeletingAFileForAnUnrelatedNewFileRefuses is the
// fail-closed control for the move reading: the polecat adds a file too, but
// nothing of the deleted file's content survives in it, so the deletion is not
// a move and the commit that main added the file with is still undone.
func TestIntegrationDetectRevertedMerges_DeletingAFileForAnUnrelatedNewFileRefuses(t *testing.T) {
	t.Parallel()
	s := newMergedTestMainScenario(t)

	runGitCmd(t, s.polecat, "rm", "internal/pkg/main_test.go")
	writeTestFileAt(t, filepath.Join(s.polecat, "internal/slot/retire.go"), pkgUnrelatedFile)
	runGitCmd(t, s.polecat, "add", "-A")
	runGitCmd(t, s.polecat, "commit", "-m", "feat: retire the TestMain, add Retire (gt-test)")

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges called an unrelated addition a relocation: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
	if !containsString(report.Reverted[0].Paths, "internal/pkg/main_test.go") {
		t.Errorf("reverted paths %v missing internal/pkg/main_test.go", report.Reverted[0].Paths)
	}
}

// TestReportRevertedMerges_ReportsRelocationAndContinues checks the
// operator-facing half: the guard states what it accepted — the commit and the
// path the content left — and lets the submission through.
func TestIntegrationReportRevertedMerges_ReportsRelocationAndContinues(t *testing.T) {
	t.Parallel()
	s := newPackageRelocationScenario(t, "summary-literal", pkgFooWithSummaryLiteral)
	commitPolecat(t, s.polecat, map[string]string{"internal/pkg/foo.go": pkgFooWithHelper}, pkgRefactorSubject)

	g := git.NewGit(s.polecat)
	if err := reportRevertedMerges(g, "origin/main"); err != nil {
		t.Fatalf("reportRevertedMerges refused a relocated block: %v", err)
	}
	note := relocatedMergesNote(g, detectRevertedMerges(t, s.polecat).Relocated)
	for _, want := range []string{"Relocated", pkgAdditiveSubject, "internal/pkg/foo.go"} {
		if !strings.Contains(note, want) {
			t.Errorf("relocation note missing %q:\n%s", want, note)
		}
	}
}

// writeTestFileAt writes content to a path whose parent directories the file
// itself creates; writeTestFile assumes they exist.
func writeTestFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	writeTestFile(t, path, content)
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

// The gt-gas9g shape: main's history folds the two wisps reads into one call,
// then adds the issues read on its own, and a polecat branch cut after that
// commit writes one combined call over both. The issues read is a purely
// additive commit, which is what made deleting its lines read as an inversion
// (gt-59p7e).
const (
	preloadWispsSubject     = "perf: fold the two wisps reads into one (gt-test)"
	preloadIssuesSubject    = "perf: answer the issues reads from one query (gt-test)"
	preloadSupersedeSubject = "perf: read both tables in one preload (gt-test)"

	preloadFile = "internal/cmd/polecat.go"

	preloadTwoReads = `package cmd

func buildRigSeats(b *bd.Client) {
	if err := b.ListLabeledWisps("gt:agent"); err != nil {
		warnf(err)
	}
	if err := b.ListLabeledWisps("gt:merge-request"); err != nil {
		warnf(err)
	}
}
`

	preloadCollapsed = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE wisps read for both label sets this listing wants.
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		warnf(err)
	}
}
`

	preloadWithIssues = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE wisps read for both label sets this listing wants.
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		warnf(err)
	}

	// The issues read, one more round trip.
	if err := b.PreloadIssues([]string{"gt:agent", "gt:merge-request"}, workStatuses); err != nil {
		warnf(err)
	}
}
`

	preloadCombined = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE read for both tables.
	if err := b.PreloadBeads([]string{"gt:agent", "gt:merge-request"}, workStatuses); err != nil {
		warnf(err)
	}
}
`

	// preloadReadDropped is the fail-closed control's tree: the read deleted
	// with no call written over it, the branch's own edit the only thing beside
	// the hole.
	preloadReadDropped = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE wisps read for both label sets this listing wants.
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		warnf(err)
	}
	audit("seats")
}
`
)

func newSupersededPreloadScenario(t *testing.T) scenarioPaths {
	t.Helper()
	p := cachedGitFixtureStrings(t, "supersededPreload", func(dir string) []string {
		sp := buildSupersededPreloadScenario(t, dir)
		return []string{sp.seed, sp.polecat}
	})
	return scenarioPaths{seed: p[0], polecat: p[1]}
}

func buildSupersededPreloadScenario(t *testing.T, dir string) scenarioPaths {
	t.Helper()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFileAt(t, filepath.Join(seed, preloadFile), preloadTwoReads)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	writeTestFileAt(t, filepath.Join(seed, preloadFile), preloadCollapsed)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", preloadWispsSubject)
	runGitCmd(t, seed, "push", "origin", "main")

	// The purely additive commit, and the point the polecat's checkout is cut
	// at: the branch under test descends from it, so nothing about it is stale.
	writeTestFileAt(t, filepath.Join(seed, preloadFile), preloadWithIssues)
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", preloadIssuesSubject)
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/zircon/gt-test")
	return scenarioPaths{seed: seed, polecat: polecat}
}

// TestDetectRevertedMerges_SupersedingThePreloadsIsNoRevert is the gt-gas9g
// incident: the branch writes the one combined call that does both reads' work
// over the two calls main merged, which is the change its bead asked for. The
// call it deletes was added on its own, so gt done refused the branch and an
// operator had to grant --allow-reverts by hand (gt-59p7e).
func TestIntegrationDetectRevertedMerges_SupersedingThePreloadsIsNoRevert(t *testing.T) {
	t.Parallel()
	s := newSupersededPreloadScenario(t)
	commitPolecat(t, s.polecat, map[string]string{preloadFile: preloadCombined}, preloadSupersedeSubject)

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges refused a branch that supersedes the merged reads: reverted %+v, relocated %+v",
			report.Reverted, report.Relocated)
	}
}

// TestDetectRevertedMerges_DeletingTheAddedReadStillRefuses is that reading's
// fail-closed control: the same branch drops the added read with nothing
// written over it, so the removal is the whole of the addition and the commit
// that added it is still undone.
func TestIntegrationDetectRevertedMerges_DeletingTheAddedReadStillRefuses(t *testing.T) {
	t.Parallel()
	s := newSupersededPreloadScenario(t)
	commitPolecat(t, s.polecat, map[string]string{preloadFile: preloadReadDropped}, "refactor: drop the issues read (gt-test)")

	report := detectRevertedMerges(t, s.polecat)
	if len(report.Relocated) != 0 {
		t.Errorf("detectRevertedMerges called a deletion a relocation: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("detectRevertedMerges found %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
	wantCommit, err := git.NewGit(s.polecat).Rev("origin/main")
	if err != nil {
		t.Fatalf("rev origin/main: %v", err)
	}
	if report.Reverted[0].Commit != wantCommit {
		t.Errorf("reverted commit = %s, want main's additive commit %s", report.Reverted[0].Commit, wantCommit)
	}
	if !containsString(report.Reverted[0].Paths, preloadFile) {
		t.Errorf("reverted paths %v missing %s", report.Reverted[0].Paths, preloadFile)
	}
}
