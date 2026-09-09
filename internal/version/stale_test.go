package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- git-backed test helpers ---

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitCommit writes file and creates a commit, returning its full hash.
func gitCommit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "--no-gpg-sign", "-m", "c-"+file)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	// These tests create tiny temp-dir repos and shell out to git a handful
	// of times — fast and deterministic, so they run even under -short. CI
	// runs `-short`; skipping here left stale.go's staleness logic at 0%
	// patch coverage (GH#4034 follow-up). Only skip if git is unavailable.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

// setBinaryCommit overrides the build-time commit for the duration of the test.
func setBinaryCommit(t *testing.T, c string) {
	t.Helper()
	orig := Commit
	t.Cleanup(func() { Commit = orig })
	Commit = c
}

func TestShortCommit(t *testing.T) {
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
			got := ShortCommit(tt.hash)
			if got != tt.expect {
				t.Errorf("ShortCommit(%q) = %q, want %q", tt.hash, got, tt.expect)
			}
		})
	}
}

func TestCommitsMatch(t *testing.T) {
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
			got := commitsMatch(tt.a, tt.b)
			if got != tt.expect {
				t.Errorf("commitsMatch(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.expect)
			}
		})
	}
}

func TestSetCommit(t *testing.T) {
	original := Commit
	defer func() { Commit = original }()

	SetCommit("abc123def456")
	if Commit != "abc123def456" {
		t.Errorf("SetCommit did not set Commit; got %q", Commit)
	}
}

func TestIsBuildBranch(t *testing.T) {
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
			if got := isBuildBranch(tt.branch); got != tt.want {
				t.Errorf("isBuildBranch(%q) = %v, want %v", tt.branch, got, tt.want)
			}
		})
	}
}

func TestCheckStaleBinary_NoCommit(t *testing.T) {
	original := Commit
	defer func() { Commit = original }()

	Commit = ""
	// Force resolveCommitHash to return empty by clearing Commit
	// (vcs.revision from build info may still be set, so this test
	// verifies the error path when no commit is available)
	info := CheckStaleBinary(t.TempDir())
	if info == nil {
		t.Fatal("CheckStaleBinary returned nil")
	}
	// Either we get an error (no commit) or we get a valid result from build info
	// Both are acceptable outcomes
	if info.BinaryCommit == "" && info.Error == nil {
		t.Error("expected error when binary commit is empty")
	}
}

// TestCheckStaleBinary_FeatureBranchBinaryAtMainTip is the GH#4034 regression:
// the resolved worktree is on a feature branch but the binary is at the main
// tip. Before the fix this falsely reported "N commits behind"; now it must be
// reported as not stale (compared against main, not the feature HEAD).
func TestCheckStaleBinary_FeatureBranchBinaryAtMainTip(t *testing.T) {
	dir := newGitRepo(t)
	gitCommit(t, dir, "a.go", "1")
	mainTip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "branch", "-M", "main")
	gitRun(t, dir, "checkout", "-q", "-b", "feat/x")
	gitCommit(t, dir, "c.go", "unmerged feature work")
	setBinaryCommit(t, mainTip)

	info := CheckStaleBinary(dir)
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
	dir := newGitRepo(t)
	old := gitCommit(t, dir, "a.go", "1")
	gitCommit(t, dir, "b.go", "2")
	mainTip := gitCommit(t, dir, "c.go", "3")
	gitRun(t, dir, "branch", "-M", "main")
	gitRun(t, dir, "checkout", "-q", "-b", "feat/x")
	gitCommit(t, dir, "d.go", "feature work")
	setBinaryCommit(t, old)

	info := CheckStaleBinary(dir)
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
	dir := newGitRepo(t)
	old := gitCommit(t, dir, "a.go", "1")
	tip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "branch", "-M", "main")
	setBinaryCommit(t, old)

	info := CheckStaleBinary(dir)
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

