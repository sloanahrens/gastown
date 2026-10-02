//go:build integration

package done

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// recoverDivergedPush against real repos: what real git's patch-ids make of
// a rebase, a rework redispatch, genuine divergence and a merge commit's own
// content. The decision table itself is unit-tested against a fake in
// done_rebase_test.go.

// TestRecoverDivergedPush_RealRepo exercises the full scenario end to end
// against real git repos: a branch pushed by one dispatch, main advancing,
// then a second dispatch reusing the branch and rebasing it onto origin/main
// (the formula's branch-reuse step) before gt done's plain push fails
// non-fast-forward. Recovery must land the rebased tip on origin without
// losing either commit's content. (gt-bf5x)
func TestIntegrationRecoverDivergedPush_RealRepo(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	seed := filepath.Join(tmp, "seed")
	testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "README.md", "# initial\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "initial")
	testRunGit(t, seed, "remote", "add", "origin", remote)
	testRunGit(t, seed, "push", "origin", "main")

	// First dispatch: branch, do work, push (this is what lands on origin
	// before the branch gets reused by a later dispatch).
	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "feature.txt", "feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature work")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	// main advances independently while the MR sits in the queue.
	testRunGit(t, seed, "checkout", "main")
	writeRepoFile(t, seed, "main-new.txt", "advance\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "advance main")
	testRunGit(t, seed, "push", "origin", "main")

	// Second dispatch: fresh checkout of the reused branch, rebased onto
	// origin/main per the formula's branch-reuse step — diverges history from
	// origin while keeping the content identical.
	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "-b", "feature", "origin/feature")
	testRunGit(t, work, "fetch", "origin")
	testRunGit(t, work, "rebase", "origin/main")

	g := gitpkg.NewGit(work)

	// The primary non-force push (what gt done tries first) must fail
	// non-fast-forward, exactly as observed in the bead.
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward after rebase")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "diverged by rebase, content identical") {
		t.Errorf("diagnosis = %q, want mention of rebase/content-identical", diagnosis)
	}

	verify := filepath.Join(tmp, "verify")
	testRunGit(t, tmp, "clone", "--branch", "feature", remote, verify)
	if _, statErr := os.Stat(filepath.Join(verify, "main-new.txt")); statErr != nil {
		t.Errorf("main-new.txt missing on origin/feature after recovery: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(verify, "feature.txt")); statErr != nil {
		t.Errorf("feature.txt missing on origin/feature after recovery: %v", statErr)
	}
}

// TestRecoverDivergedPush_RealRepoReworkRedispatch is the end-to-end form of
// the gt-i0z3 bug against real git: the reused branch is rebased *and* carries
// a new fix commit, which is what mol-polecat-work's branch-reuse step does on
// every redispatch. The two ranges are not patch-identical by design, so the
// pure-rebase-only check called it "real divergence" and raised the possible
// work-loss alarm on work that was never at risk. Recovery must land all three
// commits' content.
func TestIntegrationRecoverDivergedPush_RealRepoReworkRedispatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	seed := filepath.Join(tmp, "seed")
	testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "README.md", "# initial\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "initial")
	testRunGit(t, seed, "remote", "add", "origin", remote)
	testRunGit(t, seed, "push", "origin", "main")

	// First dispatch: two commits, pushed — this is the state origin holds
	// when the branch comes back for rework.
	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "feature.txt", "feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature work")
	writeRepoFile(t, seed, "feature-two.txt", "more feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "more feature work")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	// main advances while the MR waits in the queue.
	testRunGit(t, seed, "checkout", "main")
	writeRepoFile(t, seed, "main-new.txt", "advance\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "advance main")
	testRunGit(t, seed, "push", "origin", "main")

	// Second dispatch: reuse the branch, rebase onto origin/main, then add the
	// targeted fix commit the rework produced.
	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "-b", "feature", "origin/feature")
	testRunGit(t, work, "fetch", "origin")
	testRunGit(t, work, "rebase", "origin/main")
	writeRepoFile(t, work, "rework-fix.txt", "the review fix\n")
	testRunGit(t, work, "add", ".")
	testRunGit(t, work, "commit", "-m", "rework: address review feedback")

	g := gitpkg.NewGit(work)
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward after rebase + rework commit")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery on the rework-redispatch path, got diagnosis=%q", diagnosis)
	}
	if !strings.Contains(diagnosis, "rework-redispatch") {
		t.Errorf("diagnosis = %q, want it to name the rework case", diagnosis)
	}

	// Every commit's content must be on origin: the two origin already had,
	// plus the rework fix, on top of the advanced main.
	verify := filepath.Join(tmp, "verify")
	testRunGit(t, tmp, "clone", "--branch", "feature", remote, verify)
	for _, name := range []string{"main-new.txt", "feature.txt", "feature-two.txt", "rework-fix.txt"} {
		if _, statErr := os.Stat(filepath.Join(verify, name)); statErr != nil {
			t.Errorf("%s missing on origin/feature after recovery: %v", name, statErr)
		}
	}
}

