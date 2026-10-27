//go:build integration

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestIntegrationPolecatBranchPreservation runs branch preservation before a
// nuke, and the branch delete after it, against a real clone and remote: a
// push can be rejected, and whether a remote ref holds the tip is git's answer.
// TestPreserveFailureBlockerRequiresForceAndAcknowledgement unit-tests the
// override rule.
func TestIntegrationPolecatBranchPreservation(t *testing.T) {
	t.Parallel()
	// The witness's flint nuke: the branch was pushed, then rewritten locally
	// (rebase/amend), so origin/<branch> is at a commit the local tip does not
	// descend from. The plain push is rejected non-fast-forward and the work exists
	// nowhere on origin — the old code printed "remote branch preserved" and
	// deleted the branch anyway.
	t.Run("PreserveBranchBeforeNukeFallsBackToSideRefWhenPushRejected", func(t *testing.T) {
		t.Parallel()
		repo, _, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)

		runGitCmd(t, repo, "push", "origin", branch)
		remoteTip := gitNow(t, repo, "rev-parse", "refs/remotes/origin/"+branch)
		writeTestFile(t, filepath.Join(repo, "work.txt"), "rewritten work\n")
		runGitCmd(t, repo, "commit", "-a", "--amend", "-m", "polecat work (rebased)")
		tip := gitNow(t, repo, "rev-parse", "refs/heads/"+branch)
		if tip == remoteTip {
			t.Fatal("fixture is wrong: local tip should differ from the published tip")
		}

		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", t.TempDir())
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if outcome.AlreadyThere {
			t.Fatal("AlreadyThere = true, want false: this tip was on no remote ref")
		}
		wantRef := "origin/" + branch + "-" + tip[:7]
		if outcome.RemoteRef != wantRef {
			t.Fatalf("RemoteRef = %q, want %q", outcome.RemoteRef, wantRef)
		}
		if got := gitNow(t, repo, "rev-parse", "refs/remotes/"+wantRef); got != tip {
			t.Fatalf("%s = %s, want the preserved tip %s", wantRef, got, tip)
		}
		if got := gitNow(t, repo, "rev-parse", "refs/remotes/origin/"+branch); got != remoteTip {
			t.Fatalf("origin/%s = %s, want it left alone at %s", branch, got, remoteTip)
		}
	})

	// The branch was pushed and then deleted unmerged on the remote. The stale
	// refs/remotes/origin/<branch> still holds the tip, so a plain fetch would
	// call it preserved and the nuke would delete the only real copy.
	t.Run("PreserveBranchBeforeNukeRepushesWhenRemoteBranchWasDeleted", func(t *testing.T) {
		t.Parallel()
		repo, remote, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)

		runGitCmd(t, repo, "push", "origin", branch)
		// Delete inside the bare remote: `git push origin :<branch>` would also
		// drop the local tracking ref and leave nothing stale.
		runGitCmd(t, remote, "update-ref", "-d", "refs/heads/"+branch)
		tip := gitNow(t, repo, "rev-parse", "refs/heads/"+branch)
		if stale := gitNow(t, repo, "rev-parse", "refs/remotes/origin/"+branch); stale != tip {
			t.Fatalf("fixture is wrong: stale origin/%s = %s, want the tip %s", branch, stale, tip)
		}
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published != "" {
			t.Fatalf("fixture is wrong: %s should be deleted on the remote, got %q", branch, published)
		}

		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", t.TempDir())
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if outcome.AlreadyThere {
			t.Fatal("AlreadyThere = true, want false: the stale tracking ref is not a remote copy")
		}
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); !strings.HasPrefix(published, tip) {
			t.Fatalf("remote %s = %q, want the tip %s pushed back", branch, published, tip)
		}
	})

	// A branch already merged into main is preserved even though no origin/<branch>
	// exists: reachability, not tip membership, is the test. The jade run showed
	// two heads that are ancestors of main where `ls-remote` on the branch name
	// found nothing.
	t.Run("PreserveBranchBeforeNukeRecognizesWorkAlreadyMergedToMain", func(t *testing.T) {
		t.Parallel()
		repo, remote, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)

		runGitCmd(t, repo, "checkout", "main")
		runGitCmd(t, repo, "merge", "--ff-only", branch)
		runGitCmd(t, repo, "push", "origin", "main")
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published != "" {
			t.Fatalf("fixture is wrong: origin/%s should not exist, got %q", branch, published)
		}

		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", t.TempDir())
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if !outcome.AlreadyThere {
			t.Fatal("AlreadyThere = false, want true: the tip is an ancestor of origin/main")
		}
		if outcome.RemoteRef != "origin/main" {
			t.Fatalf("RemoteRef = %q, want origin/main", outcome.RemoteRef)
		}
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published != "" {
			t.Fatalf("no push was needed, but origin/%s was created (%q)", branch, published)
		}
	})

	// The town's own guard, installed in the fixture: a push of a branch from a
	// checkout standing on another one is refused. A preserve push that runs in
	// the seat's worktree, whose HEAD is the branch, gets through (gt-7cps5).
	t.Run("PreserveBranchBeforeNukePushesThroughTheOffBranchGuard", func(t *testing.T) {
		t.Parallel()
		repo, remote, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)
		installOffBranchGuard(t, repo)
		tip := gitNow(t, repo, "rev-parse", "refs/heads/"+branch)

		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", t.TempDir())
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if outcome.AlreadyThere {
			t.Fatal("AlreadyThere = true, want false: the branch was never published")
		}
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); !strings.HasPrefix(published, tip) {
			t.Fatalf("origin/%s = %q, want the tip %s", branch, published, tip)
		}
	})

	// The seat's worktree is on main (a reset or a reused seat) and no other
	// checkout stands on the branch. The push then comes from a temporary
	// worktree of the branch: it runs the same guard, and passes it because that
	// checkout's HEAD is the branch. Without one, this push is refused and the
	// branch cannot be preserved (gt-7cps5).
	t.Run("PreserveBranchBeforeNukePushesFromATemporaryWorktree", func(t *testing.T) {
		t.Parallel()
		repo, remote, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)
		installOffBranchGuard(t, repo)
		runGitCmd(t, repo, "checkout", "main")
		tip := gitNow(t, repo, "rev-parse", "refs/heads/"+branch)

		workRoot := t.TempDir()
		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", workRoot)
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if outcome.AlreadyThere {
			t.Fatal("AlreadyThere = true, want false: the branch was never published")
		}
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); !strings.HasPrefix(published, tip) {
			t.Fatalf("origin/%s = %q, want the tip %s", branch, published, tip)
		}
		if left := gitNow(t, repo, "worktree", "list", "--porcelain"); strings.Contains(left, workRoot) {
			t.Fatalf("the temporary worktree was left registered:\n%s", left)
		}
	})

	// The rig's real shape (gt-7cps5): a bare .repo.git whose HEAD is main, and
	// the seat's worktree checked out from it. Nuke opens the bare repository
	// with NewGitWithDir; its own commands carry --git-dir, so a push from that
	// handle stands on main and the guard refuses it. The seat's worktree stands
	// on the branch, and that is where the push has to run.
	t.Run("PreserveBranchBeforeNukePushesFromTheSeatWorktreeOfABareRigRepo", func(t *testing.T) {
		t.Parallel()
		bare, seat, remote, branch := setupPreserveRig(t)
		installOffBranchGuard(t, bare)
		requireOffBranchPushRefused(t, bare, branch)
		g := git.NewGitWithDir(bare, "")
		if head := gitNow(t, bare, "symbolic-ref", "--short", "HEAD"); head != "main" {
			t.Fatalf("fixture is wrong: the bare repository's HEAD is %q, want main", head)
		}
		tip := gitNow(t, bare, "rev-parse", "refs/heads/"+branch)
		workRoot := t.TempDir()

		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", workRoot)
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if outcome.AlreadyThere {
			t.Fatal("AlreadyThere = true, want false: the branch was never published")
		}
		if published := gitNow(t, bare, "ls-remote", "--heads", remote, branch); !strings.HasPrefix(published, tip) {
			t.Fatalf("origin/%s = %q, want the tip %s", branch, published, tip)
		}
		if head := gitNow(t, seat, "symbolic-ref", "--short", "HEAD"); head != branch {
			t.Fatalf("the seat's worktree is on %s, want %s left as it was", head, branch)
		}
		if left := gitNow(t, bare, "worktree", "list", "--porcelain"); strings.Contains(left, workRoot) {
			t.Fatalf("a temporary worktree was added under %s; the seat's checkout stands on the branch:\n%s", workRoot, left)
		}
	})

	// The same bare repository with its seat reset to main: no checkout stands on
	// the branch, so the push comes from a temporary worktree of it, made from the
	// .repo.git handle and removed afterwards.
	t.Run("PreserveBranchBeforeNukeAddsATemporaryWorktreeInABareRigRepo", func(t *testing.T) {
		t.Parallel()
		bare, seat, remote, branch := setupPreserveRig(t)
		installOffBranchGuard(t, bare)
		requireOffBranchPushRefused(t, bare, branch)
		runGitCmd(t, seat, "checkout", "main")
		g := git.NewGitWithDir(bare, "")
		tip := gitNow(t, bare, "rev-parse", "refs/heads/"+branch)
		// preserveWorkRoot's real shape: the rig's .runtime, beside .repo.git.
		workRoot := filepath.Join(filepath.Dir(bare), ".runtime", "preserve")

		outcome, err := preserveBranchBeforeNuke(g, branch, "origin", workRoot)
		if err != nil {
			t.Fatalf("preserveBranchBeforeNuke: %v", err)
		}
		if outcome.AlreadyThere {
			t.Fatal("AlreadyThere = true, want false: the branch was never published")
		}
		if published := gitNow(t, bare, "ls-remote", "--heads", remote, branch); !strings.HasPrefix(published, tip) {
			t.Fatalf("origin/%s = %q, want the tip %s", branch, published, tip)
		}
		if left := gitNow(t, bare, "worktree", "list", "--porcelain"); strings.Contains(left, workRoot) {
			t.Fatalf("the temporary worktree was left registered:\n%s", left)
		}
	})

	// With the branch tip reachable only locally and a remote that accepts no push,
	// there is nowhere safe for the work to go. The preserve step must say so
	// instead of letting the caller delete the branch.
	t.Run("PreserveBranchBeforeNukeFailsClosedWhenNoRemoteRefCanHoldTheTip", func(t *testing.T) {
		t.Parallel()
		repo, _, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)

		// Fetch keeps working; every push fails.
		runGitCmd(t, repo, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "gone.git"))

		_, err := preserveBranchBeforeNuke(g, branch, "origin", t.TempDir())
		if !errors.Is(err, git.ErrBranchNotPreserved) {
			t.Fatalf("err = %v, want it to wrap git.ErrBranchNotPreserved", err)
		}
	})

	// The sling-rollback path never pushes, so it must not claim preservation. A
	// tip with no remote copy stays local rather than being deleted.
	t.Run("DeletePolecatBranchKeepsLocalBranchWithoutRemoteCopy", func(t *testing.T) {
		t.Parallel()
		repo, _, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)
		// Production deletes the branch from the bare repo, after the worktree that
		// had it checked out is gone; check out main so git allows the delete.
		runGitCmd(t, repo, "checkout", "main")

		deletePolecatBranch(branch, g, false)

		if out := gitNow(t, repo, "branch", "--list", branch); out == "" {
			t.Fatal("local branch was deleted even though no remote ref contains its tip")
		}
	})

	t.Run("DeletePolecatBranchDeletesWhenWorkIsOnRemote", func(t *testing.T) {
		t.Parallel()
		repo, remote, branch := setupPreserveRepo(t)
		g := git.NewGit(repo)
		runGitCmd(t, repo, "push", "origin", branch)
		runGitCmd(t, repo, "checkout", "main")

		deletePolecatBranch(branch, g, false)

		if out := gitNow(t, repo, "branch", "--list", branch); out != "" {
			t.Fatalf("local branch %s should be deleted once origin holds the work", branch)
		}
		if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published == "" {
			t.Fatalf("origin/%s should still hold the work", branch)
		}
	})
}