// TestCheckStaleBinary_OnMainBranchLocalTipMatchesStaleBinary is the gt-h8s8
// regression: RIG_ROOT's local main hasn't been pulled in a while, and the
// binary was itself built from that same stale local tip — so binary and
// local HEAD match exactly. The pre-fix gt-ugo fallback only triggered when
// the binary was NOT an ancestor of local HEAD, which doesn't cover this
// case (they're equal), so the old code reported "fresh" and mayor/rig's
// run.sh never pulled or rebuilt. Comparing against origin/main (already
// fetched ahead) must catch it.
func TestCheckStaleBinary_OnMainBranchLocalTipMatchesStaleBinary(t *testing.T) {
	remoteDir := newBareRemote(t)

	dir := newGitRepo(t)
	gitRun(t, dir, "remote", "add", "origin", remoteDir)
	localTip := gitCommit(t, dir, "a.go", "1")
	gitRun(t, dir, "branch", "-M", "main")
	gitRun(t, dir, "push", "-q", "origin", "main")
	// origin/main advances further (e.g. the refinery merging other work)
	// without this worktree ever fetching again.
	remoteTip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "push", "-q", "origin", "main")
	// Roll this worktree's local main back to its last-fetched tip so it no
	// longer matches origin — but the binary was built from that same tip.
	gitRun(t, dir, "reset", "-q", "--hard", localTip)
	setBinaryCommit(t, localTip)

	// Refresh the cached origin/main ref, as CheckStaleBinaryFresh would
	// before calling CheckStaleBinary — simulating a rig that fetched but
	// never fast-forwarded its local main pointer.
	gitRun(t, dir, "fetch", "-q", "origin")

	info := CheckStaleBinary(dir)
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

// TestCheckStaleBinary_OnMainBranchStaleLocalRefPrefersOrigin: on main, but
// the local branch pointer was never fast-forwarded past a commit that
// predates the binary (e.g. refinery merged to origin without updating this
// checkout). The stale local ref must not be treated as ground truth when a
// fresher build-branch ref (origin/main) already contains the binary commit
// (gt-ugo).
func TestCheckStaleBinary_OnMainBranchStaleLocalRefPrefersOrigin(t *testing.T) {
	dir := newGitRepo(t)
	staleTip := gitCommit(t, dir, "a.go", "1")
	freshTip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "branch", "-M", "main")
	// Simulate a local main pointer that lags its remote: the branch ref
	// points at staleTip even though HEAD's history already produced
	// freshTip, which is now only reachable via origin/main.
	gitRun(t, dir, "update-ref", "refs/heads/main", staleTip)
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", freshTip)
	setBinaryCommit(t, freshTip)

	info := CheckStaleBinary(dir)
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

// TestCheckStaleBinary_NoBuildBranchSkips: feature branch, no main/master/
// carry/remote — the check must skip rather than diff against feature HEAD.
func TestCheckStaleBinary_NoBuildBranchSkips(t *testing.T) {
	dir := newGitRepo(t)
	c1 := gitCommit(t, dir, "a.go", "1")
	gitRun(t, dir, "branch", "-M", "feature/only")
	setBinaryCommit(t, c1)

	info := CheckStaleBinary(dir)
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
	dir := newGitRepo(t)
	gitCommit(t, dir, "a.go", "1")
	gitRun(t, dir, "branch", "-M", "main")
	setBinaryCommit(t, "ffffffffffffffffffffffffffffffffffffffff")

	info := CheckStaleBinary(dir)
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

// newBareRemote creates an empty bare repo to stand in for a real "origin"
// remote (a local filesystem path, so pushes/fetches need no network).
func newBareRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "--bare")
	return dir
}