// TestRecoverDivergedPush_RealRepoRefusesGenuineDivergence guards the safety
// side: when origin's tip is real, different work (not the same content
// rebased), recovery must refuse and leave origin untouched rather than
// clobber it.
func TestIntegrationRecoverDivergedPush_RealRepoRefusesGenuineDivergence(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	seed := filepath.Join(tmp, "seed")
	testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "README.md", "# initial\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "initial")
	testRunGit(t, seed, "remote", "add", "origin", remote)
	testRunGit(t, seed, "push", "origin", "main")

	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "feature.txt", "feature work\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature work")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "feature")
	writeRepoFile(t, work, "feature-more.txt", "more local work\n")
	testRunGit(t, work, "add", ".")
	testRunGit(t, work, "commit", "-m", "more feature work")

	// Someone else pushes genuinely different content to the same branch
	// concurrently.
	other := filepath.Join(tmp, "other")
	testRunGit(t, tmp, "clone", remote, other)
	testRunGit(t, other, "config", "user.email", "test@test.com")
	testRunGit(t, other, "config", "user.name", "Test")
	testRunGit(t, other, "checkout", "feature")
	writeRepoFile(t, other, "someone-elses-work.txt", "different content\n")
	testRunGit(t, other, "add", ".")
	testRunGit(t, other, "commit", "-m", "someone else's real work")
	testRunGit(t, other, "push", "origin", "feature:feature")
	otherHead := gitpkgRev(t, other, "HEAD")

	g := gitpkg.NewGit(work)
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: origin has genuinely different content, not just a rebase")
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}

	testRunGit(t, work, "fetch", "origin")
	if got := gitpkgRev(t, work, "origin/feature"); got != otherHead {
		t.Errorf("origin/feature was modified: got %s, want %s (untouched)", got, otherHead)
	}
}

// TestRecoverDivergedPush_RealRepoMergeCommitContentRefuses guards the gap
// gt-5wp5 found: a merge commit can carry content of its own — whatever was
// added while resolving it, on top of what either parent already
// contributed — that no individual commit's diff carries. A default rebase
// drops merge commits, replaying only their non-merge ancestors, so a branch
// rebased after landing such a merge loses that content entirely. The
// per-commit comparison must catch this and refuse, not force-push local's
// copy (missing the content) over origin's (which still has it).
func TestIntegrationRecoverDivergedPush_RealRepoMergeCommitContentRefuses(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	seed := filepath.Join(tmp, "seed")
	testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "README.md", "# initial\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "initial")
	testRunGit(t, seed, "remote", "add", "origin", remote)
	testRunGit(t, seed, "push", "origin", "main")

	// Two branches off the same point, touching different files so both
	// replay cleanly after a later rebase — the content this test is about
	// comes from the merge commit itself, not from a replay conflict.
	testRunGit(t, seed, "checkout", "-b", "feature")
	writeRepoFile(t, seed, "a.txt", "feature\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "feature: add a.txt")

	testRunGit(t, seed, "checkout", "main")
	testRunGit(t, seed, "checkout", "-b", "topic")
	writeRepoFile(t, seed, "b.txt", "topic\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "topic: add b.txt")

	// Merge topic into feature, then add content in the merge commit itself
	// — a conflict resolution lands the same way. c.txt exists only on this
	// commit, attached to neither parent's own diff.
	testRunGit(t, seed, "checkout", "feature")
	testRunGit(t, seed, "merge", "--no-ff", "--no-commit", "topic")
	writeRepoFile(t, seed, "c.txt", "resolved during the merge\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "Merge topic into feature, plus fixup")
	testRunGit(t, seed, "push", "origin", "feature:feature")

	// main advances while the MR sits in the queue.
	testRunGit(t, seed, "checkout", "main")
	writeRepoFile(t, seed, "main-new.txt", "advance\n")
	testRunGit(t, seed, "add", ".")
	testRunGit(t, seed, "commit", "-m", "advance main")
	testRunGit(t, seed, "push", "origin", "main")

	// Second dispatch: fresh checkout of the reused branch, rebased onto
	// origin/main per the formula's branch-reuse step. A default rebase drops
	// the merge commit and replays its non-merge ancestors individually —
	// a.txt and b.txt both come back, but c.txt (the merge's own content)
	// does not.
	work := filepath.Join(tmp, "work")
	testRunGit(t, tmp, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	testRunGit(t, work, "checkout", "-b", "feature", "origin/feature")
	testRunGit(t, work, "fetch", "origin")
	testRunGit(t, work, "rebase", "origin/main")

	if _, statErr := os.Stat(filepath.Join(work, "c.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition failed: rebase should have dropped c.txt, stat err: %v", statErr)
	}

	g := gitpkg.NewGit(work)
	if err := g.Push("origin", "feature:feature", false); err == nil {
		t.Fatal("expected plain push to fail non-fast-forward after rebase")
	}

	recovered, diagnosis, err := recoverDivergedPush(g, "origin", "feature:feature", "feature", "origin/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Fatal("must not recover: the rebase dropped the merge commit's own content (c.txt)")
	}
	if !strings.Contains(diagnosis, "real divergence") {
		t.Errorf("diagnosis = %q, want mention of real divergence", diagnosis)
	}

	// origin/feature must still carry the merge's content untouched.
	verify := filepath.Join(tmp, "verify")
	testRunGit(t, tmp, "clone", "--branch", "feature", remote, verify)
	if _, statErr := os.Stat(filepath.Join(verify, "c.txt")); statErr != nil {
		t.Errorf("c.txt missing on origin/feature after recovery attempt: %v", statErr)
	}
}

func gitpkgRev(t *testing.T, dir, ref string) string {
	t.Helper()
	sha, err := gitpkg.NewGit(dir).Rev(ref)
	if err != nil {
		t.Fatalf("rev %s in %s: %v", ref, dir, err)
	}
	return sha
}
