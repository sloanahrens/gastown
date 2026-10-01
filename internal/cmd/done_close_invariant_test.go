package cmd

import (
	"errors"
	"strings"
	"testing"
)

var errPlaceholder = errors.New("lookup failed")

type fakeCloseTimeCommitCounter struct {
	count int
	err   error
}

func (f fakeCloseTimeCommitCounter) CommitsAhead(base, branch string) (int, error) {
	return f.count, f.err
}

// TestCloseTimeInvariantSkipReason_ZeroCommitsAllowed covers gt-6hmz exit
// (a): a branch with zero commits the target lacks may always close, even
// with no override reason.
func TestCloseTimeInvariantSkipReason_ZeroCommitsAllowed(t *testing.T) {
	t.Parallel()
	counter := fakeCloseTimeCommitCounter{count: 0}

	got := closeTimeInvariantSkipReason(counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected close allowed on zero commits ahead, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_SupersedePrefixAllowed covers gt-6hmz
// exit (b): an explicit operator override bypasses the git check
// entirely, even when they would otherwise refuse.
func TestCloseTimeInvariantSkipReason_SupersedePrefixAllowed(t *testing.T) {
	t.Parallel()
	counter := fakeCloseTimeCommitCounter{count: 9} // would refuse on its own

	got := closeTimeInvariantSkipReason(counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "supersede: folded into gt-6hmz attempt 5")
	if got != "" {
		t.Errorf("expected supersede: override to allow close, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_CancelPrefixAllowed mirrors the
// supersede test for the "cancel:" override spelling.
func TestCloseTimeInvariantSkipReason_CancelPrefixAllowed(t *testing.T) {
	t.Parallel()
	counter := fakeCloseTimeCommitCounter{count: 9}

	got := closeTimeInvariantSkipReason(counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "Cancel: work no longer needed")
	if got != "" {
		t.Errorf("expected cancel: override to allow close, got skip reason %q", got)
	}
}

// TestCloseTimeInvariantSkipReason_RefusedWhenNoneHold is the refusal case:
// real unmerged commits and no override reason. The refusal
// message must name the branch and the exact unmerged commit count so a
// human reviewing the skip warning knows what to look at.
func TestCloseTimeInvariantSkipReason_RefusedWhenNoneHold(t *testing.T) {
	t.Parallel()
	counter := fakeCloseTimeCommitCounter{count: 5}

	got := closeTimeInvariantSkipReason(counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
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

// TestCloseTimeInvariantSkipReason_CommitsAheadErrorFailsOpen documents the
// deliberate fail-open behavior when branch state itself can't be
// determined (e.g. git error): the check declines to block an otherwise
// unrelated close on an inconclusive read, matching every other skip-reason
// helper's contract in this file.
func TestCloseTimeInvariantSkipReason_CommitsAheadErrorFailsOpen(t *testing.T) {
	t.Parallel()
	counter := fakeCloseTimeCommitCounter{err: errPlaceholder}

	got := closeTimeInvariantSkipReason(counter, "gt-6hmz", "polecat/basalt/gt-6hmz+abc", "main", "")
	if got != "" {
		t.Errorf("expected fail-open when commit-ahead count is inconclusive, got skip reason %q", got)
	}
}

// TestHasOperatorOverridePrefix pins the exact set of accepted prefixes.
func TestHasOperatorOverridePrefix(t *testing.T) {
	t.Parallel()
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
