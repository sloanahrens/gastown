package version

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The staleness tests run against fakeGit (fakegit_test.go): an in-memory
// repository per test, so they spawn no git and run in parallel. The same
// behaviour against real git is pinned in stale_integration_test.go.

const repoDir = "/fake/gastown"

// newRepo returns a fake git holding one empty repository at repoDir on the
// unborn branch main.
func newRepo(t *testing.T) (*fakeGit, *fakeRepo) {
	t.Helper()
	g := newFakeGit(t)
	return g, g.repo(repoDir)
}

// TestSetCommit writes the package variable Commit, so it is not parallel.
func TestSetCommit(t *testing.T) {
	original := Commit
	t.Cleanup(func() { SetCommit(original) })

	SetCommit("abc123def456")
	if Commit != "abc123def456" {
		t.Errorf("SetCommit did not set Commit; got %q", Commit)
	}
}

func TestCheckStaleBinary_NoCommit(t *testing.T) {
	t.Parallel()
	g, _ := newRepo(t)

	info := g.checker("").checkStale(repoDir)
	if info.Error == nil || !strings.Contains(info.Error.Error(), "dev build") {
		t.Fatalf("Error = %v, want the unstamped-build error", info.Error)
	}
	if info.IsStale || info.Skipped {
		t.Errorf("IsStale=%v Skipped=%v, want neither without a binary commit", info.IsStale, info.Skipped)
	}
}

func TestCheckStaleBinary_NotAGitRepo(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	c1 := r.commit("a.go", "1")

	info := g.checker(c1).checkStale("/fake/not-a-repo")
	if info.Error == nil || !strings.Contains(info.Error.Error(), "not a git worktree") {
		t.Fatalf("Error = %v, want not a git worktree", info.Error)
	}
}

// TestCheckStaleBinary_FeatureBranchBinaryAtMainTip is the GH#4034 regression:
// the resolved worktree is on a feature branch but the binary is at the main
// tip. Before the fix this falsely reported "N commits behind"; now it must be
// reported as not stale (compared against main, not the feature HEAD).
func TestCheckStaleBinary_FeatureBranchBinaryAtMainTip(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.commit("a.go", "1")
	mainTip := r.commit("b.go", "2")
	r.checkoutNew("feat/x")
	r.commit("c.go", "unmerged feature work")

	info := g.checker(mainTip).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if info.Skipped {
		t.Fatalf("expected not skipped (main resolvable), got skip: %s", info.SkipReason)
	}
	if info.IsStale {
		t.Errorf("binary is at main tip on a feature branch — must NOT be stale (GH#4034)")
	}
	if info.OnMainBranch {
		t.Errorf("OnMainBranch should be false on feat/x")
	}
	if info.CompareRef != "main" {
		t.Errorf("CompareRef = %q, want \"main\"", info.CompareRef)
	}
	if info.RepoCommit != mainTip {
		t.Errorf("RepoCommit = %q, want main tip %q", info.RepoCommit, mainTip)
	}
}

// TestCheckStaleBinary_FeatureBranchBinaryBehindMain: on a feature branch with
// a binary genuinely behind main — must still be reported stale, counted
// against main (not the feature HEAD).
func TestCheckStaleBinary_FeatureBranchBinaryBehindMain(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	old := r.commit("a.go", "1")
	r.commit("b.go", "2")
	mainTip := r.commit("c.go", "3")
	r.checkoutNew("feat/x")
	r.commit("d.go", "feature work")

	info := g.checker(old).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.IsStale {
		t.Fatalf("binary behind main must be stale")
	}
	if info.CompareRef != "main" {
		t.Errorf("CompareRef = %q, want \"main\"", info.CompareRef)
	}
	if info.RepoCommit != mainTip {
		t.Errorf("RepoCommit = %q, want main tip %q", info.RepoCommit, mainTip)
	}
	if info.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want 2 (counted against main)", info.CommitsBehind)
	}
	if !info.IsForward {
		t.Errorf("IsForward should be true (binary is ancestor of main)")
	}
	if info.OnMainBranch {
		t.Errorf("OnMainBranch should be false on feat/x")
	}
}

