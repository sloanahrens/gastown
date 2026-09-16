package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

var errPlaceholder = errors.New("lookup failed")

type fakeCloseTimeCommitCounter struct {
	count int
	err   error
}

func (f fakeCloseTimeCommitCounter) CommitsAhead(base, branch string) (int, error) {
	return f.count, f.err
}

type fakeCloseTimeMRTracker struct {
	issues map[string]*beads.Issue
	err    error
}

func (f fakeCloseTimeMRTracker) Show(id string) (*beads.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.issues[id], nil
}

// TestCloseTimeInvariantSkipReason_ZeroCommitsAllowed covers gt-6hmz exit
// (a): a branch with zero commits the target lacks may always close, even
// with no MR and no override reason.
func TestCloseTimeInvariantSkipReason_ZeroCommitsAllowed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 0}
	tracker := fakeCloseTimeMRTracker{} // no pending MR

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected close allowed on zero commits ahead, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_AllowsCloseWhenOpenMRTracksIssue covers
// gt-6hmz exit (b) — this is the "gt done's own close passes because the MR
// exists" case: real unmerged commits, but the pending MR bead is open.
func TestCloseTimeInvariantSkipReason_AllowsCloseWhenOpenMRTracksIssue(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 3}
	tracker := fakeCloseTimeMRTracker{issues: map[string]*beads.Issue{
		"gt-wisp-mr1": {ID: "gt-wisp-mr1", Status: "open"},
	}}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "gt-wisp-mr1", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected close allowed when the pending MR is open, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_AllowsCloseWhenMRClaimedInProgress covers
// the refinery-claim race (gt-6hmz om-editorial finding 3): the refinery
// transitions an MR open -> in_progress the moment it claims it
// (internal/refinery/types.go:183), between MR creation and this close
// check. in_progress must still count as "tracking", not "gone".
func TestCloseTimeInvariantSkipReason_AllowsCloseWhenMRClaimedInProgress(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 3}
	tracker := fakeCloseTimeMRTracker{issues: map[string]*beads.Issue{
		"gt-wisp-mr1": {ID: "gt-wisp-mr1", Status: "in_progress"},
	}}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "gt-wisp-mr1", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected close allowed when the pending MR was claimed in_progress, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_RefusesWhenPendingMRClosed ensures a
// pendingMRID pointing at an already-terminal MR (closed/tombstone) does not
// count as tracking — that MR no longer protects the unmerged commits.
func TestCloseTimeInvariantSkipReason_RefusesWhenPendingMRClosed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 3}
	tracker := fakeCloseTimeMRTracker{issues: map[string]*beads.Issue{
		"gt-wisp-mr1": {ID: "gt-wisp-mr1", Status: "closed"},
	}}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "gt-wisp-mr1", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got == "" {
		t.Fatal("expected refusal when the pending MR is already closed")
	}
}