// TestCheckStaleBinaryFresh_RefreshesLaggingOriginMain is the gt-cq0
// regression. CheckStaleBinary trusts repoDir's cached refs/remotes/origin/main
// exactly as of its last "git fetch" — in the reported incident that was
// mayor/rig, a checkout nobody guarantees to keep fetched. If the binary
// happens to match that stale cache, the check reports "fresh" even though
// the real origin/main has since moved on. CheckStaleBinaryFresh must
// refresh the ref first and catch the discrepancy.
func TestCheckStaleBinaryFresh_RefreshesLaggingOriginMain(t *testing.T) {
	remoteDir := newBareRemote(t)

	// cloneA is the "mayor/rig"-style worktree under test: on main, but its
	// local branch pointer lags behind the commit the binary was built from.
	cloneA := newGitRepo(t)
	gitRun(t, cloneA, "remote", "add", "origin", remoteDir)
	gitCommit(t, cloneA, "a.go", "1")
	gitRun(t, cloneA, "branch", "-M", "main")
	gitRun(t, cloneA, "push", "-q", "origin", "main")

	// cloneB simulates a different checkout landing more commits on the
	// remote — the routine way origin/main moves in this town.
	cloneB := t.TempDir()
	gitRun(t, cloneB, "init", "-q")
	gitRun(t, cloneB, "remote", "add", "origin", remoteDir)
	gitRun(t, cloneB, "fetch", "-q", "origin", "main")
	gitRun(t, cloneB, "checkout", "-q", "-b", "main", "origin/main")
	midTip := gitCommit(t, cloneB, "b.go", "2")
	gitRun(t, cloneB, "push", "-q", "origin", "main")

	// cloneA fetches once, catching its cached origin/main up to midTip —
	// its own local "main" branch pointer stays at oldTip (fetch never
	// moves it). The binary was built from midTip.
	gitRun(t, cloneA, "fetch", "-q", "origin")
	setBinaryCommit(t, midTip)

	// A further commit lands on the remote after cloneA's last fetch —
	// nobody re-fetched mayor/rig, exactly the gt-cq0 scenario.
	newTip := gitCommit(t, cloneB, "c.go", "3")
	gitRun(t, cloneB, "push", "-q", "origin", "main")

	// Sanity check on the bug itself: without a refresh, the cached
	// origin/main (midTip) still matches the binary, so the unrefreshed
	// check reports the dangerous false "fresh".
	stale := CheckStaleBinary(cloneA)
	if stale.Error != nil {
		t.Fatalf("unexpected error: %v", stale.Error)
	}
	if stale.IsStale {
		t.Fatalf("setup invariant broken: expected CheckStaleBinary to (wrongly) report fresh against the stale cache")
	}

	fresh := CheckStaleBinaryFresh(cloneA)
	if fresh.Error != nil {
		t.Fatalf("unexpected error: %v", fresh.Error)
	}
	if fresh.Skipped {
		t.Fatalf("expected a definite stale verdict, not a skip: %s", fresh.SkipReason)
	}
	if !fresh.IsStale {
		t.Fatalf("CheckStaleBinaryFresh must detect staleness after refreshing origin/main to %s (binary built from %s)",
			ShortCommit(newTip), ShortCommit(midTip))
	}
	if fresh.CompareRef != "origin/main" {
		t.Errorf("CompareRef = %q, want \"origin/main\"", fresh.CompareRef)
	}
	if fresh.RepoCommit != newTip {
		t.Errorf("RepoCommit = %q, want refreshed origin/main tip %q", fresh.RepoCommit, newTip)
	}
}

// TestCheckStaleBinaryFresh_NoRemoteFailsClosedInsteadOfFresh reuses the
// TestCheckStaleBinary_OnMainBranchStaleLocalRefPrefersOrigin fixture (a
// fabricated refs/remotes/origin/main with no real "origin" remote
// configured to verify it against). CheckStaleBinary trusts the cache and
// reports fresh; CheckStaleBinaryFresh has no remote to confirm that ref
// against, so per gt-cq0 it must fail closed to Skipped instead of repeating
// an unverified "fresh" claim.
func TestCheckStaleBinaryFresh_NoRemoteFailsClosedInsteadOfFresh(t *testing.T) {
	dir := newGitRepo(t)
	staleTip := gitCommit(t, dir, "a.go", "1")
	freshTip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "branch", "-M", "main")
	gitRun(t, dir, "update-ref", "refs/heads/main", staleTip)
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", freshTip)
	setBinaryCommit(t, freshTip)

	info := CheckStaleBinaryFresh(dir)
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
}