// TestCheckStaleBinary_OnMainBehind: on a build branch, behind HEAD — the
// pre-existing behavior must be unchanged (compare against HEAD/the branch).
func TestCheckStaleBinary_OnMainBehind(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	old := r.commit("a.go", "1")
	tip := r.commit("b.go", "2")

	info := g.checker(old).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.OnMainBranch {
		t.Fatalf("OnMainBranch should be true on main")
	}
	if !info.IsStale {
		t.Fatalf("binary behind main HEAD must be stale")
	}
	if info.CompareRef != "main" {
		t.Errorf("CompareRef = %q, want \"main\"", info.CompareRef)
	}
	if info.RepoCommit != tip {
		t.Errorf("RepoCommit = %q, want %q", info.RepoCommit, tip)
	}
	if info.CommitsBehind != 1 {
		t.Errorf("CommitsBehind = %d, want 1", info.CommitsBehind)
	}
}

// TestCheckStaleBinary_ShortBinaryCommitMatchesFullTip: the Makefile stamps a
// short hash, and a short hash at the tip is current, not stale.
func TestCheckStaleBinary_ShortBinaryCommitMatchesFullTip(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.commit("a.go", "1")
	tip := r.commit("b.go", "2")

	info := g.checker(tip[:9]).checkStale(repoDir)
	if info.Error != nil || info.Skipped {
		t.Fatalf("Error=%v Skipped=%v (%s), want a verdict", info.Error, info.Skipped, info.SkipReason)
	}
	if info.IsStale {
		t.Errorf("short hash of the tip must not be stale")
	}
}

// TestCheckStaleBinary_BeadsOnlyAdvanceIsNotStale is GH#2596: bd backup
// commits touch only .beads/ and do not change the binary.
func TestCheckStaleBinary_BeadsOnlyAdvanceIsNotStale(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	built := r.commit("a.go", "1")
	r.commit(".beads/issues.jsonl", "backup 1")
	r.commit(".beads/issues.jsonl", "backup 2")

	info := g.checker(built).checkStale(repoDir)
	if info.Error != nil || info.Skipped {
		t.Fatalf("Error=%v Skipped=%v, want a verdict", info.Error, info.Skipped)
	}
	if info.IsStale {
		t.Errorf("a build ref advanced only by .beads/ commits must not be stale")
	}

	r.commit("b.go", "real change")
	if info := g.checker(built).checkStale(repoDir); !info.IsStale {
		t.Errorf("a source change on top of the backups must be stale")
	}
}

// TestCheckStaleBinary_BinaryAheadOfMainIsNotForward: a binary built from a
// commit the build ref does not contain is stale but must not be rebuilt
// "forward" onto the older ref.
func TestCheckStaleBinary_BinaryAheadOfMainIsNotForward(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.commit("a.go", "1")
	r.checkoutNew("side")
	ahead := r.commit("b.go", "2")
	r.checkout("main")

	info := g.checker(ahead).checkStale(repoDir)
	if !info.IsStale {
		t.Fatalf("binary off the build ref must be stale, got %+v", info)
	}
	if info.IsForward {
		t.Errorf("IsForward = true, want false: main does not contain the binary commit")
	}
}

// TestCheckStaleBinary_OnMainBranchLocalTipMatchesStaleBinary is the gt-h8s8
// regression: RIG_ROOT's local main hasn't been pulled in a while, and the
// binary was itself built from that same stale local tip — so binary and
// local HEAD match exactly. Comparing against origin/main (already fetched
// ahead) must catch it.
func TestCheckStaleBinary_OnMainBranchLocalTipMatchesStaleBinary(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	g.repo("/fake/origin")
	r.addRemote("origin", "/fake/origin")
	localTip := r.commit("a.go", "1")
	r.push("origin", "main", "main")
	// origin/main advances further without this worktree's local main
	// following it.
	remoteTip := r.commit("b.go", "2")
	r.push("origin", "main", "main")
	r.setRef("refs/heads/main", localTip)

	info := g.checker(localTip).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.OnMainBranch {
		t.Fatalf("OnMainBranch should be true on main")
	}
	if info.CompareRef != "origin/main" {
		t.Errorf("CompareRef = %q, want \"origin/main\" (stale local main must not win just because it matches the binary)", info.CompareRef)
	}
	if !info.IsStale {
		t.Fatalf("binary matching only the stale local tip, with origin/main ahead, must be reported stale")
	}
	if info.RepoCommit != remoteTip {
		t.Errorf("RepoCommit = %q, want origin/main tip %q", info.RepoCommit, remoteTip)
	}
}

