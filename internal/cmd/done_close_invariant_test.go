package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

var errPlaceholder = errors.New("lookup failed")

type fakeCloseTimeCommitCounter struct {
	count int
	err   error
}

func (f fakeCloseTimeCommitCounter) CommitsAhead(base, branch string) (int, error) {
	return f.count, f.err
}

type fakeCloseTimeMRFinder struct {
	mrs []*beads.Issue
	err error
}

func (f fakeCloseTimeMRFinder) FindOpenMRsForIssue(issueID string) ([]*beads.Issue, error) {
	return f.mrs, f.err
}

// TestCloseTimeInvariantSkipReason_ZeroCommitsAllowed covers gt-6hmz exit
// (a): a branch with zero commits the target lacks may always close, even
// with no MR and no override reason.
func TestCloseTimeInvariantSkipReason_ZeroCommitsAllowed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 0}
	mrFinder := fakeCloseTimeMRFinder{} // no open MRs

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected close allowed on zero commits ahead, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_AllowsCloseWhenOpenMRTracksIssue covers
// gt-6hmz exit (b) — this is the "gt done's own close passes because the MR
// exists" case: real unmerged commits, but an open MR bead tracks them.
func TestCloseTimeInvariantSkipReason_AllowsCloseWhenOpenMRTracksIssue(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 3}
	mrFinder := fakeCloseTimeMRFinder{mrs: []*beads.Issue{{ID: "gt-wisp-mr1", Status: "open"}}}

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected close allowed when an open MR tracks the issue, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_SupersedePrefixAllowed covers gt-6hmz
// exit (c): an explicit operator override bypasses the git/MR checks
// entirely, even when they would otherwise refuse.
func TestCloseTimeInvariantSkipReason_SupersedePrefixAllowed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 9} // would refuse on its own
	mrFinder := fakeCloseTimeMRFinder{}             // no open MRs — would refuse on its own

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "supersede: folded into gt-6hmz attempt 5")
	if got != "" {
		t.Errorf("expected supersede: override to allow close, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_CancelPrefixAllowed mirrors the
// supersede test for the "cancel:" override spelling.
func TestCloseTimeInvariantSkipReason_CancelPrefixAllowed(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 9}
	mrFinder := fakeCloseTimeMRFinder{}

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "Cancel: work no longer needed")
	if got != "" {
		t.Errorf("expected cancel: override to allow close, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_RefusedWhenNoneHold is the refusal case:
// real unmerged commits, no open MR, no override reason. The refusal
// message must name the branch and the exact unmerged commit count so a
// human reviewing the skip warning knows what to look at.
func TestCloseTimeInvariantSkipReason_RefusedWhenNoneHold(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 5}
	mrFinder := fakeCloseTimeMRFinder{}

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
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

// TestCloseTimeInvariantSkipReason_MRFinderErrorTreatedAsNoOpenMR ensures a
// lookup failure for exit (b) does not silently allow the close — an error
// finding open MRs must not be treated the same as finding one.
func TestCloseTimeInvariantSkipReason_MRFinderErrorTreatedAsNoOpenMR(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{count: 2}
	mrFinder := fakeCloseTimeMRFinder{err: errPlaceholder}

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got == "" {
		t.Fatal("expected refusal when the MR lookup itself fails, not a silent allow")
	}
}

// TestCloseTimeInvariantSkipReason_CommitsAheadErrorFailsOpen documents the
// deliberate fail-open behavior when branch state itself can't be
// determined (e.g. git error): the check declines to block an otherwise
// unrelated close on an inconclusive read, matching every other skip-reason
// helper's contract in this file.
func TestCloseTimeInvariantSkipReason_CommitsAheadErrorFailsOpen(t *testing.T) {
	counter := fakeCloseTimeCommitCounter{err: errPlaceholder}
	mrFinder := fakeCloseTimeMRFinder{}

	got := closeTimeInvariantSkipReason(mrFinder, counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
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
// before the wrapper's beads client is ever asked about open MRs.
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
	got := doneCloseTimeInvariantSkipReason(bd, dir, filepath.Join(dir, "no-such-town"), "no-such-rig", "gt-6hmz")
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
	got := doneCloseTimeInvariantSkipReason(bd, dir, filepath.Join(dir, "no-such-town"), "no-such-rig", "gt-6hmz")
	if got != "" {
		t.Errorf("expected close allowed when on the default branch, got skip reason %q", got)
	}
}
