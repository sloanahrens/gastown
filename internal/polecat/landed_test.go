package polecat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// landedRepo is a clone with an origin behind it, in a gitfake world, so
// probeWorkLandedOnRef is measured against modeled git state: the question it
// answers ("is this work already in the integration branch") is a git
// question, and the merge-tree no-op arm only exists because squash merges
// leave no ancestry to follow. gitfake's contract pins the verdicts it uses.
type landedRepo struct {
	w      *world
	work   string
	origin string
}

func initLandedRepo(t *testing.T) landedRepo {
	t.Helper()
	root := t.TempDir()
	r := landedRepo{w: newWorld(), work: filepath.Join(root, "work"), origin: filepath.Join(root, "origin")}
	r.w.InitBare(t, r.origin)
	r.w.Commit(t, r.origin, "main", "initial", map[string]string{"README.md": "# Test\n"})
	r.w.Clone(t, r.origin, r.work)
	return r
}

func (r landedRepo) g() gitRepo { return r.w.repo(r.work) }

func (r landedRepo) do(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (r landedRepo) commit(t *testing.T, name, content, message string) {
	t.Helper()
	r.w.writeAndCommit(t, r.work, message, map[string]string{name: content})
}

func (r landedRepo) probe(branch string) LandedEvidence {
	return probeWorkLandedOnRef(r.w.opener(), r.work, branch, "origin")
}

// startLandedBranch creates and pushes a polecat branch carrying one change.
func (r landedRepo) startLandedBranch(t *testing.T, branch string) {
	t.Helper()
	r.do(t, r.g().CheckoutNewBranch(branch, "HEAD"))
	r.commit(t, "feature.txt", "the fix\n", "the fix")
	r.do(t, r.g().Push("origin", branch, false))
}

// landOnMain checks out main, lands branch on it with merge and pushes main.
func (r landedRepo) landOnMain(t *testing.T, branch string, merge func(gitfake.BranchRepo) error) {
	t.Helper()
	r.do(t, r.g().Checkout("main"))
	r.do(t, merge(r.w.OpenBranchRepo(r.work)))
	r.do(t, r.g().Push("origin", "main", false))
}

func noFF(branch string) func(gitfake.BranchRepo) error {
	return func(g gitfake.BranchRepo) error { return g.MergeNoFF(branch, "merge polecat work") }
}

func squash(branch string) func(gitfake.BranchRepo) error {
	return func(g gitfake.BranchRepo) error { return g.MergeSquash(branch, "squash polecat work") }
}

func ffOnly(branch string) func(gitfake.BranchRepo) error {
	return func(g gitfake.BranchRepo) error { return g.ResetHard(branch) }
}

// TestProbeWorkLandedOnRef covers the evidence the dangling-active_mr gate
// rests on: work that is already in origin/main (by merge or by squash) is
// landed, everything else — including every way of not knowing — is not.
func TestProbeWorkLandedOnRef(t *testing.T) {
	t.Parallel()
	const branch = "polecat/opal/gt-eoi9+mu8i3jnq"

	t.Run("merged branch is landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, noFF(branch))

		got := repo.probe(branch)
		if !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified", got)
		}
		if got.Ref != "origin/main" {
			t.Fatalf("Ref = %q, want origin/main", got.Ref)
		}
	})

	t.Run("squash-merged branch is landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, squash(branch))

		// The squash leaves no ancestry to follow: this is the arm that makes
		// a finished polecat's slot reclaimable at all.
		got := repo.probe(branch)
		if !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified for a squash-merged branch", got)
		}
	})

	t.Run("branch with an unmerged commit is not landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)

		if got := repo.probe(branch); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for unmerged work", got)
		}
	})

	t.Run("probe answers about the submitted tip, not the local branch", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, squash(branch))

		// The submitted tip is on main; the local branch has since grown a
		// commit that is on neither. That leftover is the reuse gate's business
		// (it overlays preservation against the polecat's remote branch onto
		// UnpushedCommits), not this probe's — asking the local ref here would
		// report "unpreserved" about an MR that demonstrably landed, which is
		// the stranding this fix exists to remove (gt-wprt's opal and topaz).
		repo.do(t, repo.g().Checkout(branch))
		repo.commit(t, "left-behind.txt", "unpushed\n", "not pushed anywhere")

		if got := repo.probe(branch); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified: the submitted tip landed", got)
		}
	})

	t.Run("branch deleted from origin after landing is landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, squash(branch))
		repo.w.DeleteRef(t, repo.origin, "refs/heads/"+branch)

		// The usual post-merge cleanup: the submitted ref is gone, so the local
		// branch stands in — and it is on main.
		if got := repo.probe(branch); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified after the submitted branch was deleted", got)
		}
	})

	t.Run("local branch landed by a non-squash merge then deleted is landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, noFF(branch))
		repo.w.DeleteRef(t, repo.origin, "refs/heads/"+branch)

		// A --no-ff merge leaves the branch tip an ancestor of integration, so
		// the ancestry arm alone cannot tell it from a ref that was never
		// committed to. It is distinguishable by how integration reached it:
		// the merge, not integration's own first-parent line. Losing this case
		// is the false negative the divergence guard was rejected for.
		if got := repo.probe(branch); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified for a --no-ff merge whose remote branch was deleted", got)
		}
	})

	t.Run("local branch landed by a non-squash merge, integration advances, then deleted is landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, noFF(branch))
		repo.w.DeleteRef(t, repo.origin, "refs/heads/"+branch)

		// Integration keeps moving after the merge landed, the ordinary case
		// since a landed check typically runs well after other work has
		// continued to land on main. head is now two hops behind integration's
		// tip on its first-parent line, not one: a check that only inspects
		// ref^ would find head is not that single immediate parent and wrongly
		// conclude head sits off the line, reintroducing the false negative
		// this probe exists to fix.
		repo.commit(t, "later.txt", "later main work\n", "later main commit")
		repo.do(t, repo.g().Push("origin", "main", false))

		if got := repo.probe(branch); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified for a --no-ff merge once integration has advanced further", got)
		}
	})

	t.Run("local branch fast-forwarded then deleted is not landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, ffOnly(branch))
		repo.w.DeleteRef(t, repo.origin, "refs/heads/"+branch)

		// A fast-forward leaves the tip equal to integration's tip — byte for
		// byte the state a branch that was created and never committed to is
		// in. This asserts the fail-closed answer for that unresolvable case
		// rather than leaving it to chance; refCarriesOwnWork says why.
		if got := repo.probe(branch); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified: a fast-forward landing is indistinguishable from an empty branch", got)
		}
	})

	t.Run("remote branch that never carried a commit is not landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		// Pushed, but at integration's own tip with nothing of its own — a
		// branch created from origin/main and left there. The remote-tracking
		// ref is therefore a trivial ancestor of integration, and the guard
		// has to apply to it too, not just to the local fallback.
		repo.do(t, repo.g().Push("origin", "main:"+branch, false))

		if got := repo.probe(branch); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for a pushed branch with no commits of its own", got)
		}
	})

	t.Run("empty local branch is not landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		// Created and never committed to, so the ref sits exactly on
		// integration and every preservation arm finds it trivially preserved.
		// This is the fail-open the probe must not take (gt-7pec): the local
		// branch stands in for a submitted branch, and this one submitted
		// nothing.
		repo.do(t, repo.g().CheckoutNewBranch(branch, "HEAD"))

		if got := repo.probe(branch); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for a branch with no commits of its own", got)
		}
	})

	t.Run("stale local branch on integration's own history is not landed", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		// A ref left at an earlier commit of main and never advanced: an
		// ancestor of integration because it is main's own history, which is
		// not this branch's work.
		head, _ := repo.g().Rev("HEAD")
		repo.w.SetRef(t, repo.work, "refs/heads/"+branch, head)
		repo.commit(t, "unrelated.txt", "unrelated main work\n", "unrelated main commit")
		repo.do(t, repo.g().Push("origin", "main", false))

		if got := repo.probe(branch); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified for a stale branch with no commits of its own", got)
		}
	})

	t.Run("dirty worktree does not change the answer", func(t *testing.T) {
		t.Parallel()
		repo := initLandedRepo(t)
		repo.startLandedBranch(t, branch)
		repo.landOnMain(t, branch, squash(branch))
		if err := os.WriteFile(filepath.Join(repo.work, "README.md"), []byte("# Unrelated leftovers\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		// The landed question is about committed work; uncommitted leftovers of
		// their own are the reuse gate's business, not this probe's.
		if got := repo.probe(branch); !got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want verified despite a dirty worktree", got)
		}
	})

	t.Run("unknown state fails closed", func(t *testing.T) {
		t.Parallel()
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
				t.Parallel()
				if got := probeWorkLandedOnRef(repo.w.opener(), tc.path, tc.branch, "origin"); got.Verified {
					t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified", got)
				}
			})
		}
	})

	t.Run("no origin remote fails closed", func(t *testing.T) {
		t.Parallel()
		w := newWorld()
		dir := t.TempDir() // local-only repo: no origin, no origin/main
		w.InitRepo(t, dir)
		w.Commit(t, dir, "main", "initial", map[string]string{"README.md": "# Test\n"})
		if got := probeWorkLandedOnRef(w.opener(), dir, "main", "origin"); got.Verified {
			t.Fatalf("ProbeWorkLandedOnRef = %+v, want unverified without an origin", got)
		}
	})
}
