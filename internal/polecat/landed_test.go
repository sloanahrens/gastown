package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// landedRepo is a worktree with a real origin/main behind it, so
// ProbeWorkLandedOnRef can be measured against actual git state rather than a
// fake: the question it answers ("is this work already in the integration
// branch") is a git question, and the merge-tree no-op arm only exists because
// squash merges leave no ancestry to follow.
type landedRepo struct {
	work   string
	origin string
}

func runLandedGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
}

func initLandedRepo(t *testing.T) landedRepo {
	t.Helper()
	origin := t.TempDir()
	runLandedGit(t, origin, "init", "--bare", "-b", "main")

	work := t.TempDir()
	runLandedGit(t, work, "clone", origin, ".")
	runLandedGit(t, work, "config", "user.email", "test@test.com")
	runLandedGit(t, work, "config", "user.name", "Test User")
	writeLandedFile(t, filepath.Join(work, "README.md"), "# Test\n")
	runLandedGit(t, work, "add", ".")
	runLandedGit(t, work, "commit", "-m", "initial")
	runLandedGit(t, work, "push", "origin", "main")
	return landedRepo{work: work, origin: origin}
}

func writeLandedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// startLandedBranch creates and pushes a polecat branch carrying one change.
func (r landedRepo) startLandedBranch(t *testing.T, branch string) {
	t.Helper()
	runLandedGit(t, r.work, "checkout", "-b", branch)
	writeLandedFile(t, filepath.Join(r.work, "feature.txt"), "the fix\n")
	runLandedGit(t, r.work, "add", ".")
	runLandedGit(t, r.work, "commit", "-m", "the fix")
	runLandedGit(t, r.work, "push", "origin", branch)
}

