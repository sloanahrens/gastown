package version

import "testing"

// buildCheckoutDir is a path directly under the build-checkout base, where
// buildsrc.Ensure puts a rig's job-owned checkout. buildCheckoutBare is the bare
// repo that checkout clones from, the rig's .repo.git beside its directory.
const (
	buildCheckoutDir  = "/fake/town/.runtime/build-src/gastown"
	buildCheckoutBare = "/fake/town/gastown/.repo.git"
)

// newBuildCheckout makes a checkout at dir whose origin is the rig's bare repo,
// with main at c1 then c2 pushed to it, so the bare main and the checkout's
// origin/main are both c2. HEAD is detached at c2. It returns the fake and the
// two commits.
func newBuildCheckout(t *testing.T, dir string) (g *fakeGit, r *fakeRepo, c1, c2 string) {
	t.Helper()
	g = newFakeGit(t)
	r = g.repo(dir)
	g.repo(buildCheckoutBare)
	r.addRemote("origin", buildCheckoutBare)
	c1 = r.commit("a.go", "1")
	c2 = r.commit("b.go", "2")
	r.push("origin", "main", "main")
	r.head = ""
	r.detach = c2
	return g, r, c1, c2
}

// TestCheckStaleBuildCheckoutOnMainFollowsTheDetachedHead pins OnMainBranch for
// a build checkout: a detached HEAD is on main at main's tip and at an ancestor
// of it; past the tip, and on another branch, it is not.
func TestCheckStaleBuildCheckoutOnMainFollowsTheDetachedHead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		move     func(r *fakeRepo, c1, c2 string)
		wantMain bool
	}{
		{name: "detached at main's tip is on main", move: func(*fakeRepo, string, string) {}, wantMain: true},
		{
			name:     "detached at an ancestor of main is on main",
			move:     func(r *fakeRepo, c1, _ string) { r.detach = c1 },
			wantMain: true,
		},
		{
			name: "detached past main's tip is off main",
			move: func(r *fakeRepo, _, _ string) { r.commit("off.go", "3") },
		},
		{
			name: "checkout on another branch is off main",
			move: func(r *fakeRepo, _, _ string) { r.checkoutNew("feature") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, r, c1, c2 := newBuildCheckout(t, buildCheckoutDir)
			tc.move(r, c1, c2)

			info := g.checker(c2).checkStale(buildCheckoutDir)
			if info.Error != nil {
				t.Fatalf("checkStale: %v", info.Error)
			}
			if info.OnMainBranch != tc.wantMain {
				t.Fatalf("OnMainBranch = %v, want %v (CompareRef %q)", info.OnMainBranch, tc.wantMain, info.CompareRef)
			}
		})
	}
}

// TestCheckStaleDetachedOutsideBuildCheckoutKeepsItsAnswer pins that a detached
// repoDir which is not under the build-checkout base is not read as on main,
// even at main's tip: the answer it always had.
func TestCheckStaleDetachedOutsideBuildCheckoutKeepsItsAnswer(t *testing.T) {
	t.Parallel()
	g, _, _, c2 := newBuildCheckout(t, repoDir)

	info := g.checker(c2).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("checkStale: %v", info.Error)
	}
	if info.OnMainBranch {
		t.Fatalf("a detached checkout outside the build-checkout base reads as on main (CompareRef %q)", info.CompareRef)
	}
}

// TestCheckStaleBuildCheckoutIsJudgedAgainstTheBareMain pins the ref a detached
// build checkout is read against: the bare repo's main, not the checkout's
// cached origin/main. Here the cached ref is behind the bare main, so a HEAD at
// the bare main is on main only if the bare main is what is read.
func TestCheckStaleBuildCheckoutIsJudgedAgainstTheBareMain(t *testing.T) {
	t.Parallel()
	g, r, c1, c2 := newBuildCheckout(t, buildCheckoutDir)
	r.setRef("refs/remotes/origin/main", c1)

	info := g.checker(c2).checkStale(buildCheckoutDir)
	if info.Error != nil {
		t.Fatalf("checkStale: %v", info.Error)
	}
	if !info.OnMainBranch || info.RepoCommit != c2 {
		t.Fatalf("OnMainBranch = %v, RepoCommit %s; want on main at the bare main %s", info.OnMainBranch, ShortCommit(info.RepoCommit), ShortCommit(c2))
	}
}

// TestCheckStaleBuildCheckoutWhenItsMainOrHeadIsUnreadableIsOffMain pins the
// refusals: a build checkout whose bare repo has no main, or whose HEAD names no
// commit, is not on main, even at a commit the cached origin/main would name.
func TestCheckStaleBuildCheckoutWhenItsMainOrHeadIsUnreadableIsOffMain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		breaks func(g *fakeGit, c2 string)
	}{
		{
			name:   "the bare repo has no main",
			breaks: func(g *fakeGit, _ string) { delete(g.repos[buildCheckoutBare].refs, "refs/heads/main") },
		},
		{
			name: "HEAD names no commit",
			breaks: func(g *fakeGit, _ string) {
				g.repos[buildCheckoutDir].detach = "0000000000000000000000000000000000000000"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, _, _, c2 := newBuildCheckout(t, buildCheckoutDir)
			tc.breaks(g, c2)

			info := g.checker(c2).checkStale(buildCheckoutDir)
			if info.Error != nil {
				t.Fatalf("checkStale: %v", info.Error)
			}
			if info.OnMainBranch {
				t.Fatalf("a build checkout with an unreadable main or HEAD reads as on main (CompareRef %q)", info.CompareRef)
			}
		})
	}
}

// TestCheckStaleBinaryFreshReadsTheBareMainWithoutFetching pins the live read on
// a build checkout: the verdict comes from the bare repo's main and the checkout
// runs no fetch to get there. A fresh verdict is the case a live refresh would
// run for, so it is the one that proves the absence; a stale one is the case
// where the bare main has moved on.
func TestCheckStaleBinaryFreshReadsTheBareMainWithoutFetching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		moveBare  bool
		wantStale bool
	}{
		{name: "binary at the bare main is fresh", wantStale: false},
		{name: "binary behind a moved bare main is stale", moveBare: true, wantStale: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, r, _, c2 := newBuildCheckout(t, buildCheckoutDir)
			if tc.moveBare {
				g.repos[buildCheckoutBare].commit("d.go", "4")
			}

			info := g.checker(c2).checkStaleFresh(buildCheckoutDir)
			if info.Error != nil || info.Skipped {
				t.Fatalf("checkStaleFresh: error %v, skipped %q", info.Error, info.SkipReason)
			}
			if info.IsStale != tc.wantStale {
				t.Fatalf("IsStale = %v, want %v (CompareRef %q)", info.IsStale, tc.wantStale, info.CompareRef)
			}
			if fetches := r.fetches(); len(fetches) != 0 {
				t.Fatalf("checkStaleFresh fetched %v for a build checkout, want none", fetches)
			}
		})
	}
}
