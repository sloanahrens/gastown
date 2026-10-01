//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestIntegrationDoneCloseTimeInvariantSkipReason runs the close-time
// invariant's branch/target resolution (doneCloseTimeInvariantSkipReason)
// against real repositories, without a live bd server. The decision itself is
// unit-tested over fakeCloseTimeCommitCounter
// (TestCloseTimeInvariantSkipReason_*).
func TestIntegrationDoneCloseTimeInvariantSkipReason(t *testing.T) {
	t.Parallel()
	// The wrapper identifies "zero commits ahead", exit (a).
	t.Run("ZeroCommitsAgainstRealGit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		testRunGit(t, dir, "init", "-b", "main")
		testRunGit(t, dir, "config", "user.email", "test@test.com")
		testRunGit(t, dir, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		testRunGit(t, dir, "add", ".")
		testRunGit(t, dir, "commit", "-m", "initial")
		testRunGit(t, dir, "checkout", "-b", "polecat/basalt/gt-6hmz+abc")

		// townRoot/rigName don't resolve to a real rig config, so the wrapper
		// falls back to defaultBranch "main" — which matches the repo above.
		got := doneCloseTimeInvariantSkipReason(dir, filepath.Join(dir, "no-such-town"), "no-such-rig", "gt-6hmz")
		if got != "" {
			t.Errorf("expected close allowed with zero commits ahead of main, got skip reason %q", got)
		}
	})

	// The wrapper doesn't try to compare a branch against itself when the
	// current branch IS the rig's default branch (e.g. a non-polecat context).
	t.Run("OnDefaultBranchAllowsClose", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		testRunGit(t, dir, "init", "-b", "main")
		testRunGit(t, dir, "config", "user.email", "test@test.com")
		testRunGit(t, dir, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		testRunGit(t, dir, "add", ".")
		testRunGit(t, dir, "commit", "-m", "initial")

		got := doneCloseTimeInvariantSkipReason(dir, filepath.Join(dir, "no-such-town"), "no-such-rig", "gt-6hmz")
		if got != "" {
			t.Errorf("expected close allowed when on the default branch, got skip reason %q", got)
		}
	})

	// TestDoneCloseTimeInvariantSkipReason_StaleLocalMainAllowsZeroCommitClose
	// is the real-git regression test for gt-6hmz om-editorial finding 1: a
	// polecat worktree is created from origin/<default> (internal/polecat/
	// manager.go:798), so the shared .repo.git's local "main" is routinely
	// behind origin/main. A branch with zero commits the polecat authored (a
	// no_merge/review_only/report-only close) must still be allowed to close
	// even though local main..branch is nonzero — the wrapper must compare
	// against origin/main, not local main. This must fail before the fix (it
	// compared bare "main") and pass after.
	t.Run("StaleLocalMainAllowsZeroCommitClose", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		remote := filepath.Join(tmp, "remote.git")
		testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

		seed := filepath.Join(tmp, "seed")
		testRunGit(t, tmp, "init", "--initial-branch", "main", seed)
		testRunGit(t, seed, "config", "user.email", "test@test.com")
		testRunGit(t, seed, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# initial\n"), 0644); err != nil {
			t.Fatal(err)
		}
		testRunGit(t, seed, "add", ".")
		testRunGit(t, seed, "commit", "-m", "initial")
		testRunGit(t, seed, "remote", "add", "origin", remote)
		testRunGit(t, seed, "push", "origin", "main")

		// Clone the repo at the point above, before origin/main advances — this
		// is what leaves "work"'s local "main" ref stale.
		work := filepath.Join(tmp, "work")
		testRunGit(t, tmp, "clone", remote, work)
		testRunGit(t, work, "config", "user.email", "test@test.com")
		testRunGit(t, work, "config", "user.name", "Test")

		// origin/main advances independently (another polecat merged) while the
		// SHARED local .repo.git's "main" branch — a distinct, separately
		// checked out worktree the wrapper's git.NewGit(cwd) never fetches —
		// stays behind at clone-time main.
		testRunGit(t, seed, "checkout", "main")
		if err := os.WriteFile(filepath.Join(seed, "main-new.txt"), []byte("advance\n"), 0644); err != nil {
			t.Fatal(err)
		}
		testRunGit(t, seed, "add", ".")
		testRunGit(t, seed, "commit", "-m", "advance main")
		testRunGit(t, seed, "push", "origin", "main")
		testRunGit(t, work, "fetch", "origin")

		// Polecat worktree: created from origin/<default> AFTER it advanced
		// (internal/polecat/manager.go:798), so the branch has zero commits of
		// its own relative to origin/main — a report-only task — even though
		// "work"'s local "main" ref is still the un-fetched, stale clone-time
		// commit.
		testRunGit(t, work, "checkout", "-b", "polecat/basalt/gt-6hmz+abc", "origin/main")

		// Sanity: local main (stale) vs branch is nonzero even though the
		// polecat authored zero commits — this is the false-refusal trap.
		staleAhead, err := git.NewGit(work).CommitsAhead("main", "polecat/basalt/gt-6hmz+abc")
		if err != nil {
			t.Fatalf("CommitsAhead(main, branch): %v", err)
		}
		if staleAhead == 0 {
			t.Fatal("test setup invalid: expected local stale main to diverge from the branch")
		}

		got := doneCloseTimeInvariantSkipReason(work, filepath.Join(tmp, "no-such-town"), "no-such-rig", "gt-6hmz")
		if got != "" {
			t.Errorf("expected close allowed comparing against origin/main (zero polecat commits), got skip reason %q — wrapper likely compared against stale local main instead", got)
		}
	})
}