// installOffBranchGuard installs the refusal the town's pre-push hook carries,
// in the hooks directory git reads for repo: a push of a branch from a checkout
// standing on another one is refused. The directory is asked of git rather than
// assumed to be <repo>/.git/hooks, which a bare repository does not have — a
// hook written there is never run, and the bare-rig cases below would pass
// without exercising the guard at all (gt-7cps5).
//
// A copy of the real hook would drag in the town's role checks, which this
// fixture has no identity for; the rule under test is the HEAD-mismatch one.
func installOffBranchGuard(t *testing.T, repo string) {
	t.Helper()
	hooks := gitNow(t, repo, "rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(hooks) {
		// git prints the local repository's hooks directory relative to the
		// repository root.
		hooks = filepath.Join(repo, hooks)
	}
	hook := filepath.Join(hooks, "pre-push")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatalf("make the hooks directory: %v", err)
	}
	body := "#!/bin/sh\n" +
		`head=$(git symbolic-ref --short -q HEAD || echo "")` + "\n" +
		"while read local_ref local_sha remote_ref remote_sha; do\n" +
		`  pushed=${local_ref#refs/heads/}` + "\n" +
		`  if [ -n "$head" ] && [ "$head" != "$pushed" ]; then` + "\n" +
		`    echo "BLOCKED: pushing '$pushed' while HEAD is on '$head'." >&2` + "\n" +
		"    exit 1\n" +
		"  fi\n" +
		"done\n"
	if err := os.WriteFile(hook, []byte(body), 0o755); err != nil {
		t.Fatalf("write the pre-push hook: %v", err)
	}
}