// TestCheckStaleBinary_OnMainBranchPrefersUpstreamTracking: with both remotes
// tracking the branch, upstream is the reference.
func TestCheckStaleBinary_OnMainBranchPrefersUpstreamTracking(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	built := r.commit("a.go", "1")
	upstreamTip := r.commit("b.go", "2")
	r.setRef("refs/remotes/origin/main", built)
	r.setRef("refs/remotes/upstream/main", upstreamTip)

	info := g.checker(built).checkStale(repoDir)
	if info.CompareRef != "upstream/main" || info.RepoCommit != upstreamTip {
		t.Errorf("CompareRef=%q RepoCommit=%q, want upstream/main at %s", info.CompareRef, info.RepoCommit, upstreamTip)
	}
	if !info.IsStale {
		t.Errorf("binary behind upstream/main must be stale")
	}
}

// TestCheckStaleBinary_OnMainBranchStaleLocalRefPrefersOrigin: on main, but
// the local branch pointer was never fast-forwarded past a commit that
// predates the binary. The stale local ref must not be treated as ground
// truth when a fresher build-branch ref (origin/main) already contains the
// binary commit (gt-ugo).
func TestCheckStaleBinary_OnMainBranchStaleLocalRefPrefersOrigin(t *testing.T) {
	t.Parallel()
	g, r := staleLocalMainFixture(t)
	freshTip := r.refs["refs/remotes/origin/main"]

	info := g.checker(freshTip).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.OnMainBranch {
		t.Fatalf("OnMainBranch should be true on main")
	}
	if info.CompareRef != "origin/main" {
		t.Errorf("CompareRef = %q, want \"origin/main\" (stale local main must not win)", info.CompareRef)
	}
	if info.RepoCommit != freshTip {
		t.Errorf("RepoCommit = %q, want origin/main tip %q", info.RepoCommit, freshTip)
	}
	if info.IsStale {
		t.Errorf("binary at origin/main tip must not be reported stale")
	}
}

// TestCheckStaleBinary_OnMainNoRemoteLocalLagsFallsBackToBuildRef: on main
// with no remote-tracking ref, a local main that predates the binary is
// replaced by the freshest build-branch ref containing it (gt-ugo).
func TestCheckStaleBinary_OnMainNoRemoteLocalLagsFallsBackToBuildRef(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	old := r.commit("a.go", "1")
	fresh := r.commit("b.go", "2")
	r.branch("carry/ops")
	r.setRef("refs/heads/main", old)

	info := g.checker(fresh).checkStale(repoDir)
	if info.CompareRef != "carry/ops" || info.RepoCommit != fresh {
		t.Errorf("CompareRef=%q RepoCommit=%q, want carry/ops at %s", info.CompareRef, info.RepoCommit, fresh)
	}
	if info.IsStale {
		t.Errorf("binary at the carry tip must not be stale")
	}
}

// TestCheckStaleBinary_NoBuildBranchSkips: feature branch, no main/master/
// carry/remote — the check must skip rather than diff against feature HEAD.
func TestCheckStaleBinary_NoBuildBranchSkips(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	c1 := r.commit("a.go", "1")
	r.renameBranch("feature/only")

	info := g.checker(c1).checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.Skipped {
		t.Fatalf("expected Skipped when no build-branch ref exists")
	}
	if info.SkipReason == "" {
		t.Errorf("SkipReason should be set when skipped")
	}
	if info.IsStale {
		t.Errorf("IsStale must be false when skipped")
	}
	if info.OnMainBranch {
		t.Errorf("OnMainBranch must be false on feature/only")
	}
}