// TestCloseTimeInvariantSkipReason_SupersedePrefixAllowed covers gt-6hmz
// exit (c): an explicit operator override bypasses the git/MR checks
// entirely, even when they would otherwise refuse.
func TestCloseTimeInvariantSkipReason_SupersedePrefixAllowed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 9} // would refuse on its own
	tracker := fakeCloseTimeMRTracker{}             // no pending MR — would refuse on its own

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "", "polecat/basalt/gt-6hmz+abc", "main", "supersede: folded into gt-6hmz attempt 5")
	if got != "" {
		t.Errorf("expected supersede: override to allow close, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_CancelPrefixAllowed mirrors the
// supersede test for the "cancel:" override spelling.
func TestCloseTimeInvariantSkipReason_CancelPrefixAllowed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 9}
	tracker := fakeCloseTimeMRTracker{}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "", "polecat/basalt/gt-6hmz+abc", "main", "Cancel: work no longer needed")
	if got != "" {
		t.Errorf("expected cancel: override to allow close, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_RefusedWhenNoneHold is the refusal case:
// real unmerged commits, no pending MR, no override reason. The refusal
// message must name the branch and the exact unmerged commit count so a
// human reviewing the skip warning knows what to look at.
func TestCloseTimeInvariantSkipReason_RefusedWhenNoneHold(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 5}
	tracker := fakeCloseTimeMRTracker{}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got == "" {
		t.Fatal("expected close to be refused when no exit condition holds")
	}
	if !strings.Contains(got, "polecat/basalt/gt-6hmz+abc") {
		t.Errorf("refusal message missing branch name: %q", got)
	}
	if !strings.Contains(got, "5") {
		t.Errorf("refusal message missing unmerged commit count: %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_MRLookupErrorTreatedAsNotTracking ensures
// a lookup failure for exit (b) does not silently allow the close — an
// error resolving the pending MR must not be treated the same as it being
// open.
func TestCloseTimeInvariantSkipReason_MRLookupErrorTreatedAsNotTracking(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 2}
	tracker := fakeCloseTimeMRTracker{err: errPlaceholder}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "gt-wisp-mr1", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got == "" {
		t.Fatal("expected refusal when the pending MR lookup itself fails, not a silent allow")
	}
}

// TestCloseTimeInvariantSkipReason_CommitsAheadErrorFailsOpen documents the
// deliberate fail-open behavior when branch state itself can't be
// determined (e.g. git error): the check declines to block an otherwise
// unrelated close on an inconclusive read, matching every other skip-reason
// helper's contract in this file.
func TestCloseTimeInvariantSkipReason_CommitsAheadErrorFailsOpen(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{err: errPlaceholder}
	tracker := fakeCloseTimeMRTracker{}

	got := closeTimeInvariantSkipReason(tracker, counter, "gt-6hmz", "", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected fail-open when commit-ahead count is inconclusive, got skip reason %q", got)
	}
}

// TestHasOperatorOverridePrefix pins the exact set of accepted prefixes.
func TestHasOperatorOverridePrefix(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"supersede: dup of gt-1234", true},
		{"cancel: no longer needed", true},
		{"  Supersede: leading whitespace", true},
		{"done", false},
		{"", false},
		{"superseded by gt-1234", false}, // no colon after the word — not the prefix form
	}
	for _, tc := range cases {
		if got := hasOperatorOverridePrefix(tc.reason); got != tc.want {
			t.Errorf("hasOperatorOverridePrefix(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

// TestDoneCloseTimeInvariantSkipReason_ZeroCommitsAgainstRealGit exercises
// the branch/target resolution wrapper against a real git repo (rather than
// fakes), confirming it correctly identifies "zero commits ahead" — exit
// (a) — without needing a live bd server: aheadCount==0 short-circuits
// before the wrapper's beads client is ever asked about the pending MR.
func TestDoneCloseTimeInvariantSkipReason_ZeroCommitsAgainstRealGit(t *testing.T) {
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

	bd := beads.New(dir)
	// townRoot/rigName don't resolve to a real rig config, so the wrapper
	// falls back to defaultBranch "main" — which matches the repo above.
	got := doneCloseTimeInvariantSkipReason(bd, dir, filepath.Join(dir, "no-such-town"), "no-such-rig", "gt-6hmz", "")
	if got != "" {
		t.Errorf("expected close allowed with zero commits ahead of main, got skip reason %q", got)
	}
}

// TestDoneCloseTimeInvariantSkipReason_OnDefaultBranchAllowsClose confirms
// the wrapper doesn't try to compare a branch against itself when the
// current branch IS the rig's default branch (e.g. a non-polecat context).
func TestDoneCloseTimeInvariantSkipReason_OnDefaultBranchAllowsClose(t *testing.T) {
	dir := t.TempDir()
	testRunGit(t, dir, "init", "-b", "main")
	testRunGit(t, dir, "config", "user.email", "test@test.com")
	testRunGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, dir, "add", ".")
	testRunGit(t, dir, "commit", "-m", "initial")

	bd := beads.New(dir)
	got := doneCloseTimeInvariantSkipReason(bd, dir, filepath.Join(dir, "no-such-town"), "no-such-rig", "gt-6hmz", "")
	if got != "" {
		t.Errorf("expected close allowed when on the default branch, got skip reason %q", got)
	}
}

// TestDoneCloseTimeInvariantSkipReason_StaleLocalMainAllowsZeroCommitClose
// is the real-git regression test for gt-6hmz om-editorial finding 1: a
// polecat worktree is created from origin/<default> (internal/polecat/
// manager.go:798), so the shared .repo.git's local "main" is routinely
// behind origin/main. A branch with zero commits the polecat authored (a
// no_merge/review_only/report-only close) must still be allowed to close
// even though local main..branch is nonzero — the wrapper must compare
// against origin/main, not local main. This must fail before the fix (it
// compared bare "main") and pass after.
func TestDoneCloseTimeInvariantSkipReason_StaleLocalMainAllowsZeroCommitClose(t *testing.T) {
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

	bd := beads.New(work)
	got := doneCloseTimeInvariantSkipReason(bd, work, filepath.Join(tmp, "no-such-town"), "no-such-rig", "gt-6hmz", "")
	if got != "" {
		t.Errorf("expected close allowed comparing against origin/main (zero polecat commits), got skip reason %q — wrapper likely compared against stale local main instead", got)
	}
}