// requireOffBranchPushRefused proves installOffBranchGuard's hook is live for
// repo: a push of branch attempted from this checkout (whose HEAD is another
// branch) is refused by the guard before any ref is written. The probe targets
// a throwaway ref, so a guard that failed to install is the only way it can
// write anything.
func requireOffBranchPushRefused(t *testing.T, repo, branch string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "push", "origin", branch+":refs/heads/guard-probe")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the off-branch guard did not refuse a push of %s from %s:\n%s", branch, repo, out)
	}
	if !strings.Contains(string(out), "BLOCKED") {
		t.Fatalf("the push of %s from %s failed for a reason other than the guard:\n%s", branch, repo, out)
	}
}

// gitNow runs git and returns trimmed stdout, failing the test on error.
func gitNow(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// setupPreserveRepo creates a repo with a bare origin and a polecat branch one
// commit ahead of origin/main, unpublished. It returns the repo path, the bare
// remote path, and the branch name.
func setupPreserveRepo(t *testing.T) (repo, remote, branch string) {
	t.Helper()
	p := cachedGitFixtureStrings(t, "setupPreserveRepo", func(dir string) []string {
		repo, remote, branch := buildSetupPreserveRepo(t, dir)
		return []string{repo, remote, branch}
	})
	return p[0], p[1], p[2]
}

// setupPreserveRig builds the repository shape a nuke actually finds: a bare
// .repo.git cloned from the remote, with the seat's worktree made from it and
// the polecat branch one unpublished commit ahead. It returns the bare
// repository, the seat's worktree, the bare remote, and the branch.
func setupPreserveRig(t *testing.T) (bare, seat, remote, branch string) {
	t.Helper()
	p := cachedGitFixtureStrings(t, "setupPreserveRig", func(dir string) []string {
		bare, seat, remote, branch := buildSetupPreserveRig(t, dir)
		return []string{bare, seat, remote, branch}
	})
	return p[0], p[1], p[2], p[3]
}

// buildSetupPreserveRig makes setupPreserveRig's repos under dir.
func buildSetupPreserveRig(t *testing.T, dir string) (bare, seat, remote, branch string) {
	t.Helper()
	remote = filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, "", "init", seed)
	runGitCmd(t, seed, "config", "user.email", "test@example.com")
	runGitCmd(t, seed, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(seed, "README.md"), "base\n")
	runGitCmd(t, seed, "add", "README.md")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "branch", "-M", "main")
	runGitCmd(t, seed, "remote", "add", "origin", remote)
	runGitCmd(t, seed, "push", "-u", "origin", "main")
	// A push leaves the remote's HEAD alone; without this the bare clone below
	// inherits the init default and the rig's default branch would not be main.
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	// The rig's bare repository, and the seat's worktree checked out from it on
	// the polecat's branch — the shape nuke opens with NewGitWithDir.
	bare = filepath.Join(dir, ".repo.git")
	runGitCmd(t, "", "clone", "--bare", remote, bare)
	branch = "polecat/test/gt-7cps5+mu745h79"
	seat = filepath.Join(dir, "polecats", "test", "gastown")
	runGitCmd(t, bare, "worktree", "add", "-b", branch, seat, "main")
	runGitCmd(t, seat, "config", "user.email", "test@example.com")
	runGitCmd(t, seat, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(seat, "work.txt"), "work\n")
	runGitCmd(t, seat, "add", "work.txt")
	runGitCmd(t, seat, "commit", "-m", "polecat work")
	return bare, seat, remote, branch
}

// buildSetupPreserveRepo makes setupPreserveRepo's repos under dir.
func buildSetupPreserveRepo(t *testing.T, dir string) (repo, remote, branch string) {
	t.Helper()
	remote = filepath.Join(dir, "remote.git")
	repo = filepath.Join(dir, "repo")
	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, "", "init", repo)
	runGitCmd(t, repo, "config", "user.email", "test@example.com")
	runGitCmd(t, repo, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(repo, "README.md"), "base\n")
	runGitCmd(t, repo, "add", "README.md")
	runGitCmd(t, repo, "commit", "-m", "base")
	runGitCmd(t, repo, "branch", "-M", "main")
	runGitCmd(t, repo, "remote", "add", "origin", remote)
	runGitCmd(t, repo, "push", "-u", "origin", "main")

	branch = "polecat/test/gt-yxys+mu745h79"
	runGitCmd(t, repo, "checkout", "-b", branch)
	writeTestFile(t, filepath.Join(repo, "work.txt"), "work\n")
	runGitCmd(t, repo, "add", "work.txt")
	runGitCmd(t, repo, "commit", "-m", "polecat work")
	return repo, remote, branch
}