// TestCheckStaleBinaryFresh_ConfirmedFreshIsNotSkipped proves the fail-closed
// path added for gt-cq0 doesn't over-block: when a real, reachable remote
// confirms the binary is genuinely at the tip, the result must be a clean
// "not stale" — not a skip.
func TestCheckStaleBinaryFresh_ConfirmedFreshIsNotSkipped(t *testing.T) {
	remoteDir := newBareRemote(t)

	dir := newGitRepo(t)
	gitRun(t, dir, "remote", "add", "origin", remoteDir)
	staleTip := gitCommit(t, dir, "a.go", "1")
	freshTip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "branch", "-M", "main")
	// Local main pointer lags (same shape as the "stale local ref" fixture
	// above), but here origin is a real, reachable remote genuinely at
	// freshTip — dir has not fetched it yet, so CheckStaleBinaryFresh must
	// fetch it to find that out.
	gitRun(t, dir, "update-ref", "refs/heads/main", staleTip)
	gitRun(t, dir, "push", "-q", "origin", freshTip+":refs/heads/main")
	setBinaryCommit(t, freshTip)

	info := CheckStaleBinaryFresh(dir)
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
}

// TestCheckStaleBinaryFresh_UnreachableRemoteFailsClosed: origin is
// configured but unreachable (bad path, nothing to fetch from). The stale
// local cache says "fresh"; CheckStaleBinaryFresh must not trust it.
func TestCheckStaleBinaryFresh_UnreachableRemoteFailsClosed(t *testing.T) {
	dir := newGitRepo(t)
	gitRun(t, dir, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))
	staleTip := gitCommit(t, dir, "a.go", "1")
	freshTip := gitCommit(t, dir, "b.go", "2")
	gitRun(t, dir, "branch", "-M", "main")
	gitRun(t, dir, "update-ref", "refs/heads/main", staleTip)
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", freshTip)
	setBinaryCommit(t, freshTip)

	info := CheckStaleBinaryFresh(dir)
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

func TestResolveBuildBranchRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("prefers carry when same commit is on carry and main", func(t *testing.T) {
		dir := newGitRepo(t)
		c1 := gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "main")
		gitRun(t, dir, "branch", "carry/operational")
		gitRun(t, dir, "checkout", "-q", "-b", "feat/x")
		ref, ok := resolveBuildBranchRef(dir, c1)
		if !ok || ref.display != "carry/operational" || ref.ref != "refs/heads/carry/operational" || ref.commit != c1 {
			t.Errorf("got (%+v,%v), want carry/operational at %s", ref, ok, c1)
		}
	})

	t.Run("routes to carry when binary not on main", func(t *testing.T) {
		dir := newGitRepo(t)
		gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "main")
		gitRun(t, dir, "checkout", "-q", "-b", "carry/operational")
		carryOnly := gitCommit(t, dir, "b.go", "fork work")
		gitRun(t, dir, "checkout", "-q", "-b", "feat/x")
		ref, ok := resolveBuildBranchRef(dir, carryOnly)
		if !ok || ref.display != "carry/operational" || ref.commit != carryOnly {
			t.Errorf("got (%+v,%v), want carry/operational at %s", ref, ok, carryOnly)
		}
	})

	t.Run("ambiguous carry is skipped", func(t *testing.T) {
		dir := newGitRepo(t)
		c1 := gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "feature/only")
		gitRun(t, dir, "branch", "carry/a")
		gitRun(t, dir, "branch", "carry/b")
		if ref, ok := resolveBuildBranchRef(dir, c1); ok {
			t.Errorf("got (%+v,%v), want no ref for ambiguous carry/*", ref, ok)
		}
	})

	t.Run("falls back to origin/main", func(t *testing.T) {
		dir := newGitRepo(t)
		c1 := gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "feature/only")
		gitRun(t, dir, "update-ref", "refs/remotes/origin/main", c1)
		ref, ok := resolveBuildBranchRef(dir, c1)
		if !ok || ref.display != "origin/main" || ref.ref != "refs/remotes/origin/main" || ref.commit != c1 {
			t.Errorf("got (%+v,%v), want origin/main at %s", ref, ok, c1)
		}
	})

	t.Run("fresher remote beats stale local main", func(t *testing.T) {
		dir := newGitRepo(t)
		old := gitCommit(t, dir, "a.go", "1")
		fresh := gitCommit(t, dir, "b.go", "2")
		gitRun(t, dir, "branch", "-M", "main")
		gitRun(t, dir, "update-ref", "refs/remotes/origin/main", fresh)
		gitRun(t, dir, "reset", "--hard", old)
		gitRun(t, dir, "checkout", "-q", "-b", "feat/x")

		ref, ok := resolveBuildBranchRef(dir, old)
		if !ok || ref.display != "origin/main" || ref.commit != fresh {
			t.Errorf("got (%+v,%v), want fresher origin/main at %s", ref, ok, fresh)
		}
	})

	t.Run("prefers upstream over divergent origin", func(t *testing.T) {
		dir := newGitRepo(t)
		base := gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "main")
		gitRun(t, dir, "checkout", "-q", "-b", "origin-line")
		originTip := gitCommit(t, dir, "origin.go", "origin")
		gitRun(t, dir, "update-ref", "refs/remotes/origin/main", originTip)
		gitRun(t, dir, "checkout", "-q", "main")
		gitRun(t, dir, "checkout", "-q", "-b", "upstream-line")
		upstreamTip := gitCommit(t, dir, "upstream.go", "upstream")
		gitRun(t, dir, "update-ref", "refs/remotes/upstream/main", upstreamTip)
		gitRun(t, dir, "checkout", "-q", "main")
		gitRun(t, dir, "checkout", "-q", "-b", "feat/x")

		ref, ok := resolveBuildBranchRef(dir, base)
		if !ok || ref.display != "upstream/main" || ref.commit != upstreamTip {
			t.Errorf("got (%+v,%v), want upstream/main at %s", ref, ok, upstreamTip)
		}
	})

	t.Run("uses remote carry when local carry absent", func(t *testing.T) {
		dir := newGitRepo(t)
		c1 := gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "feature/only")
		gitRun(t, dir, "update-ref", "refs/remotes/origin/carry/operational", c1)

		ref, ok := resolveBuildBranchRef(dir, c1)
		if !ok || ref.display != "origin/carry/operational" || ref.ref != "refs/remotes/origin/carry/operational" || ref.commit != c1 {
			t.Errorf("got (%+v,%v), want origin/carry/operational at %s", ref, ok, c1)
		}
	})

	t.Run("fully qualified remote ref resists local branch shadow", func(t *testing.T) {
		dir := newGitRepo(t)
		old := gitCommit(t, dir, "a.go", "1")
		gitRun(t, dir, "branch", "-M", "feature/only")
		fresh := gitCommit(t, dir, "b.go", "2")
		gitRun(t, dir, "update-ref", "refs/heads/origin/main", old)
		gitRun(t, dir, "update-ref", "refs/remotes/origin/main", fresh)

		ref, ok := resolveBuildBranchRef(dir, old)
		if !ok || ref.display != "origin/main" || ref.ref != "refs/remotes/origin/main" || ref.commit != fresh {
			t.Errorf("got (%+v,%v), want remote origin/main at %s", ref, ok, fresh)
		}
	})
}

func TestStaleBinaryInfo_Describe(t *testing.T) {
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
			if got := tt.info.Describe(tt.subject); got != tt.want {
				t.Errorf("Describe(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}