// TestProbeWorkLandedOnRef covers the evidence the dangling-active_mr gate
// rests on: work that is already in origin/main (by merge or by squash) is
// landed, everything else — including every way of not knowing — is not.
func TestProbeWorkLandedOnRef(t *testing.T) {
	const branch = "polecat/opal/gt-eoi9+mu8i3jnq"

	t.Run("merged branch is landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--no-ff", "-m", "merge polecat work", branch)
		runLandedGit(t, repo.work, "push", "origin", "main")

		got := ProbeWorkLandedOnRef(repo.work, branch, "origin")
		if !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified", got)
		}
		if got.Ref != "origin/main" {
			t.Fatalf("Ref = %q, want origin/main", got.Ref)
		}
	})

	t.Run("squash-merged branch is landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--squash", branch)
		runLandedGit(t, repo.work, "commit", "-m", "squash polecat work")
		runLandedGit(t, repo.work, "push", "origin", "main")

		// The squash leaves no ancestry to follow: this is the arm that makes
		// a finished polecat's slot reclaimable at all.
		got := ProbeWorkLandedOnRef(repo.work, branch, "origin")
		if !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified for a squash-merged branch", got)
		}
	})

	t.Run("branch with an unmerged commit is not landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)

		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for unmerged work", got)
		}
	})

	t.Run("probe answers about the submitted tip, not the local branch", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--squash", branch)
		runLandedGit(t, repo.work, "commit", "-m", "squash polecat work")
		runLandedGit(t, repo.work, "push", "origin", "main")

		// The submitted tip is on main; the local branch has since grown a
		// commit that is on neither. That leftover is the reuse gate's business
		// (it overlays preservation against the polecat's remote branch onto
		// UnpushedCommits), not this probe's — asking the local ref here would
		// report "unpreserved" about an MR that demonstrably landed, which is
		// the stranding this fix exists to remove (gt-wprt's opal and topaz).
		runLandedGit(t, repo.work, "checkout", branch)
		writeLandedFile(t, filepath.Join(repo.work, "left-behind.txt"), "unpushed\n")
		runLandedGit(t, repo.work, "add", ".")
		runLandedGit(t, repo.work, "commit", "-m", "not pushed anywhere")

		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified: the submitted tip landed", got)
		}
	})

	t.Run("branch deleted from origin after landing is landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--squash", branch)
		runLandedGit(t, repo.work, "commit", "-m", "squash polecat work")
		runLandedGit(t, repo.work, "push", "origin", "main")
		runLandedGit(t, repo.work, "push", "origin", "--delete", branch)

		// The usual post-merge cleanup: the submitted ref is gone, so the local
		// branch stands in — and it is on main.
		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified after the submitted branch was deleted", got)
		}
	})

	t.Run("local branch landed by a non-squash merge then deleted is landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--no-ff", "-m", "merge polecat work", branch)
		runLandedGit(t, repo.work, "push", "origin", "main")
		runLandedGit(t, repo.work, "push", "origin", "--delete", branch)

		// A --no-ff merge leaves the branch tip an ancestor of integration, so
		// the ancestry arm alone cannot tell it from a ref that was never
		// committed to. It is distinguishable by how integration reached it:
		// the merge, not integration's own first-parent line. Losing this case
		// is the false negative the divergence guard was rejected for.
		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified for a --no-ff merge whose remote branch was deleted", got)
		}
	})

	t.Run("local branch landed by a non-squash merge, integration advances, then deleted is landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--no-ff", "-m", "merge polecat work", branch)
		runLandedGit(t, repo.work, "push", "origin", "main")
		runLandedGit(t, repo.work, "push", "origin", "--delete", branch)

		// Integration keeps moving after the merge landed, the ordinary case
		// since a landed check typically runs well after other work has
		// continued to land on main. head is now two hops behind integration's
		// tip on its first-parent line, not one: a check that only inspects
		// ref^ would find head is not that single immediate parent and wrongly
		// conclude head sits off the line, reintroducing the false negative
		// this probe exists to fix.
		writeLandedFile(t, filepath.Join(repo.work, "later.txt"), "later main work\n")
		runLandedGit(t, repo.work, "add", ".")
		runLandedGit(t, repo.work, "commit", "-m", "later main commit")
		runLandedGit(t, repo.work, "push", "origin", "main")

		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified for a --no-ff merge once integration has advanced further", got)
		}
	})

	t.Run("local branch fast-forwarded then deleted is not landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--ff-only", branch)
		runLandedGit(t, repo.work, "push", "origin", "main")
		runLandedGit(t, repo.work, "push", "origin", "--delete", branch)

		// A fast-forward leaves the tip equal to integration's tip — byte for
		// byte the state a branch that was created and never committed to is
		// in. This asserts the fail-closed answer for that unresolvable case
		// rather than leaving it to chance; refCarriesOwnWork says why.
		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified: a fast-forward landing is indistinguishable from an empty branch", got)
		}
	})

	t.Run("remote branch that never carried a commit is not landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		// Pushed, but at integration's own tip with nothing of its own — a
		// branch created from origin/main and left there. The remote-tracking
		// ref is therefore a trivial ancestor of integration, and the guard
		// has to apply to it too, not just to the local fallback.
		runLandedGit(t, repo.work, "push", "origin", "main:"+branch)

		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for a pushed branch with no commits of its own", got)
		}
	})

	t.Run("empty local branch is not landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		// Created and never committed to, so the ref sits exactly on
		// integration and every preservation arm finds it trivially preserved.
		// This is the fail-open the probe must not take (gt-7pec): the local
		// branch stands in for a submitted branch, and this one submitted
		// nothing.
		runLandedGit(t, repo.work, "checkout", "-b", branch)

		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for a branch with no commits of its own", got)
		}
	})

	t.Run("stale local branch on integration's own history is not landed", func(t *testing.T) {
		repo := initLandedRepo(t)
		// A ref left at an earlier commit of main and never advanced: an
		// ancestor of integration because it is main's own history, which is
		// not this branch's work.
		runLandedGit(t, repo.work, "branch", branch)
		writeLandedFile(t, filepath.Join(repo.work, "unrelated.txt"), "unrelated main work\n")
		runLandedGit(t, repo.work, "add", ".")
		runLandedGit(t, repo.work, "commit", "-m", "unrelated main commit")
		runLandedGit(t, repo.work, "push", "origin", "main")

		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for a stale branch with no commits of its own", got)
		}
	})

	t.Run("dirty worktree does not change the answer", func(t *testing.T) {
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		runLandedGit(t, repo.work, "checkout", "main")
		runLandedGit(t, repo.work, "merge", "--squash", branch)
		runLandedGit(t, repo.work, "commit", "-m", "squash polecat work")
		runLandedGit(t, repo.work, "push", "origin", "main")
		writeLandedFile(t, filepath.Join(repo.work, "README.md"), "# Unrelated leftovers\n")

		// The landed question is about committed work; uncommitted leftovers of
		// their own are the reuse gate's business, not this probe's.
		if got := ProbeWorkLandedOnRef(repo.work, branch, "origin"); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified despite a dirty worktree", got)
		}
	})

	t.Run("unknown state fails closed", func(t *testing.T) {
		repo := initLandedRepo(t)

		cases := []struct {
			name   string
			path   string
			branch string
		}{
			{name: "no worktree", path: "", branch: branch},
			{name: "no branch", path: repo.work, branch: ""},
			{name: "HEAD is not a branch name", path: repo.work, branch: "HEAD"},
			{name: "worktree that does not exist", path: filepath.Join(t.TempDir(), "gone"), branch: branch},
			{name: "branch that does not exist", path: repo.work, branch: "polecat/nobody/gt-nowhere+m0"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := ProbeWorkLandedOnRef(tc.path, tc.branch, "origin"); got.Verified {
					t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified", got)
				}
			})
		}
	})

	t.Run("no origin remote fails closed", func(t *testing.T) {
		dir := initLiveGitRepo(t) // local-only repo: no origin, no origin/main
		if got := ProbeWorkLandedOnRef(dir, "main", "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified without an origin", got)
		}
	})
}