func TestCheckStaleBinary_BinaryCommitMissingSkips(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.commit("a.go", "1")

	info := g.checker("ffffffffffffffffffffffffffffffffffffffff").checkStale(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.Skipped {
		t.Fatalf("expected missing binary commit to skip")
	}
	if !strings.Contains(info.SkipReason, "binary commit not found") {
		t.Errorf("SkipReason = %q, want binary commit not found", info.SkipReason)
	}
	if info.IsStale {
		t.Errorf("IsStale must be false when binary commit is missing")
	}
}

// staleLocalMainFixture is a repo on main whose local branch pointer lags a
// cached refs/remotes/origin/main one commit ahead of it. No origin remote is
// configured; callers add one when they need it.
func staleLocalMainFixture(t *testing.T) (*fakeGit, *fakeRepo) {
	t.Helper()
	g, r := newRepo(t)
	staleTip := r.commit("a.go", "1")
	freshTip := r.commit("b.go", "2")
	r.setRef("refs/heads/main", staleTip)
	r.setRef("refs/remotes/origin/main", freshTip)
	return g, r
}

// TestCheckStaleBinaryFresh_RefreshesLaggingOriginMain is the gt-cq0
// regression. CheckStaleBinary trusts repoDir's cached refs/remotes/origin/main
// exactly as of its last fetch. If the binary happens to match that stale
// cache, the check reports "fresh" even though the real origin/main has since
// moved on. CheckStaleBinaryFresh must refresh the ref first and catch it.
func TestCheckStaleBinaryFresh_RefreshesLaggingOriginMain(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	origin := g.repo("/fake/origin")
	r.addRemote("origin", "/fake/origin")
	r.commit("a.go", "1")
	r.push("origin", "main", "main")

	// Another checkout lands a commit, and this one fetches it into its
	// cached origin/main without moving local main. The binary is built from
	// that commit.
	midTip := origin.commit("b.go", "2")
	if err := r.fetch("origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	// A further commit lands after this checkout's last fetch.
	newTip := origin.commit("c.go", "3")

	c := g.checker(midTip)
	stale := c.checkStale(repoDir)
	if stale.Error != nil {
		t.Fatalf("unexpected error: %v", stale.Error)
	}
	if stale.IsStale {
		t.Fatalf("setup invariant broken: expected checkStale to (wrongly) report fresh against the stale cache")
	}

	fresh := c.checkStaleFresh(repoDir)
	if fresh.Error != nil {
		t.Fatalf("unexpected error: %v", fresh.Error)
	}
	if fresh.Skipped {
		t.Fatalf("expected a definite stale verdict, not a skip: %s", fresh.SkipReason)
	}
	if !fresh.IsStale {
		t.Fatalf("checkStaleFresh must detect staleness after refreshing origin/main to %s (binary built from %s)",
			ShortCommit(newTip), ShortCommit(midTip))
	}
	if fresh.CompareRef != "origin/main" {
		t.Errorf("CompareRef = %q, want \"origin/main\"", fresh.CompareRef)
	}
	if fresh.RepoCommit != newTip {
		t.Errorf("RepoCommit = %q, want refreshed origin/main tip %q", fresh.RepoCommit, newTip)
	}
}

// TestCheckStaleBinaryFresh_NoRemoteFailsClosedInsteadOfFresh: a cached
// refs/remotes/origin/main with no "origin" remote configured to verify it
// against. checkStale trusts the cache and reports fresh; checkStaleFresh has
// nothing to confirm that ref with, so per gt-cq0 it must fail closed to
// Skipped instead of repeating an unverified "fresh" claim.
func TestCheckStaleBinaryFresh_NoRemoteFailsClosedInsteadOfFresh(t *testing.T) {
	t.Parallel()
	g, r := staleLocalMainFixture(t)

	info := g.checker(r.refs["refs/remotes/origin/main"]).checkStaleFresh(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if info.IsStale {
		t.Errorf("must not claim IsStale from an unverifiable ref either — Skipped is the correct fail-closed state")
	}
	if !info.Skipped {
		t.Fatalf("expected fail-closed Skipped when no real origin remote exists to confirm origin/main; got a trusted verdict instead")
	}
	if !strings.Contains(info.SkipReason, "origin/main") {
		t.Errorf("SkipReason = %q, want it to name origin/main", info.SkipReason)
	}
	if got := r.fetches(); len(got) != 0 {
		t.Errorf("fetches = %v, want none: no remote is configured", got)
	}
}

// TestCheckStaleBinaryFresh_ConfirmedFreshIsNotSkipped proves the fail-closed
// path added for gt-cq0 doesn't over-block: when a reachable remote confirms
// the binary is genuinely at the tip, the result must be a clean "not stale"
// — not a skip.
func TestCheckStaleBinaryFresh_ConfirmedFreshIsNotSkipped(t *testing.T) {
	t.Parallel()
	g, r := staleLocalMainFixture(t)
	freshTip := r.refs["refs/remotes/origin/main"]
	g.repo("/fake/origin")
	r.addRemote("origin", "/fake/origin")
	r.push("origin", freshTip, "main")

	info := g.checker(freshTip).checkStaleFresh(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if info.Skipped {
		t.Fatalf("must not fail closed when the live remote confirms the binary is genuinely fresh: %s", info.SkipReason)
	}
	if info.IsStale {
		t.Errorf("binary at the confirmed remote tip must not be reported stale")
	}
	if info.CompareRef != "origin/main" {
		t.Errorf("CompareRef = %q, want \"origin/main\"", info.CompareRef)
	}
	want := []string{"fetch", "--quiet", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"}
	fetched := false
	for _, f := range r.fetches() {
		fetched = fetched || eq(f, want...)
	}
	if !fetched {
		t.Errorf("fetches = %v, want %v among them", r.fetches(), want)
	}
}

// TestCheckStaleBinaryFresh_UnreachableRemoteFailsClosed: origin is
// configured but unreachable. The stale local cache says "fresh";
// checkStaleFresh must not trust it.
func TestCheckStaleBinaryFresh_UnreachableRemoteFailsClosed(t *testing.T) {
	t.Parallel()
	g, r := staleLocalMainFixture(t)
	r.addRemote("origin", "/fake/does-not-exist")

	info := g.checker(r.refs["refs/remotes/origin/main"]).checkStaleFresh(repoDir)
	if info.Error != nil {
		t.Fatalf("unexpected error: %v", info.Error)
	}
	if !info.Skipped {
		t.Fatalf("expected fail-closed Skipped when origin is configured but unreachable; got a trusted verdict instead")
	}
	if info.IsStale {
		t.Errorf("must not claim IsStale from an unverifiable ref either — Skipped is the correct fail-closed state")
	}
}

// TestCheckStaleBinaryFresh_StaleFromCacheSkipsTheFetch: drift the cached ref
// already proves needs no network (gt-vxjz).
func TestCheckStaleBinaryFresh_StaleFromCacheSkipsTheFetch(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	origin := g.repo("/fake/origin")
	r.addRemote("origin", "/fake/origin")
	builtFrom := r.commit("a.go", "1")
	r.push("origin", "main", "main")

	fetchedTip := origin.commit("b.go", "2")
	if err := r.fetch("origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	// A later tip that only a live fetch would learn about.
	origin.commit("c.go", "3")

	info := g.checker(builtFrom).checkStaleFresh(repoDir)
	if !info.IsStale {
		t.Fatalf("cached origin/main is %s and the binary is %s; want stale",
			ShortCommit(fetchedTip), ShortCommit(builtFrom))
	}
	if info.Skipped {
		t.Errorf("Skipped = true (%s), want the verdict the cached ref already supports", info.SkipReason)
	}
	if info.RepoCommit != fetchedTip {
		t.Errorf("RepoCommit = %s, want the cached %s", ShortCommit(info.RepoCommit), ShortCommit(fetchedTip))
	}
	if got := r.fetches(); len(got) != 0 {
		t.Errorf("fetches = %v, want none: a stale-from-cache verdict must not fetch", got)
	}
}

// TestCheckStaleBinaryFresh_UnstampedBuildSkipsTheFetch: a build with no
// commit has no verdict to make fresh, so it must not reach the network. This
// is the path that made gt formula sync non-hermetic (gt-vxjz).
func TestCheckStaleBinaryFresh_UnstampedBuildSkipsTheFetch(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	g.repo("/fake/origin")
	r.addRemote("origin", "/fake/origin")
	r.commit("a.go", "1")
	r.push("origin", "main", "main")

	info := g.checker("").checkStaleFresh(repoDir)
	if info.Error == nil {
		t.Fatalf("want the unstamped-build error, got IsStale=%v Skipped=%v", info.IsStale, info.Skipped)
	}
	if got := r.fetches(); len(got) != 0 {
		t.Errorf("fetches = %v, want none: an unstamped build must not fetch", got)
	}
}

func TestResolveBuildBranchRef(t *testing.T) {
	t.Parallel()

	t.Run("prefers carry when same commit is on carry and main", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		c1 := r.commit("a.go", "1")
		r.branch("carry/operational")
		r.checkoutNew("feat/x")
		ref, ok := g.checker(c1).resolveBuildBranchRef(repoDir, c1)
		if !ok || ref.display != "carry/operational" || ref.ref != "refs/heads/carry/operational" || ref.commit != c1 {
			t.Errorf("got (%+v,%v), want carry/operational at %s", ref, ok, c1)
		}
	})

	t.Run("routes to carry when binary not on main", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		r.commit("a.go", "1")
		r.checkoutNew("carry/operational")
		carryOnly := r.commit("b.go", "fork work")
		r.checkoutNew("feat/x")
		ref, ok := g.checker(carryOnly).resolveBuildBranchRef(repoDir, carryOnly)
		if !ok || ref.display != "carry/operational" || ref.commit != carryOnly {
			t.Errorf("got (%+v,%v), want carry/operational at %s", ref, ok, carryOnly)
		}
	})

	t.Run("ambiguous carry is skipped", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		c1 := r.commit("a.go", "1")
		r.renameBranch("feature/only")
		r.branch("carry/a")
		r.branch("carry/b")
		if ref, ok := g.checker(c1).resolveBuildBranchRef(repoDir, c1); ok {
			t.Errorf("got (%+v,%v), want no ref for ambiguous carry/*", ref, ok)
		}
	})

	t.Run("falls back to origin/main", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		c1 := r.commit("a.go", "1")
		r.renameBranch("feature/only")
		r.setRef("refs/remotes/origin/main", c1)
		ref, ok := g.checker(c1).resolveBuildBranchRef(repoDir, c1)
		if !ok || ref.display != "origin/main" || ref.ref != "refs/remotes/origin/main" || ref.commit != c1 {
			t.Errorf("got (%+v,%v), want origin/main at %s", ref, ok, c1)
		}
	})

	t.Run("fresher remote beats stale local main", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		old := r.commit("a.go", "1")
		fresh := r.commit("b.go", "2")
		r.setRef("refs/remotes/origin/main", fresh)
		r.setRef("refs/heads/main", old)
		r.checkoutNew("feat/x")

		ref, ok := g.checker(old).resolveBuildBranchRef(repoDir, old)
		if !ok || ref.display != "origin/main" || ref.commit != fresh {
			t.Errorf("got (%+v,%v), want fresher origin/main at %s", ref, ok, fresh)
		}
	})

	t.Run("prefers upstream over divergent origin", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		base := r.commit("a.go", "1")
		r.checkoutNew("origin-line")
		originTip := r.commit("origin.go", "origin")
		r.setRef("refs/remotes/origin/main", originTip)
		r.checkout("main")
		r.checkoutNew("upstream-line")
		upstreamTip := r.commit("upstream.go", "upstream")
		r.setRef("refs/remotes/upstream/main", upstreamTip)
		r.checkout("main")
		r.checkoutNew("feat/x")

		ref, ok := g.checker(base).resolveBuildBranchRef(repoDir, base)
		if !ok || ref.display != "upstream/main" || ref.commit != upstreamTip {
			t.Errorf("got (%+v,%v), want upstream/main at %s", ref, ok, upstreamTip)
		}
	})

	t.Run("uses remote carry when local carry absent", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		c1 := r.commit("a.go", "1")
		r.renameBranch("feature/only")
		r.setRef("refs/remotes/origin/carry/operational", c1)

		ref, ok := g.checker(c1).resolveBuildBranchRef(repoDir, c1)
		if !ok || ref.display != "origin/carry/operational" || ref.ref != "refs/remotes/origin/carry/operational" || ref.commit != c1 {
			t.Errorf("got (%+v,%v), want origin/carry/operational at %s", ref, ok, c1)
		}
	})

	t.Run("fully qualified remote ref resists local branch shadow", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		old := r.commit("a.go", "1")
		r.renameBranch("feature/only")
		fresh := r.commit("b.go", "2")
		r.setRef("refs/heads/origin/main", old)
		r.setRef("refs/remotes/origin/main", fresh)

		ref, ok := g.checker(old).resolveBuildBranchRef(repoDir, old)
		if !ok || ref.display != "origin/main" || ref.ref != "refs/remotes/origin/main" || ref.commit != fresh {
			t.Errorf("got (%+v,%v), want remote origin/main at %s", ref, ok, fresh)
		}
	})

	t.Run("binary on no candidate finds nothing", func(t *testing.T) {
		t.Parallel()
		g, r := newRepo(t)
		r.commit("a.go", "1")
		r.checkoutNew("feat/x")
		orphan := r.commit("b.go", "2")
		if ref, ok := g.checker(orphan).resolveBuildBranchRef(repoDir, orphan); ok {
			t.Errorf("got (%+v,%v), want no ref when no build branch contains the binary", ref, ok)
		}
	})
}

func TestShortCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		hash   string
		expect string
	}{
		{"full SHA", "abcdef1234567890abcdef1234567890abcdef12", "abcdef123456"},
		{"exactly 12", "abcdef123456", "abcdef123456"},
		{"short hash", "abcdef", "abcdef"},
		{"empty", "", ""},
		{"13 chars", "abcdef1234567", "abcdef123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ShortCommit(tt.hash)
			if got != tt.expect {
				t.Errorf("ShortCommit(%q) = %q, want %q", tt.hash, got, tt.expect)
			}
		})
	}
}

func TestCommitsMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		a, b   string
		expect bool
	}{
		{"identical full", "abcdef1234567890", "abcdef1234567890", true},
		{"prefix match short-long", "abcdef1234567", "abcdef1234567890abcd", true},
		{"prefix match long-short", "abcdef1234567890abcd", "abcdef1234567", true},
		{"no match", "abcdef1234567", "1234567abcdef", false},
		{"too short a", "abc", "abcdef1234567", false},
		{"too short b", "abcdef1234567", "abc", false},
		{"both too short", "abc", "abc", false},
		{"exactly 7 chars match", "abcdefg", "abcdefg", true},
		{"exactly 7 chars no match", "abcdefg", "abcdefh", false},
		{"6 chars too short", "abcdef", "abcdef", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := commitsMatch(tt.a, tt.b)
			if got != tt.expect {
				t.Errorf("commitsMatch(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.expect)
			}
		})
	}
}

func TestIsBuildBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		branch string
		want   bool
	}{
		{"main", true},
		{"master", true},
		{"carry/operational", true},
		{"carry/staging", true},
		{"carry/", true},
		{"fix/something", false},
		{"feat/new-thing", false},
		{"develop", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			t.Parallel()
			if got := isBuildBranch(tt.branch); got != tt.want {
				t.Errorf("isBuildBranch(%q) = %v, want %v", tt.branch, got, tt.want)
			}
		})
	}
}

func TestStaleBinaryInfo_Describe(t *testing.T) {
	t.Parallel()
	const (
		bin  = "abc1234567890def"
		repo = "fed0987654321cba"
	)
	tests := []struct {
		name    string
		info    StaleBinaryInfo
		subject string
		want    string
	}{
		{
			name:    "commits behind known",
			info:    StaleBinaryInfo{BinaryCommit: bin, RepoCommit: repo, CompareRef: "main", CommitsBehind: 3},
			subject: "Binary",
			want:    "Binary is 3 commits behind main (built from abc123456789, main at fed098765432)",
		},
		{
			name:    "count unknown falls back to stale wording",
			info:    StaleBinaryInfo{BinaryCommit: bin, RepoCommit: repo, CompareRef: "origin/main", CommitsBehind: 0},
			subject: "gt binary",
			want:    "gt binary is stale (built from abc123456789, origin/main at fed098765432)",
		},
		{
			name:    "short hashes are not truncated",
			info:    StaleBinaryInfo{BinaryCommit: "abc123", RepoCommit: "def456", CompareRef: "carry/ops", CommitsBehind: 1},
			subject: "Binary",
			want:    "Binary is 1 commits behind carry/ops (built from abc123, carry/ops at def456)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.Describe(tt.subject); got != tt.want {
				t.Errorf("Describe(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}

// errNotBuilt stands in for the "cannot determine binary commit" error.
var errNotBuilt = errors.New("cannot determine binary commit (dev build?)")

// TestLiveRefVerdict pins the gate that decides whether CheckStaleBinaryFresh
// goes to the network. Only a "current" verdict resting on a remote-tracking
// ref can be changed by a fetch; everything else is returned as-is, so a
// routine caller does not rewrite the shared checkout's refs for nothing
// (gt-vxjz).
func TestLiveRefVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		info *StaleBinaryInfo
		want bool
	}{
		{"current, judged against a remote-tracking ref", &StaleBinaryInfo{CompareRef: "origin/main"}, true},
		{"current, judged against a local branch", &StaleBinaryInfo{CompareRef: "main"}, false},
		{"current, judged against a carry branch", &StaleBinaryInfo{CompareRef: "carry/ops"}, false},
		{"stale from the cached ref", &StaleBinaryInfo{CompareRef: "origin/main", IsStale: true}, false},
		{"no verdict (skipped)", &StaleBinaryInfo{CompareRef: "origin/main", Skipped: true}, false},
		{"no verdict (error)", &StaleBinaryInfo{CompareRef: "origin/main", Error: errNotBuilt}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := liveRefVerdict(tt.info); got != tt.want {
				t.Errorf("liveRefVerdict(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

// TestGetRepoRootForTown_ReadsOnlyTheTown keeps a caller reporting one town's
// build drift from resolving — and, to report freshness, fetching — whichever
// checkout the process happens to sit near (gt-vxjz).
func TestGetRepoRootForTown_ReadsOnlyTheTown(t *testing.T) {
	t.Parallel()
	// GetRepoRootForTown reads no environment and no working directory; the
	// town root is its only input.
	empty := t.TempDir()
	if _, err := GetRepoRootForTown(empty); err == nil {
		t.Error("resolved a checkout for a town with no gastown/ below it")
	}

	tests := []struct {
		name string
		rel  string
	}{
		{"canonical", "gastown"},
		{"mayor's rig worktree", filepath.Join("gastown", "mayor", "rig")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			source := filepath.Join(town, tt.rel)
			main := filepath.Join(source, "cmd", "gt", "main.go")
			if err := os.MkdirAll(filepath.Dir(main), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(main, []byte("package main\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := GetRepoRootForTown(town)
			if err != nil {
				t.Fatalf("GetRepoRootForTown: %v", err)
			}
			if got != source {
				t.Errorf("GetRepoRootForTown = %q, want %q", got, source)
			}
		})
	}
}
